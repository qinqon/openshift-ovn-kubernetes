package infraprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/runner"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/testcontext"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
)

// sudoRunner wraps a runner to prepend "sudo" to every command.
// This is needed on AWS because rootless podman uses slirp4netns
// for networking, which cannot properly handle strongSwan's IKE
// UDP socket behavior. Rootful podman (via sudo) uses a real
// Linux bridge with kernel-level NAT, allowing IPsec to work.
type sudoRunner struct {
	inner api.Runner
}

func newSudoRunner(inner api.Runner) api.Runner {
	return &sudoRunner{inner: inner}
}

func (r *sudoRunner) Run(command string, args ...string) (string, error) {
	sudoArgs := append([]string{command}, args...)
	return r.inner.Run("sudo", sudoArgs...)
}

const (
	// awsOnPremUser is the SSH user for the "on-prem" EC2 instance
	// acting as the external FRR ToR / iperf3 server.
	awsOnPremUser = "ec2-user"
	awsOnPremPort = "22"
	// awsPrimaryNetworkName matches the network name the kubevirt.go
	// test expects when it calls infraprovider.Get().GetNetwork("bgpnet")
	// for routed ingress mode.
	awsPrimaryNetworkName = "bgpnet"
	// awsExternalFRRContainerName is the name of the external FRR
	// container running on the "on-prem" EC2 instance.
	awsExternalFRRContainerName = "frr"
)

// awsInfra provides external container management for AWS-based
// OpenShift clusters. It follows the same pattern as baremetalInfra:
// a container engine (local or SSH-based) manages containers
// (FRR, iperf3 server, etc.) on a host acting as "on-prem".
//
// The FRR container acts as the on-prem ToR router, terminating an
// IPsec VPN tunnel to AWS Transit Gateway and peering BGP over it.
type awsInfra struct {
	engine         *container.Engine
	machineNetwork api.Network
	onPremIP       string
	infraState     *AWSInfraState
	clusterInfo    *AWSClusterInfo
	// execNodeCmd is a function to execute commands on OCP nodes
	// (typically via "oc debug node/<name> -- chroot /host").
	execNodeCmd func(nodeName string, cmd []string) (string, error)
}

// initializeAWSInfra sets up the full AWS BGP test environment:
// 1. Detects AWS platform
// 2. Reads cluster info (VPC, workers, subnets)
// 3. Creates AWS infra (Route Server, TGW, Connect, VPN)
// 4. Configures external FRR container (strongSwan + BGP + iperf3)
// 5. Creates FRRConfiguration CR for FRR-k8s ↔ Route Server eBGP
func initializeAWSInfra(config *rest.Config) (*awsInfra, error) {
	configClient, err := configclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve config client: %w", err)
	}
	infra, err := configClient.ConfigV1().Infrastructures().Get(
		context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve cluster infrastructure: %w", err)
	}

	if infra.Spec.PlatformSpec.Type != configv1.AWSPlatformType {
		return nil, nil
	}

	// Get region from infrastructure status
	region := ""
	if infra.Status.PlatformStatus != nil &&
		infra.Status.PlatformStatus.AWS != nil {
		region = infra.Status.PlatformStatus.AWS.Region
	}
	if envRegion := os.Getenv("AWS_REGION"); envRegion != "" {
		region = envRegion
	}
	if region == "" {
		return nil, fmt.Errorf("AWS region not found in infrastructure status or AWS_REGION env")
	}

	ci := &awsInfra{}

	// Determine container engine runner: SSH to remote host or local
	var engineRunner api.Runner
	sshRunner, err := awsOnPremSSHRunner()
	if err != nil {
		return nil, err
	}
	if sshRunner != nil {
		// Remote mode: SSH to on-prem EC2 instance
		if _, err := sshRunner.Run("echo", "connection test"); err != nil {
			return nil, fmt.Errorf("failed SSH connectivity check: %w", err)
		}
		engineRunner = sshRunner
	} else {
		// Local mode: run containers on the test runner machine.
		// Use sudo to get rootful podman — needed because rootless
		// podman uses slirp4netns networking which breaks strongSwan's
		// IKE UDP socket (packets never reach the physical interface).
		// Rootful podman uses a real Linux bridge with kernel NAT.
		engineRunner = newSudoRunner(runner.NewDirectRunner())
	}

	// Detect container runtime (podman or docker)
	containerRuntime := "podman"
	if rt := os.Getenv("CONTAINER_RUNTIME"); rt != "" {
		containerRuntime = rt
	}
	ci.engine = container.NewEngine(containerRuntime, engineRunner)

	// Read on-prem IP (for Customer Gateway and FRR container identity)
	ci.onPremIP, _ = readAWSOnPremIP()
	if ci.onPremIP == "" {
		// Try to detect public IP
		if out, err := engineRunner.Run("curl", "-4", "-s", "--max-time", "5", "ifconfig.me"); err == nil {
			ci.onPremIP = strings.TrimSpace(out)
		}
	}

	// --- Create bgpnet network and FRR container ---
	// These are infrastructure-level resources, created outside the
	// per-test context using the runner directly (the engine's
	// CreateNetwork/CreateExternalContainer require a test context).
	framework.Logf("Creating bgpnet network and FRR container...")

	// Use 172.29.0.0/24 to avoid collision with the OCP service
	// network (172.30.0.0/16). Traffic to 172.30.x.x gets
	// intercepted by OVN on the worker nodes, breaking the
	// north/south return path from VMs to the external container.
	bgpNetSubnetV4 := "172.29.0.0/24"
	if cidr := os.Getenv("AWS_BGP_MACHINE_NETWORK_CIDR"); cidr != "" {
		bgpNetSubnetV4 = cidr
	}
	bgpNetSubnetV6 := "fd00:172:29::/64"

	// Create dual-stack network via container runtime directly.
	// The upstream kubevirt test unconditionally adds both IPv4 and
	// IPv6 routes to the iperf container, so we need both families.
	if _, err := engineRunner.Run(containerRuntime, "network", "create",
		"--subnet", bgpNetSubnetV4,
		"--subnet", bgpNetSubnetV6,
		awsPrimaryNetworkName); err != nil {
		// Ignore if already exists
		if !strings.Contains(err.Error(), "already exists") {
			framework.Logf("Network create output/error: %v (may already exist)", err)
		}
	}

	// Get network via engine (read-only, no test context needed)
	bgpNet, err := ci.engine.GetNetwork(awsPrimaryNetworkName)
	if err != nil {
		return nil, fmt.Errorf("failed to get bgpnet network after creation: %w", err)
	}
	framework.Logf("bgpnet network ready: %s", bgpNet.Name())
	ci.machineNetwork = bgpNet

	// Create FRR container if it doesn't exist
	frr := api.ExternalContainer{Name: awsExternalFRRContainerName}
	if _, checkErr := ci.engine.ExecExternalContainerCommand(frr, []string{"hostname"}); checkErr != nil {
		framework.Logf("FRR container not found, creating...")
		if _, err := engineRunner.Run(containerRuntime, "run", "-itd", "--privileged",
			"--name", awsExternalFRRContainerName,
			"--network", awsPrimaryNetworkName,
			"--hostname", awsExternalFRRContainerName,
			"quay.io/frrouting/frr:10.5.3"); err != nil {
			return nil, fmt.Errorf("failed to create FRR container: %w", err)
		}
		framework.Logf("FRR container created")
	} else {
		framework.Logf("FRR container already exists, reusing")
	}

	// --- Step 1: Read cluster info ---
	framework.Logf("Reading AWS cluster info...")
	clusterInfo, err := readAWSClusterInfo(config, region)
	if err != nil {
		return nil, fmt.Errorf("failed to read cluster info: %w", err)
	}
	ci.clusterInfo = clusterInfo

	// Set up node command executor (oc debug)
	ci.execNodeCmd = func(nodeName string, cmd []string) (string, error) {
		ocArgs := append([]string{"debug", fmt.Sprintf("node/%s", nodeName),
			"--to-namespace=default", "--", "chroot", "/host"}, cmd...)
		ocCmd := exec.Command("oc", ocArgs...)
		out, err := ocCmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// --- Step 2: Create or reuse AWS infrastructure ---
	//
	// AWS_SKIP_INFRA_SETUP=1  → skip everything (legacy flag, manual VPN config)
	// AWS_KEEP_INFRA=1        → persist state to disk; on next run, reload and
	//                           skip creation + reaper (fast iteration loop)
	keepInfra := os.Getenv("AWS_KEEP_INFRA") != ""

	if os.Getenv("AWS_SKIP_INFRA_SETUP") != "" {
		framework.Logf("Skipping AWS infra setup (AWS_SKIP_INFRA_SETUP is set)")
		// Load VPN config from file if provided
		vpnConfigFile := os.Getenv("AWS_VPN_CONFIG_FILE")
		if vpnConfigFile != "" {
			vpnConfig, err := ParseVPNConfigFile(vpnConfigFile)
			if err != nil {
				return nil, fmt.Errorf("failed to parse VPN config: %w", err)
			}
			if asnStr := os.Getenv("AWS_BGP_LOCAL_ASN"); asnStr != "" {
				var asn int
				if _, err := fmt.Sscanf(asnStr, "%d", &asn); err == nil {
					vpnConfig.LocalASN = asn
				}
			}
			if err := ci.configureExternalFRRForAWS(vpnConfig); err != nil {
				return nil, fmt.Errorf("failed to configure FRR: %w", err)
			}
		}
	} else if keepInfra {
		// Try to reload infra state from a previous run.
		saved, loadErr := loadAWSInfraState()
		if loadErr != nil {
			framework.Logf("WARNING: failed to load saved infra state: %v — will create fresh", loadErr)
		}
		if saved != nil && saved.TransitGatewayID != "" {
			framework.Logf("Reusing saved AWS infrastructure (TGW=%s, RS=%s, VPN=%s)",
				saved.TransitGatewayID, saved.RouteServerID, saved.VPNConnectionID)
			ci.infraState = saved

			// Re-download VPN config and reconfigure FRR container
			framework.Logf("Re-downloading VPN configuration...")
			vpnXML, err := downloadVPNConfig(saved.VPNConnectionID, region)
			if err != nil {
				return nil, fmt.Errorf("failed to download VPN config on reuse: %w", err)
			}
			vpnConfig, err := ParseVPNConfigXML(vpnXML)
			if err != nil {
				return nil, fmt.Errorf("failed to parse VPN config on reuse: %w", err)
			}
			if asnStr := os.Getenv("AWS_BGP_LOCAL_ASN"); asnStr != "" {
				var asn int
				if _, err := fmt.Sscanf(asnStr, "%d", &asn); err == nil {
					vpnConfig.LocalASN = asn
				}
			}
			if err := ci.configureExternalFRRForAWS(vpnConfig); err != nil {
				return nil, fmt.Errorf("failed to configure FRR on reuse: %w", err)
			}
		} else {
			// No saved state → create fresh (fall through to creation below)
			ci.infraState = nil
		}
	}

	// Create fresh infrastructure if we don't already have it
	if ci.infraState == nil && os.Getenv("AWS_SKIP_INFRA_SETUP") == "" {
		framework.Logf("Creating AWS BGP infrastructure...")
		if ci.onPremIP == "" {
			return nil, fmt.Errorf("on-prem public IP required for VPN setup; set AWS_ONPREM_IP or ensure curl ifconfig.me works")
		}
		infraState, err := createAWSInfrastructure(clusterInfo, ci.onPremIP)
		if err != nil {
			// Cleanup whatever was created
			if infraState != nil {
				deleteAWSInfrastructure(infraState)
			}
			return nil, fmt.Errorf("failed to create AWS infrastructure: %w", err)
		}
		ci.infraState = infraState

		// Persist state for reuse if AWS_KEEP_INFRA is set
		if keepInfra {
			if saveErr := saveAWSInfraState(infraState); saveErr != nil {
				framework.Logf("WARNING: failed to save infra state: %v", saveErr)
			}
		}

		// --- Step 3: Download VPN config and configure FRR container ---
		framework.Logf("Downloading VPN configuration...")
		vpnXML, err := downloadVPNConfig(infraState.VPNConnectionID, region)
		if err != nil {
			deleteAWSInfrastructure(infraState)
			return nil, fmt.Errorf("failed to download VPN config: %w", err)
		}

		vpnConfig, err := ParseVPNConfigXML(vpnXML)
		if err != nil {
			deleteAWSInfrastructure(infraState)
			return nil, fmt.Errorf("failed to parse VPN config: %w", err)
		}

		// Allow overriding the local ASN
		if asnStr := os.Getenv("AWS_BGP_LOCAL_ASN"); asnStr != "" {
			var asn int
			if _, err := fmt.Sscanf(asnStr, "%d", &asn); err == nil {
				vpnConfig.LocalASN = asn
			}
		}

		if err := ci.configureExternalFRRForAWS(vpnConfig); err != nil {
			deleteAWSInfrastructure(infraState)
			return nil, fmt.Errorf("failed to configure FRR: %w", err)
		}
	}

	// --- Step 5: Create per-AZ FRRConfiguration CRs for FRR-k8s ---
	// The upstream kubevirt test creates a RouteAdvertisements CR that
	// requires at least one FRRConfiguration to be present in the cluster.
	// Each FRRConfiguration tells the FRR-k8s pods in a specific AZ to
	// peer with the RS endpoint in their own subnet (directly connected,
	// no eBGP multihop needed). This per-AZ model aligns with the
	// rosa-bgp-operator architecture.
	if ci.infraState != nil && len(ci.infraState.RouteServerEndpointIPs) > 0 {
		framework.Logf("Creating per-AZ FRRConfiguration CRs for FRR-k8s...")
		if err := createPerAZFRRConfigurations(clusterInfo, ci.infraState.RouteServerEndpointIPs); err != nil {
			framework.Logf("WARNING: failed to create FRRConfiguration CRs: %v", err)
		}
	}

	return ci, nil
}

// createPerAZFRRConfigurations creates one FRRConfiguration CR per AZ in the
// openshift-frr-k8s namespace. Each configuration tells FRR-k8s worker pods
// in that AZ to peer with the Route Server endpoint in their own subnet.
// Since the RS endpoint is directly connected (same subnet as the workers),
// eBGP multihop is not needed.
func createPerAZFRRConfigurations(clusterInfo *AWSClusterInfo, endpointIPs map[string]string) error {
	// Group workers by AZ to determine unique AZs and their subnet→IP mapping
	type azInfo struct {
		az         string
		endpointIP string
	}
	seenAZs := make(map[string]azInfo)
	for _, worker := range clusterInfo.Workers {
		if _, seen := seenAZs[worker.AZ]; seen {
			continue
		}
		ip, ok := endpointIPs[worker.SubnetID]
		if !ok {
			continue
		}
		seenAZs[worker.AZ] = azInfo{az: worker.AZ, endpointIP: ip}
	}

	for _, info := range seenAZs {
		name := fmt.Sprintf("aws-bgp-rs-%s", info.az)
		yaml := fmt.Sprintf(`apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: %s
  namespace: openshift-frr-k8s
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""
      topology.kubernetes.io/zone: %s
  bgp:
    routers:
    - asn: 65001
      neighbors:
      - address: %s
        asn: 64512
        disableMP: true
        toReceive:
          allowed:
            mode: all
`, name, info.az, info.endpointIP)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(yaml)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("kubectl apply FRRConfiguration %s failed: %w\noutput: %s", name, err, string(out))
		}
		framework.Logf("  FRRConfiguration %s created (AZ %s, RS endpoint %s): %s",
			name, info.az, info.endpointIP, strings.TrimSpace(string(out)))
	}
	return nil
}

func (ci *awsInfra) GetNetwork(name string) (api.Network, error) {
	if name == "kind" || name == awsPrimaryNetworkName {
		framework.Logf("overriding network %q with AWS primary network %s",
			name, awsPrimaryNetworkName)
		return ci.machineNetwork, nil
	}
	return ci.engine.GetNetwork(name)
}

func (ci *awsInfra) ExecExternalContainerCommand(
	container api.ExternalContainer, cmd []string) (string, error) {
	return ci.engine.ExecExternalContainerCommand(container, cmd)
}

func (ci *awsInfra) ExternalContainerPrimaryInterfaceName() string {
	return ci.engine.ExternalContainerPrimaryInterfaceName()
}

func (ci *awsInfra) GetExternalContainerLogs(
	container api.ExternalContainer) (string, error) {
	return ci.engine.GetExternalContainerLogs(container)
}

func (ci *awsInfra) GetExternalContainerPort() uint16 {
	return ci.engine.GetExternalContainerPort()
}

func (ci *awsInfra) ListNetworks() ([]string, error) {
	return ci.engine.ListNetworks()
}

func (ci *awsInfra) GetExternalContainerNetworkInterface(
	container api.ExternalContainer,
	network api.Network,
) (api.NetworkInterface, error) {
	// Always use the container's actual IP on the Docker/Podman network.
	// The on-prem public IP (ci.onPremIP) is only used for the AWS
	// Customer Gateway, not for container-to-container routing.
	return ci.engine.GetNetworkInterface(container.Name, network.Name())
}

func (ci *awsInfra) GetExternalContainerContextProvider(
	context *testcontext.TestContext,
) api.ExternalContainerContextProvider {
	ciWithTestContext := &awsInfra{
		engine:         ci.engine.WithTestContext(context),
		machineNetwork: ci.machineNetwork,
		onPremIP:       ci.onPremIP,
	}
	return ciWithTestContext
}

func (ci *awsInfra) CreateExternalContainer(
	container api.ExternalContainer,
) (api.ExternalContainer, error) {
	return ci.engine.CreateExternalContainer(container)
}

func (ci *awsInfra) DeleteExternalContainer(
	container api.ExternalContainer,
) error {
	return ci.engine.DeleteExternalContainer(container)
}

func (ci *awsInfra) CreateNetwork(
	name string, subnets ...string,
) (api.Network, error) {
	return ci.engine.CreateNetwork(name, subnets...)
}

func (ci *awsInfra) AttachNetwork(
	network api.Network, container string,
) (api.NetworkInterface, error) {
	return ci.engine.AttachNetwork(network, container)
}

func (ci *awsInfra) DetachNetwork(
	network api.Network, container string,
) error {
	return ci.engine.DetachNetwork(network, container)
}

func (ci *awsInfra) DeleteNetwork(network api.Network) error {
	return ci.engine.DeleteNetwork(network)
}

// awsOnPremSSHRunner creates an SSH runner for the "on-prem" EC2
// instance. Reads the IP from AWS_ONPREM_IP env var or from
// $SHARED_DIR/aws-onprem-ip file. SSH key from
// AWS_ONPREM_SSH_KEY env var or $CLUSTER_PROFILE_DIR/ssh-privatekey.
func awsOnPremSSHRunner() (api.Runner, error) {
	ip, err := readAWSOnPremIP()
	if err != nil {
		return nil, err
	}
	if ip == "" {
		return nil, nil
	}

	sshKeyPath, err := findAWSSSHKeyPath()
	if err != nil {
		return nil, err
	}
	if sshKeyPath == "" {
		return nil, nil
	}

	user := os.Getenv("AWS_ONPREM_USER")
	if user == "" {
		user = awsOnPremUser
	}

	port := os.Getenv("AWS_ONPREM_SSH_PORT")
	if port == "" {
		port = awsOnPremPort
	}

	sshRunner, err := runner.NewSSHRunner(ip, user, port, sshKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH runner for on-prem EC2: %w", err)
	}

	return sshRunner, nil
}

// readAWSOnPremIP reads the on-prem EC2 instance IP.
// Priority: AWS_ONPREM_IP env var > $SHARED_DIR/aws-onprem-ip file.
func readAWSOnPremIP() (string, error) {
	if ip := os.Getenv("AWS_ONPREM_IP"); ip != "" {
		return strings.TrimSpace(ip), nil
	}

	sharedDir := os.Getenv("SHARED_DIR")
	if sharedDir == "" {
		return "", nil
	}

	ipFile := filepath.Join(sharedDir, "aws-onprem-ip")
	exists, err := fileExists(ipFile)
	if err != nil {
		return "", fmt.Errorf("failed to check on-prem IP file: %w", err)
	}
	if !exists {
		return "", nil
	}

	data, err := os.ReadFile(ipFile)
	if err != nil {
		return "", fmt.Errorf("failed to read on-prem IP file: %w", err)
	}

	ip := strings.TrimSpace(string(data))
	if ip == "" {
		return "", fmt.Errorf("on-prem IP file is empty")
	}

	return ip, nil
}

// findAWSSSHKeyPath locates the SSH key for the on-prem EC2 instance.
// Priority: AWS_ONPREM_SSH_KEY env var > $CLUSTER_PROFILE_DIR/ssh-privatekey
// > standard locations.
func findAWSSSHKeyPath() (string, error) {
	if keyPath := os.Getenv("AWS_ONPREM_SSH_KEY"); keyPath != "" {
		exists, err := fileExists(keyPath)
		if err != nil {
			return "", fmt.Errorf("failed to check SSH key: %w", err)
		}
		if exists {
			return keyPath, nil
		}
		return "", fmt.Errorf("SSH key not found at %s", keyPath)
	}

	clusterProfileDir := os.Getenv("CLUSTER_PROFILE_DIR")
	if clusterProfileDir != "" {
		keyPath := filepath.Join(clusterProfileDir, "ssh-privatekey")
		exists, err := fileExists(keyPath)
		if err != nil {
			return "", fmt.Errorf("failed to check SSH key: %w", err)
		}
		if exists {
			return keyPath, nil
		}
	}

	// Try default SSH key locations
	homeDir, _ := os.UserHomeDir()
	for _, name := range []string{"id_rsa", "id_ed25519"} {
		keyPath := filepath.Join(homeDir, ".ssh", name)
		exists, err := fileExists(keyPath)
		if err != nil {
			continue
		}
		if exists {
			return keyPath, nil
		}
	}

	return "", nil
}

// findAWSNodeInterface retrieves the network interface matching the
// given subnets from a remote host via SSH.
func findAWSNodeInterface(
	runner api.Runner, v4Subnet, v6Subnet string,
) (*api.NetworkInterface, error) {
	result, err := runner.Run("ip", "-j", "addr")
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve network links: %w", err)
	}

	var links []linkInfo
	if err := json.Unmarshal([]byte(result), &links); err != nil {
		return nil, fmt.Errorf("failed to parse network links: %w", err)
	}

	for _, link := range links {
		if netInfo := tryMatchLink(link, v4Subnet, v6Subnet); netInfo != nil {
			return netInfo, nil
		}
	}
	return nil, fmt.Errorf(
		"no interface found matching subnets v4=%s v6=%s", v4Subnet, v6Subnet)
}

// configureExternalFRRForAWS installs strongSwan and iperf3 inside the
// running FRR container, configures IPsec tunnels to AWS TGW, creates
// xfrm tunnel interfaces, adds BGP peers to FRR, and starts iperf3.
//
// This transforms the stock FRR container (quay.io/frrouting/frr)
// into a full "on-prem ToR" that:
// - Terminates IPsec VPN tunnels to AWS TGW (via NAT-T, works behind NAT)
// - Peers BGP with TGW over the tunnels (APIPA 169.254.x.x)
// - Learns VM routes (e.g., 10.200.0.0/24) from AWS via BGP
// - Runs iperf3 server for traffic testing
//
// The container must be running with --privileged (already the case in
// e2e tests) for xfrm/IPsec to work inside the container's netns.
// No --net=host is needed.
func (ci *awsInfra) configureExternalFRRForAWS(vpnConfig *AWSVPNConfig) error {
	frr := api.ExternalContainer{Name: awsExternalFRRContainerName}

	// Step 1: Install strongSwan + iperf3 + tcpdump
	framework.Logf("Installing strongSwan, iperf3, tcpdump in FRR container...")
	if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
		"apk", "add", "--no-cache", "strongswan", "iperf3", "tcpdump",
	}); err != nil {
		return fmt.Errorf("failed to install packages in FRR container: %w", err)
	}

	// Step 1b: Enable IP forwarding and disable reverse path filtering.
	// The FRR container acts as a router between bgpnet and the VPN
	// tunnel. Without IP forwarding, packets from the iperf container
	// won't be forwarded through the VPN. Without disabling rp_filter,
	// the kernel drops forwarded packets because the source IP
	// (e.g., 172.29.0.3 from bgpnet) doesn't match the interface the
	// packet egresses on (vti1/vti2).
	framework.Logf("Enabling IP forwarding and disabling rp_filter...")
	for _, sysctl := range []string{
		"net.ipv4.ip_forward=1",
		"net.ipv4.conf.all.rp_filter=0",
		"net.ipv4.conf.default.rp_filter=0",
	} {
		if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
			"sysctl", "-w", sysctl,
		}); err != nil {
			framework.Logf("WARNING: sysctl %s failed: %v", sysctl, err)
		}
	}

	// Step 2: Write strongSwan swanctl config
	framework.Logf("Configuring strongSwan for AWS VPN tunnels...")
	swanctlConf, err := vpnConfig.GenerateSwanctlConf()
	if err != nil {
		return fmt.Errorf("failed to generate swanctl.conf: %w", err)
	}

	// Ensure swanctl conf directory exists
	if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
		"mkdir", "-p", "/etc/swanctl/conf.d",
	}); err != nil {
		return fmt.Errorf("failed to create swanctl conf dir: %w", err)
	}

	// Write the config file
	if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
		"sh", "-c", fmt.Sprintf("cat > /etc/swanctl/conf.d/aws.conf << 'SWANEOF'\n%s\nSWANEOF", swanctlConf),
	}); err != nil {
		return fmt.Errorf("failed to write swanctl.conf: %w", err)
	}

	// Step 3: Create xfrm tunnel interfaces (delete first to handle container reuse)
	framework.Logf("Creating xfrm tunnel interfaces...")
	for _, ifName := range vpnConfig.XfrmInterfaceNames() {
		// Best-effort delete in case the container was reused from a previous run
		_, _ = ci.engine.ExecExternalContainerCommand(frr, []string{"ip", "link", "del", ifName})
	}
	for _, cmd := range vpnConfig.XfrmInterfaceCommands() {
		if _, err := ci.engine.ExecExternalContainerCommand(frr, cmd); err != nil {
			return fmt.Errorf("failed to create xfrm interface (cmd: %v): %w", cmd, err)
		}
	}

	// Step 4: Start FRR daemons (zebra + bgpd) directly
	// Kill stale daemons in case of container reuse from a previous run
	framework.Logf("Starting FRR daemons...")
	_, _ = ci.engine.ExecExternalContainerCommand(frr, []string{"ipsec", "stop"})
	_, _ = ci.engine.ExecExternalContainerCommand(frr, []string{"killall", "-q", "bgpd"})
	_, _ = ci.engine.ExecExternalContainerCommand(frr, []string{"killall", "-q", "zebra"})
	// Create minimal config files
	if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
		"sh", "-c", "touch /etc/frr/vtysh.conf && chown frr:frr /etc/frr/vtysh.conf",
	}); err != nil {
		return fmt.Errorf("failed to create vtysh.conf: %w", err)
	}
	// Start zebra (required before bgpd)
	if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
		"/usr/lib/frr/zebra", "-d",
	}); err != nil {
		return fmt.Errorf("failed to start zebra: %w", err)
	}
	// Start bgpd
	if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
		"/usr/lib/frr/bgpd", "-d",
	}); err != nil {
		return fmt.Errorf("failed to start bgpd: %w", err)
	}

	// Step 5: Start strongSwan
	framework.Logf("Starting strongSwan...")
	if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
		"ipsec", "start",
	}); err != nil {
		return fmt.Errorf("failed to start ipsec: %w", err)
	}

	// Load swanctl configuration
	if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
		"swanctl", "--load-all",
	}); err != nil {
		return fmt.Errorf("failed to load swanctl config: %w", err)
	}

	// Step 5: Add TGW BGP peers to running FRR
	framework.Logf("Adding TGW BGP peers to FRR...")
	bgpCmds := vpnConfig.GenerateFRRBGPCommands()
	vtyshArgs := []string{"vtysh"}
	for _, cmd := range bgpCmds {
		vtyshArgs = append(vtyshArgs, "-c", cmd)
	}
	if _, err := ci.engine.ExecExternalContainerCommand(frr, vtyshArgs); err != nil {
		return fmt.Errorf("failed to configure FRR BGP peers: %w", err)
	}

	// Step 5b: Advertise bgpnet subnet so return traffic from VMs
	// can be routed back through TGW → VPN → FRR → bgpnet.
	bgpNetV4Subnet, _, _ := ci.machineNetwork.IPv4IPv6Subnets()
	if bgpNetV4Subnet != "" {
		framework.Logf("Advertising bgpnet subnet %s via BGP...", bgpNetV4Subnet)
		if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
			"vtysh",
			"-c", "configure terminal",
			"-c", fmt.Sprintf("router bgp %d", vpnConfig.LocalASN),
			"-c", "address-family ipv4 unicast",
			"-c", fmt.Sprintf("network %s", bgpNetV4Subnet),
			"-c", "end",
		}); err != nil {
			framework.Logf("WARNING: failed to advertise bgpnet subnet: %v", err)
		}
	}

	// Step 6: Start iperf3 server
	framework.Logf("Starting iperf3 server in FRR container...")
	if _, err := ci.engine.ExecExternalContainerCommand(frr, []string{
		"iperf3", "-s", "-D",
	}); err != nil {
		return fmt.Errorf("failed to start iperf3 server: %w", err)
	}

	// Step 7: Verify IPsec tunnels are establishing
	framework.Logf("Verifying IPsec tunnel status...")
	output, err := ci.engine.ExecExternalContainerCommand(frr, []string{
		"ipsec", "status",
	})
	if err != nil {
		framework.Logf("WARNING: ipsec status check failed: %v", err)
	} else {
		framework.Logf("IPsec status:\n%s", output)
	}

	// Step 8: Verify BGP peer status
	framework.Logf("Verifying BGP peer status...")
	output, err = ci.engine.ExecExternalContainerCommand(frr, []string{
		"vtysh", "-c", "show ip bgp summary",
	})
	if err != nil {
		framework.Logf("WARNING: BGP summary check failed: %v", err)
	} else {
		framework.Logf("BGP summary:\n%s", output)
	}

	framework.Logf("External FRR container configured for AWS BGP testing")
	return nil
}

// Note: ipInCIDR from baremetal.go is reusable within this package.

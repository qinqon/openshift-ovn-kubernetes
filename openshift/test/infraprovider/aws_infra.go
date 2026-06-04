package infraprovider

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/kubernetes/test/e2e/framework"
)

// awsBGPTestTag is applied to every AWS resource created by this provider.
// The orphan reaper uses it to find and clean up leaked resources from
// previous crashed runs.
const awsBGPTestTagKey = "ocpstrat3267-bgp-e2e"
const awsBGPTestTagValue = "owned"

// tagSpec returns the --tag-specifications flag value for a given resource type.
func tagSpec(resourceType string) string {
	return fmt.Sprintf("ResourceType=%s,Tags=[{Key=%s,Value=%s}]",
		resourceType, awsBGPTestTagKey, awsBGPTestTagValue)
}

// AWSInfraState holds the IDs of all created AWS resources for cleanup.
// JSON tags enable persistence to disk for infra reuse across test runs.
type AWSInfraState struct {
	Region string `json:"region"`
	VPCID  string `json:"vpcID"`

	// Route Server
	RouteServerID string `json:"routeServerID"`
	// Per-AZ Route Server endpoints: map[subnetID] = endpointID
	RouteServerEndpointIDs map[string]string `json:"routeServerEndpointIDs"`
	// Per-AZ Route Server endpoint IPs: map[subnetID] = IP address
	RouteServerEndpointIPs map[string]string `json:"routeServerEndpointIPs"`
	RouteServerPeerIDs     []string          `json:"routeServerPeerIDs"`

	// Transit Gateway
	TransitGatewayID          string   `json:"transitGatewayID"`
	TGWVPCAttachmentID        string   `json:"tgwVPCAttachmentID"`
	TGWConnectAttachmentID    string   `json:"tgwConnectAttachmentID"`
	TGWConnectPeerIDs         []string `json:"tgwConnectPeerIDs"`
	TGWConnectPeerInsideCIDRs []string `json:"tgwConnectPeerInsideCIDRs"` // 169.254.x.x/29 for each peer
	TGWConnectPeerGREAddrs    []string `json:"tgwConnectPeerGREAddrs"`    // 192.0.2.x for each peer

	// VPN
	CustomerGatewayID string `json:"customerGatewayID"`
	VPNConnectionID   string `json:"vpnConnectionID"`

	// Modifications to existing resources (for rollback)
	ModifiedSecurityGroupID string   `json:"modifiedSecurityGroupID"`
	ModifiedRouteTableIDs   []string `json:"modifiedRouteTableIDs"`
	DisabledSrcDstInstances []string `json:"disabledSrcDstInstances"`
}

// awsInfraStateFile returns the path for persisting AWSInfraState.
func awsInfraStateFile() string {
	if f := os.Getenv("AWS_INFRA_STATE_FILE"); f != "" {
		return f
	}
	return "aws-infra-state.json"
}

// saveAWSInfraState writes AWSInfraState to disk as JSON.
func saveAWSInfraState(state *AWSInfraState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal infra state: %w", err)
	}
	path := awsInfraStateFile()
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write infra state to %s: %w", path, err)
	}
	framework.Logf("Saved AWS infra state to %s", path)
	return nil
}

// loadAWSInfraState reads a previously-saved AWSInfraState from disk.
// Returns nil, nil when the file does not exist.
func loadAWSInfraState() (*AWSInfraState, error) {
	path := awsInfraStateFile()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read infra state from %s: %w", path, err)
	}
	var state AWSInfraState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal infra state from %s: %w", path, err)
	}
	framework.Logf("Loaded AWS infra state from %s (TGW=%s, RS=%s, VPN=%s)",
		path, state.TransitGatewayID, state.RouteServerID, state.VPNConnectionID)
	return &state, nil
}

// createAWSInfrastructure creates all AWS resources needed for the
// OCPSTRAT-3267 BGP test: Route Server, Transit Gateway, TGW Connect,
// Site-to-Site VPN, security groups, and src/dst check disabling.
//
// All resources are created via AWS CLI (exec.Command).
// Resource IDs are stored in AWSInfraState for cleanup.
func createAWSInfrastructure(clusterInfo *AWSClusterInfo, publicIP string) (*AWSInfraState, error) {
	region := clusterInfo.Region
	state := &AWSInfraState{Region: region, VPCID: clusterInfo.VPCID}

	// Reap orphaned resources from previous crashed runs before creating new ones.
	reapOrphanedAWSInfra(region, clusterInfo.VPCID)

	// ============================================================
	// 1. Security Group Rules
	// ============================================================
	framework.Logf("Configuring security groups...")
	if clusterInfo.WorkerSecurityGroupID != "" {
		state.ModifiedSecurityGroupID = clusterInfo.WorkerSecurityGroupID
		// Allow GRE (protocol 47), BGP (TCP 179), ICMP, iperf3 (TCP 5201),
		// and all TCP from bgpnet. The bgpnet rule is needed because AWS
		// Security Groups do not track connections with non-ENI source IPs
		// as stateful — even though the outbound SYN from the VM (source
		// IP = UDN address, not the ENI address) is allowed by the egress
		// rule, the return SYN-ACK from bgpnet is dropped without an
		// explicit ingress rule.
		bgpNetCIDR := os.Getenv("AWS_BGP_MACHINE_NETWORK_CIDR")
		if bgpNetCIDR == "" {
			bgpNetCIDR = "172.29.0.0/24"
		}
		for _, rule := range [][]string{
			{"--ip-permissions", `IpProtocol=47,IpRanges=[{CidrIp=0.0.0.0/0}]`},
			{"--ip-permissions", `IpProtocol=tcp,FromPort=179,ToPort=179,IpRanges=[{CidrIp=0.0.0.0/0}]`},
			{"--ip-permissions", `IpProtocol=icmp,FromPort=-1,ToPort=-1,IpRanges=[{CidrIp=0.0.0.0/0}]`},
			{"--ip-permissions", `IpProtocol=tcp,FromPort=5201,ToPort=5201,IpRanges=[{CidrIp=0.0.0.0/0}]`},
			{"--ip-permissions", fmt.Sprintf(`IpProtocol=tcp,FromPort=0,ToPort=65535,IpRanges=[{CidrIp=%s}]`, bgpNetCIDR)},
		} {
			args := append([]string{"ec2", "authorize-security-group-ingress",
				"--group-id", clusterInfo.WorkerSecurityGroupID}, rule...)
			if _, err := awsCLIText(region, args...); err != nil {
				// Ignore duplicate rule errors
				if !strings.Contains(err.Error(), "InvalidPermission.Duplicate") {
					framework.Logf("WARNING: failed to add SG rule: %v", err)
				}
			}
		}
	}

	// ============================================================
	// 2. Disable src/dst check on workers
	// ============================================================
	framework.Logf("Disabling src/dst checks on worker instances...")
	for _, worker := range clusterInfo.Workers {
		if _, err := awsCLIText(region,
			"ec2", "modify-instance-attribute",
			"--instance-id", worker.InstanceID,
			"--no-source-dest-check",
		); err != nil {
			return state, fmt.Errorf("failed to disable src/dst check on %s: %w",
				worker.InstanceID, err)
		}
		state.DisabledSrcDstInstances = append(state.DisabledSrcDstInstances, worker.InstanceID)
	}

	// ============================================================
	// 3. Route Server
	// ============================================================
	framework.Logf("Creating Route Server...")

	// Create Route Server
	rsID, err := awsCLIText(region,
		"ec2", "create-route-server",
		"--amazon-side-asn", "64512",
		"--tag-specifications", tagSpec("route-server"),
		"--query", "RouteServer.RouteServerId",
	)
	if err != nil {
		return state, fmt.Errorf("failed to create Route Server: %w", err)
	}
	state.RouteServerID = rsID
	framework.Logf("  Route Server: %s", rsID)

	// Associate Route Server with VPC
	if _, err := awsCLIText(region,
		"ec2", "associate-route-server",
		"--route-server-id", rsID,
		"--vpc-id", clusterInfo.VPCID,
	); err != nil {
		return state, fmt.Errorf("failed to associate Route Server with VPC: %w", err)
	}
	framework.Logf("  Route Server associated with VPC %s", clusterInfo.VPCID)

	// Enable Route Server route propagation to all private route tables.
	// Without this, the RS learns UDN routes from FRR-k8s via BGP but
	// never installs them in the VPC route tables — so VPC traffic
	// to UDN pod IPs has no route and gets dropped.
	framework.Logf("  Enabling Route Server route propagation...")
	for _, rtID := range clusterInfo.PrivateRouteTableIDs {
		if _, err := awsCLIText(region,
			"ec2", "enable-route-server-propagation",
			"--route-server-id", rsID,
			"--route-table-id", rtID,
		); err != nil {
			framework.Logf("WARNING: failed to enable RS propagation on %s: %v", rtID, err)
		}
	}

	// Create one Route Server endpoint per unique worker subnet (per-AZ).
	// Placing endpoints in the worker subnets means workers peer with a
	// directly-connected RS endpoint, eliminating the need for eBGP
	// multihop. This aligns with the rosa-bgp-operator's per-AZ model.
	state.RouteServerEndpointIDs = make(map[string]string)
	state.RouteServerEndpointIPs = make(map[string]string)
	seenSubnets := make(map[string]bool)
	for _, worker := range clusterInfo.Workers {
		if seenSubnets[worker.SubnetID] {
			continue
		}
		seenSubnets[worker.SubnetID] = true

		rsEndpointID, err := awsCLIText(region,
			"ec2", "create-route-server-endpoint",
			"--route-server-id", rsID,
			"--subnet-id", worker.SubnetID,
			"--query", "RouteServerEndpoint.RouteServerEndpointId",
		)
		if err != nil {
			return state, fmt.Errorf("failed to create RS endpoint in subnet %s: %w", worker.SubnetID, err)
		}
		state.RouteServerEndpointIDs[worker.SubnetID] = rsEndpointID
		// RS endpoint doesn't support tag-on-create; tag after
		awsCLIText(region, "ec2", "create-tags",
			"--resources", rsEndpointID,
			"--tags", fmt.Sprintf("Key=%s,Value=%s", awsBGPTestTagKey, awsBGPTestTagValue))
		framework.Logf("  RS endpoint %s in subnet %s (AZ %s) — waiting...", rsEndpointID, worker.SubnetID, worker.AZ)
	}

	// Wait for all endpoints to become available and get their IPs
	for subnetID, endpointID := range state.RouteServerEndpointIDs {
		if err := waitForRouteServerEndpoint(endpointID, region); err != nil {
			return state, fmt.Errorf("RS endpoint %s not available: %w", endpointID, err)
		}
		ip, err := awsCLIText(region,
			"ec2", "describe-route-server-endpoints",
			"--route-server-endpoint-ids", endpointID,
			"--query", "RouteServerEndpoints[0].EniAddress",
		)
		if err != nil {
			return state, fmt.Errorf("failed to get RS endpoint IP for %s: %w", endpointID, err)
		}
		state.RouteServerEndpointIPs[subnetID] = ip
		framework.Logf("  RS endpoint %s IP: %s", endpointID, ip)
	}

	// Register workers as Route Server peers — each worker peers with the
	// RS endpoint in its own subnet (directly connected, no eBGP multihop).
	for _, worker := range clusterInfo.Workers {
		endpointID := state.RouteServerEndpointIDs[worker.SubnetID]
		peerID, err := awsCLIText(region,
			"ec2", "create-route-server-peer",
			"--route-server-endpoint-id", endpointID,
			"--peer-address", worker.PrivateIP,
			"--bgp-options", "PeerAsn=65001",
			"--query", "RouteServerPeer.RouteServerPeerId",
		)
		if err != nil {
			return state, fmt.Errorf("failed to create RS peer for %s: %w",
				worker.NodeName, err)
		}
		// RS peer doesn't support tag-on-create; tag after
		awsCLIText(region, "ec2", "create-tags",
			"--resources", peerID,
			"--tags", fmt.Sprintf("Key=%s,Value=%s", awsBGPTestTagKey, awsBGPTestTagValue))
		state.RouteServerPeerIDs = append(state.RouteServerPeerIDs, peerID)
		framework.Logf("  RS peer: %s -> %s (%s) on endpoint %s", peerID, worker.PrivateIP, worker.NodeName, endpointID)
	}

	// ============================================================
	// 4. Transit Gateway
	// ============================================================
	framework.Logf("Creating Transit Gateway...")
	tgwID, err := awsCLIText(region,
		"ec2", "create-transit-gateway",
		"--options", "AmazonSideAsn=64512,DefaultRouteTableAssociation=enable,DefaultRouteTablePropagation=enable,TransitGatewayCidrBlocks=192.0.2.0/24",
		"--tag-specifications", tagSpec("transit-gateway"),
		"--query", "TransitGateway.TransitGatewayId",
	)
	if err != nil {
		return state, fmt.Errorf("failed to create Transit Gateway: %w", err)
	}
	state.TransitGatewayID = tgwID
	framework.Logf("  Transit Gateway: %s (waiting for availability...)", tgwID)

	if err := waitForTransitGateway(tgwID, region); err != nil {
		return state, fmt.Errorf("Transit Gateway not available: %w", err)
	}

	// ============================================================
	// 5. TGW VPC Attachment
	// ============================================================
	framework.Logf("Creating TGW VPC attachment...")
	tgwVPCAttArgs := []string{
		"ec2", "create-transit-gateway-vpc-attachment",
		"--transit-gateway-id", tgwID,
		"--vpc-id", clusterInfo.VPCID,
		"--subnet-ids",
	}
	tgwVPCAttArgs = append(tgwVPCAttArgs, clusterInfo.PrivateSubnetIDs...)
	tgwVPCAttArgs = append(tgwVPCAttArgs,
		"--tag-specifications", tagSpec("transit-gateway-attachment"),
		"--query", "TransitGatewayVpcAttachment.TransitGatewayAttachmentId")
	tgwVPCAttID, err := awsCLIText(region, tgwVPCAttArgs...)
	if err != nil {
		return state, fmt.Errorf("failed to create TGW VPC attachment: %w", err)
	}
	state.TGWVPCAttachmentID = tgwVPCAttID
	framework.Logf("  TGW VPC attachment: %s (waiting...)", tgwVPCAttID)

	if err := waitForTGWAttachment(tgwVPCAttID, region); err != nil {
		return state, fmt.Errorf("TGW VPC attachment not available: %w", err)
	}

	// ============================================================
	// 6. TGW Connect Attachment
	// ============================================================
	framework.Logf("Creating TGW Connect attachment...")
	tgwConnectAttID, err := awsCLIText(region,
		"ec2", "create-transit-gateway-connect",
		"--transport-transit-gateway-attachment-id", tgwVPCAttID,
		"--options", "Protocol=gre",
		"--tag-specifications", tagSpec("transit-gateway-attachment"),
		"--query", "TransitGatewayConnect.TransitGatewayAttachmentId",
	)
	if err != nil {
		return state, fmt.Errorf("failed to create TGW Connect attachment: %w", err)
	}
	state.TGWConnectAttachmentID = tgwConnectAttID
	framework.Logf("  TGW Connect attachment: %s (waiting...)", tgwConnectAttID)

	if err := waitForTGWAttachment(tgwConnectAttID, region); err != nil {
		return state, fmt.Errorf("TGW Connect attachment not available: %w", err)
	}

	// ============================================================
	// 7. TGW Connect Peers (one per worker)
	// ============================================================
	framework.Logf("Creating TGW Connect peers...")
	for i, worker := range clusterInfo.Workers {
		greAddr := fmt.Sprintf("192.0.2.%d", i+1)
		insideCIDR := fmt.Sprintf("169.254.%d.0/29", 10+i)

		peerID, err := awsCLIText(region,
			"ec2", "create-transit-gateway-connect-peer",
			"--transit-gateway-attachment-id", tgwConnectAttID,
			"--peer-address", worker.PrivateIP,
			"--transit-gateway-address", greAddr,
			"--inside-cidr-blocks", insideCIDR,
			"--bgp-options", "PeerAsn=65001",
			"--tag-specifications", tagSpec("transit-gateway-connect-peer"),
			"--query", "TransitGatewayConnectPeer.TransitGatewayConnectPeerId",
		)
		if err != nil {
			return state, fmt.Errorf("failed to create TGW Connect peer for %s: %w",
				worker.NodeName, err)
		}
		state.TGWConnectPeerIDs = append(state.TGWConnectPeerIDs, peerID)
		state.TGWConnectPeerGREAddrs = append(state.TGWConnectPeerGREAddrs, greAddr)
		state.TGWConnectPeerInsideCIDRs = append(state.TGWConnectPeerInsideCIDRs, insideCIDR)
		framework.Logf("  TGW Connect peer: %s -> %s (GRE %s, inside %s)",
			peerID, worker.PrivateIP, greAddr, insideCIDR)
	}

	// ============================================================
	// 8. VPC Route Table: TGW CIDR -> TGW
	// ============================================================
	framework.Logf("Adding TGW CIDR route to VPC route tables...")
	// bgpNetSubnetV4 is the on-prem bgpnet subnet. We add a route
	// so that return traffic from VMs to the external iperf container
	// goes back through the TGW → VPN → on-prem FRR → bgpnet.
	bgpNetSubnetV4 := os.Getenv("AWS_BGP_MACHINE_NETWORK_CIDR")
	if bgpNetSubnetV4 == "" {
		bgpNetSubnetV4 = "172.29.0.0/24"
	}
	for _, rtID := range clusterInfo.PrivateRouteTableIDs {
		for _, cidr := range []string{"192.0.2.0/24", bgpNetSubnetV4} {
			if _, err := awsCLIText(region,
				"ec2", "create-route",
				"--route-table-id", rtID,
				"--destination-cidr-block", cidr,
				"--transit-gateway-id", tgwID,
			); err != nil {
				if !strings.Contains(err.Error(), "RouteAlreadyExists") {
					framework.Logf("WARNING: failed to add route %s to %s: %v", cidr, rtID, err)
				}
			}
		}
		state.ModifiedRouteTableIDs = append(state.ModifiedRouteTableIDs, rtID)
	}

	// ============================================================
	// 8b. TGW Route Table: 10.0.0.0/8 -> VPC attachment
	// ============================================================
	// UDN subnets are randomly generated within 10.0.0.0/8 (by
	// randomCUDNSubnets) but are typically outside the VPC CIDR
	// (10.0.0.0/16). Without this static route, traffic from
	// on-prem to UDN IPs would be dropped at the TGW because
	// only 10.0.0.0/16 is propagated from the VPC attachment.
	framework.Logf("Adding UDN catch-all route to TGW route table...")
	tgwRTB, err := awsCLIText(region,
		"ec2", "describe-transit-gateways",
		"--transit-gateway-ids", tgwID,
		"--query", "TransitGateways[0].Options.AssociationDefaultRouteTableId",
	)
	if err != nil {
		framework.Logf("WARNING: failed to get TGW route table: %v", err)
	} else {
		if _, err := awsCLIText(region,
			"ec2", "create-transit-gateway-route",
			"--transit-gateway-route-table-id", tgwRTB,
			"--destination-cidr-block", "10.0.0.0/8",
			"--transit-gateway-attachment-id", tgwVPCAttID,
		); err != nil {
			if !strings.Contains(err.Error(), "RouteAlreadyExists") {
				framework.Logf("WARNING: failed to add 10.0.0.0/8 route to TGW: %v", err)
			}
		} else {
			framework.Logf("  TGW route: 10.0.0.0/8 -> %s (VPC attachment)", tgwVPCAttID)
		}
	}

	// ============================================================
	// 9. Customer Gateway + Site-to-Site VPN
	// ============================================================
	framework.Logf("Creating Customer Gateway and VPN connection...")
	cgwID, err := awsCLIText(region,
		"ec2", "create-customer-gateway",
		"--type", "ipsec.1",
		"--public-ip", publicIP,
		"--bgp-asn", "65000",
		"--tag-specifications", tagSpec("customer-gateway"),
		"--query", "CustomerGateway.CustomerGatewayId",
	)
	if err != nil {
		return state, fmt.Errorf("failed to create Customer Gateway: %w", err)
	}
	state.CustomerGatewayID = cgwID
	framework.Logf("  Customer Gateway: %s (public IP: %s)", cgwID, publicIP)

	vpnID, err := awsCLIText(region,
		"ec2", "create-vpn-connection",
		"--type", "ipsec.1",
		"--transit-gateway-id", tgwID,
		"--customer-gateway-id", cgwID,
		"--options", `{"StaticRoutesOnly":false}`,
		"--tag-specifications", tagSpec("vpn-connection"),
		"--query", "VpnConnection.VpnConnectionId",
	)
	if err != nil {
		return state, fmt.Errorf("failed to create VPN connection: %w", err)
	}
	state.VPNConnectionID = vpnID
	framework.Logf("  VPN connection: %s (waiting for availability...)", vpnID)

	if err := waitForVPN(vpnID, region); err != nil {
		return state, fmt.Errorf("VPN connection not available: %w", err)
	}

	framework.Logf("AWS infrastructure created successfully")
	return state, nil
}

// downloadVPNConfig downloads the VPN configuration XML from AWS.
func downloadVPNConfig(vpnID, region string) ([]byte, error) {
	out, err := awsCLIText(region,
		"ec2", "describe-vpn-connections",
		"--vpn-connection-ids", vpnID,
		"--query", "VpnConnections[0].CustomerGatewayConfiguration",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to download VPN config: %w", err)
	}
	// The output is the raw XML string (text format)
	return []byte(out), nil
}

// deleteAWSInfrastructure deletes all AWS resources in dependency order,
// polling for deletion completion at each step to prevent IncorrectState
// errors and resource leaks.
func deleteAWSInfrastructure(state *AWSInfraState) error {
	region := state.Region
	var errs []string

	framework.Logf("Cleaning up AWS infrastructure...")

	// Delete VPN connection (async — VPN TGW attachment auto-deletes)
	if state.VPNConnectionID != "" {
		framework.Logf("  Deleting VPN connection %s...", state.VPNConnectionID)
		if _, err := awsCLIText(region, "ec2", "delete-vpn-connection",
			"--vpn-connection-id", state.VPNConnectionID); err != nil {
			errs = append(errs, fmt.Sprintf("delete VPN: %v", err))
		}
	}

	// Remove TGW CIDR routes from route tables (idempotent, no wait needed)
	for _, rtID := range state.ModifiedRouteTableIDs {
		framework.Logf("  Removing TGW route from %s...", rtID)
		awsCLIText(region, "ec2", "delete-route",
			"--route-table-id", rtID,
			"--destination-cidr-block", "192.0.2.0/24")
	}

	// Delete TGW Connect peers — must finish before Connect attachment delete
	for _, peerID := range state.TGWConnectPeerIDs {
		framework.Logf("  Deleting TGW Connect peer %s...", peerID)
		if _, err := awsCLIText(region, "ec2", "delete-transit-gateway-connect-peer",
			"--transit-gateway-connect-peer-id", peerID); err != nil {
			if !strings.Contains(err.Error(), "InvalidTransitGatewayConnectPeerId") {
				errs = append(errs, fmt.Sprintf("delete TGW Connect peer %s: %v", peerID, err))
			}
		}
	}
	// Wait for all connect peers to fully delete
	for _, peerID := range state.TGWConnectPeerIDs {
		if err := waitForTGWConnectPeerDeleted(peerID, region); err != nil {
			framework.Logf("  WARNING: %v (continuing)", err)
		}
	}

	// Delete TGW Connect attachment — must finish before VPC attachment delete
	if state.TGWConnectAttachmentID != "" {
		framework.Logf("  Deleting TGW Connect attachment %s...", state.TGWConnectAttachmentID)
		if _, err := awsCLIText(region, "ec2", "delete-transit-gateway-connect",
			"--transit-gateway-attachment-id", state.TGWConnectAttachmentID); err != nil {
			if !strings.Contains(err.Error(), "InvalidTransitGatewayAttachmentId") {
				errs = append(errs, fmt.Sprintf("delete TGW Connect: %v", err))
			}
		}
		if err := waitForTGWAttachmentDeleted(state.TGWConnectAttachmentID, region); err != nil {
			framework.Logf("  WARNING: %v (continuing)", err)
		}
	}

	// Delete TGW VPC attachment — must finish before TGW delete
	if state.TGWVPCAttachmentID != "" {
		framework.Logf("  Deleting TGW VPC attachment %s...", state.TGWVPCAttachmentID)
		if _, err := awsCLIText(region, "ec2", "delete-transit-gateway-vpc-attachment",
			"--transit-gateway-attachment-id", state.TGWVPCAttachmentID); err != nil {
			if !strings.Contains(err.Error(), "InvalidTransitGatewayAttachmentId") {
				errs = append(errs, fmt.Sprintf("delete TGW VPC att: %v", err))
			}
		}
		if err := waitForTGWAttachmentDeleted(state.TGWVPCAttachmentID, region); err != nil {
			framework.Logf("  WARNING: %v (continuing)", err)
		}
	}

	// Wait for VPN deletion (and its auto-deleted TGW attachment) before TGW delete
	if state.VPNConnectionID != "" {
		if err := waitForVPNDeleted(state.VPNConnectionID, region); err != nil {
			framework.Logf("  WARNING: %v (continuing)", err)
		}
	}

	// Also wait until TGW has no non-deleted attachments (catches any we missed)
	if state.TransitGatewayID != "" {
		waitForAllTGWAttachmentsDeleted(state.TransitGatewayID, region)
	}

	// Delete Transit Gateway
	if state.TransitGatewayID != "" {
		framework.Logf("  Deleting Transit Gateway %s...", state.TransitGatewayID)
		if _, err := awsCLIText(region, "ec2", "delete-transit-gateway",
			"--transit-gateway-id", state.TransitGatewayID); err != nil {
			if !strings.Contains(err.Error(), "InvalidTransitGatewayID") {
				errs = append(errs, fmt.Sprintf("delete TGW: %v", err))
			}
		}
	}

	// Delete Customer Gateway (must wait for VPN to be fully deleted first)
	if state.CustomerGatewayID != "" {
		framework.Logf("  Deleting Customer Gateway %s...", state.CustomerGatewayID)
		if _, err := awsCLIText(region, "ec2", "delete-customer-gateway",
			"--customer-gateway-id", state.CustomerGatewayID); err != nil {
			if !strings.Contains(err.Error(), "InvalidCustomerGatewayID") {
				errs = append(errs, fmt.Sprintf("delete CGW: %v", err))
			}
		}
	}

	// Delete Route Server peers — must finish before endpoint delete
	for _, peerID := range state.RouteServerPeerIDs {
		framework.Logf("  Deleting Route Server peer %s...", peerID)
		if _, err := awsCLIText(region, "ec2", "delete-route-server-peer",
			"--route-server-peer-id", peerID); err != nil {
			if !strings.Contains(err.Error(), "InvalidRouteServerPeerId") {
				errs = append(errs, fmt.Sprintf("delete RS peer %s: %v", peerID, err))
			}
		}
	}
	for _, peerID := range state.RouteServerPeerIDs {
		if err := waitForRSPeerDeleted(peerID, region); err != nil {
			framework.Logf("  WARNING: %v (continuing)", err)
		}
	}

	// Delete Route Server endpoints — must finish before RS disassociate/delete
	for subnetID, endpointID := range state.RouteServerEndpointIDs {
		framework.Logf("  Deleting RS endpoint %s (subnet %s)...", endpointID, subnetID)
		if _, err := awsCLIText(region, "ec2", "delete-route-server-endpoint",
			"--route-server-endpoint-id", endpointID); err != nil {
			if !strings.Contains(err.Error(), "InvalidRouteServerEndpointId") {
				errs = append(errs, fmt.Sprintf("delete RS endpoint %s: %v", endpointID, err))
			}
		}
	}
	for _, endpointID := range state.RouteServerEndpointIDs {
		if err := waitForRSEndpointDeleted(endpointID, region); err != nil {
			framework.Logf("  WARNING: %v (continuing)", err)
		}
	}

	// Disassociate Route Server from VPC
	if state.RouteServerID != "" && state.VPCID != "" {
		framework.Logf("  Disassociating Route Server from VPC...")
		awsCLIText(region, "ec2", "disassociate-route-server",
			"--route-server-id", state.RouteServerID,
			"--vpc-id", state.VPCID)
		// Brief wait for disassociation to propagate
		time.Sleep(10 * time.Second)
	}

	// Delete Route Server
	if state.RouteServerID != "" {
		framework.Logf("  Deleting Route Server %s...", state.RouteServerID)
		if _, err := awsCLIText(region, "ec2", "delete-route-server",
			"--route-server-id", state.RouteServerID); err != nil {
			if !strings.Contains(err.Error(), "InvalidRouteServerId") {
				errs = append(errs, fmt.Sprintf("delete RS: %v", err))
			}
		}
	}

	// Re-enable src/dst check on workers
	for _, instanceID := range state.DisabledSrcDstInstances {
		framework.Logf("  Re-enabling src/dst check on %s...", instanceID)
		awsCLIText(region, "ec2", "modify-instance-attribute",
			"--instance-id", instanceID,
			"--source-dest-check")
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %s", strings.Join(errs, "; "))
	}

	framework.Logf("AWS infrastructure cleaned up successfully")
	return nil
}

// --- Waiters (creation — poll for "available") ---

func waitForRouteServerEndpoint(endpointID, region string) error {
	for i := 0; i < 60; i++ {
		out, err := awsCLIText(region,
			"ec2", "describe-route-server-endpoints",
			"--route-server-endpoint-ids", endpointID,
			"--query", "RouteServerEndpoints[0].State",
		)
		if err == nil && strings.TrimSpace(out) == "available" {
			return nil
		}
		framework.Logf("  Route Server endpoint state: %s (waiting...)", strings.TrimSpace(out))
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timeout waiting for Route Server endpoint %s", endpointID)
}

func waitForTransitGateway(tgwID, region string) error {
	for i := 0; i < 60; i++ {
		out, err := awsCLIText(region,
			"ec2", "describe-transit-gateways",
			"--transit-gateway-ids", tgwID,
			"--query", "TransitGateways[0].State",
		)
		if err == nil && strings.TrimSpace(out) == "available" {
			return nil
		}
		framework.Logf("  Transit Gateway state: %s (waiting...)", strings.TrimSpace(out))
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timeout waiting for Transit Gateway %s", tgwID)
}

func waitForTGWAttachment(attachmentID, region string) error {
	for i := 0; i < 60; i++ {
		out, err := awsCLIText(region,
			"ec2", "describe-transit-gateway-attachments",
			"--transit-gateway-attachment-ids", attachmentID,
			"--query", "TransitGatewayAttachments[0].State",
		)
		if err == nil && strings.TrimSpace(out) == "available" {
			return nil
		}
		framework.Logf("  TGW attachment %s state: %s (waiting...)", attachmentID, strings.TrimSpace(out))
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timeout waiting for TGW attachment %s", attachmentID)
}

func waitForVPN(vpnID, region string) error {
	for i := 0; i < 60; i++ {
		out, err := awsCLIText(region,
			"ec2", "describe-vpn-connections",
			"--vpn-connection-ids", vpnID,
			"--query", "VpnConnections[0].State",
		)
		if err == nil && strings.TrimSpace(out) == "available" {
			return nil
		}
		framework.Logf("  VPN connection state: %s (waiting...)", strings.TrimSpace(out))
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timeout waiting for VPN connection %s", vpnID)
}

// --- Waiters (deletion — poll until "deleted" or not-found) ---

func waitForTGWAttachmentDeleted(attachmentID, region string) error {
	for i := 0; i < 60; i++ {
		out, err := awsCLIText(region,
			"ec2", "describe-transit-gateway-attachments",
			"--transit-gateway-attachment-ids", attachmentID,
			"--query", "TransitGatewayAttachments[0].State",
		)
		state := strings.TrimSpace(out)
		if err != nil || state == "deleted" || state == "" || state == "None" {
			return nil
		}
		framework.Logf("  TGW attachment %s state: %s (waiting for deletion...)", attachmentID, state)
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timeout waiting for TGW attachment %s to delete", attachmentID)
}

func waitForTGWConnectPeerDeleted(peerID, region string) error {
	for i := 0; i < 60; i++ {
		out, err := awsCLIText(region,
			"ec2", "describe-transit-gateway-connect-peers",
			"--transit-gateway-connect-peer-ids", peerID,
			"--query", "TransitGatewayConnectPeers[0].State",
		)
		state := strings.TrimSpace(out)
		if err != nil || state == "deleted" || state == "" || state == "None" {
			return nil
		}
		framework.Logf("  TGW connect peer %s state: %s (waiting for deletion...)", peerID, state)
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timeout waiting for TGW connect peer %s to delete", peerID)
}

func waitForVPNDeleted(vpnID, region string) error {
	for i := 0; i < 60; i++ {
		out, err := awsCLIText(region,
			"ec2", "describe-vpn-connections",
			"--vpn-connection-ids", vpnID,
			"--query", "VpnConnections[0].State",
		)
		state := strings.TrimSpace(out)
		if err != nil || state == "deleted" || state == "" || state == "None" {
			return nil
		}
		framework.Logf("  VPN %s state: %s (waiting for deletion...)", vpnID, state)
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timeout waiting for VPN %s to delete", vpnID)
}

func waitForRSPeerDeleted(peerID, region string) error {
	for i := 0; i < 30; i++ {
		out, err := awsCLIText(region,
			"ec2", "describe-route-server-peers",
			"--route-server-peer-ids", peerID,
			"--query", "RouteServerPeers[0].State",
		)
		state := strings.TrimSpace(out)
		if err != nil || state == "deleted" || state == "" || state == "None" {
			return nil
		}
		framework.Logf("  RS peer %s state: %s (waiting for deletion...)", peerID, state)
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timeout waiting for RS peer %s to delete", peerID)
}

func waitForRSEndpointDeleted(endpointID, region string) error {
	for i := 0; i < 60; i++ {
		out, err := awsCLIText(region,
			"ec2", "describe-route-server-endpoints",
			"--route-server-endpoint-ids", endpointID,
			"--query", "RouteServerEndpoints[0].State",
		)
		state := strings.TrimSpace(out)
		if err != nil || state == "deleted" || state == "" || state == "None" {
			return nil
		}
		framework.Logf("  RS endpoint %s state: %s (waiting for deletion...)", endpointID, state)
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timeout waiting for RS endpoint %s to delete", endpointID)
}

// waitForAllTGWAttachmentsDeleted waits until a TGW has no non-deleted attachments.
func waitForAllTGWAttachmentsDeleted(tgwID, region string) {
	for i := 0; i < 90; i++ {
		out, err := awsCLIText(region,
			"ec2", "describe-transit-gateway-attachments",
			"--filters", fmt.Sprintf("Name=transit-gateway-id,Values=%s", tgwID),
			"--query", "TransitGatewayAttachments[?State!='deleted'].TransitGatewayAttachmentId",
		)
		remaining := strings.TrimSpace(out)
		if err != nil || remaining == "" || remaining == "None" {
			framework.Logf("  All TGW %s attachments deleted", tgwID)
			return
		}
		framework.Logf("  TGW %s still has attachments: %s (waiting...)", tgwID, remaining)
		time.Sleep(10 * time.Second)
	}
	framework.Logf("  WARNING: timeout waiting for TGW %s attachments to delete", tgwID)
}

// --- Orphan Reaper ---

// reapOrphanedAWSInfra finds and deletes AWS resources tagged with
// awsBGPTestTag that were leaked by previous crashed test runs.
// Only tagged resources are touched — cluster infrastructure is safe.
func reapOrphanedAWSInfra(region, vpcID string) {
	framework.Logf("Checking for orphaned AWS BGP test resources...")
	tagFilter := fmt.Sprintf("Name=tag:%s,Values=%s", awsBGPTestTagKey, awsBGPTestTagValue)

	// 1. TGW Connect peers
	peerIDs, _ := awsCLIText(region,
		"ec2", "describe-transit-gateway-connect-peers",
		"--filters", tagFilter,
		"--query", "TransitGatewayConnectPeers[?State!='deleted'].TransitGatewayConnectPeerId",
	)
	for _, id := range splitIDs(peerIDs) {
		framework.Logf("  Reaping orphan TGW connect peer %s", id)
		awsCLIText(region, "ec2", "delete-transit-gateway-connect-peer",
			"--transit-gateway-connect-peer-id", id)
	}
	for _, id := range splitIDs(peerIDs) {
		waitForTGWConnectPeerDeleted(id, region)
	}

	// 2. TGW attachments (Connect, VPC, VPN — in that order)
	for _, resType := range []string{"connect", "vpc"} {
		attIDs, _ := awsCLIText(region,
			"ec2", "describe-transit-gateway-attachments",
			"--filters", tagFilter,
			fmt.Sprintf("Name=resource-type,Values=%s", resType),
			"--query", "TransitGatewayAttachments[?State!='deleted'].TransitGatewayAttachmentId",
		)
		for _, id := range splitIDs(attIDs) {
			framework.Logf("  Reaping orphan TGW %s attachment %s", resType, id)
			if resType == "connect" {
				awsCLIText(region, "ec2", "delete-transit-gateway-connect",
					"--transit-gateway-attachment-id", id)
			} else {
				awsCLIText(region, "ec2", "delete-transit-gateway-vpc-attachment",
					"--transit-gateway-attachment-id", id)
			}
		}
		for _, id := range splitIDs(attIDs) {
			waitForTGWAttachmentDeleted(id, region)
		}
	}

	// 3. VPN connections
	vpnIDs, _ := awsCLIText(region,
		"ec2", "describe-vpn-connections",
		"--filters", tagFilter,
		"--query", "VpnConnections[?State!='deleted'].VpnConnectionId",
	)
	for _, id := range splitIDs(vpnIDs) {
		framework.Logf("  Reaping orphan VPN %s", id)
		awsCLIText(region, "ec2", "delete-vpn-connection",
			"--vpn-connection-id", id)
	}
	for _, id := range splitIDs(vpnIDs) {
		waitForVPNDeleted(id, region)
	}

	// 4. Transit Gateways — wait for all attachments gone first
	tgwIDs, _ := awsCLIText(region,
		"ec2", "describe-transit-gateways",
		"--filters", tagFilter,
		"--query", "TransitGateways[?State!='deleted'].TransitGatewayId",
	)
	for _, id := range splitIDs(tgwIDs) {
		waitForAllTGWAttachmentsDeleted(id, region)
		framework.Logf("  Reaping orphan TGW %s", id)
		awsCLIText(region, "ec2", "delete-transit-gateway",
			"--transit-gateway-id", id)
	}

	// 5. Customer Gateways
	cgwIDs, _ := awsCLIText(region,
		"ec2", "describe-customer-gateways",
		"--filters", tagFilter,
		"--query", "CustomerGateways[?State!='deleted'].CustomerGatewayId",
	)
	for _, id := range splitIDs(cgwIDs) {
		framework.Logf("  Reaping orphan CGW %s", id)
		awsCLIText(region, "ec2", "delete-customer-gateway",
			"--customer-gateway-id", id)
	}

	// 6. Route Server peers
	rsPeerIDs, _ := awsCLIText(region,
		"ec2", "describe-route-server-peers",
		"--filters", tagFilter,
		"--query", "RouteServerPeers[?State!='deleted'].RouteServerPeerId",
	)
	for _, id := range splitIDs(rsPeerIDs) {
		framework.Logf("  Reaping orphan RS peer %s", id)
		awsCLIText(region, "ec2", "delete-route-server-peer",
			"--route-server-peer-id", id)
	}
	for _, id := range splitIDs(rsPeerIDs) {
		waitForRSPeerDeleted(id, region)
	}

	// 7. Route Server endpoints
	rsEndIDs, _ := awsCLIText(region,
		"ec2", "describe-route-server-endpoints",
		"--filters", tagFilter,
		"--query", "RouteServerEndpoints[?State!='deleted'].RouteServerEndpointId",
	)
	for _, id := range splitIDs(rsEndIDs) {
		framework.Logf("  Reaping orphan RS endpoint %s", id)
		awsCLIText(region, "ec2", "delete-route-server-endpoint",
			"--route-server-endpoint-id", id)
	}
	for _, id := range splitIDs(rsEndIDs) {
		waitForRSEndpointDeleted(id, region)
	}

	// 8. Route Servers (disassociate then delete)
	rsIDs, _ := awsCLIText(region,
		"ec2", "describe-route-servers",
		"--filters", tagFilter,
		"--query", "RouteServers[?State!='deleted'].RouteServerId",
	)
	for _, id := range splitIDs(rsIDs) {
		framework.Logf("  Reaping orphan Route Server %s", id)
		awsCLIText(region, "ec2", "disassociate-route-server",
			"--route-server-id", id,
			"--vpc-id", vpcID)
		time.Sleep(10 * time.Second)
		awsCLIText(region, "ec2", "delete-route-server",
			"--route-server-id", id)
	}

	framework.Logf("Orphan reap complete")
}

// splitIDs splits AWS CLI text output (tab or newline separated IDs) into
// a slice, filtering out empty strings and "None".
func splitIDs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "None" {
		return nil
	}
	// AWS CLI text output separates with tabs (single line) or newlines
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\t' || r == '\n' || r == '\r'
	})
	var ids []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && p != "None" {
			ids = append(ids, p)
		}
	}
	return ids
}

// Ensure json import is used (for potential future use in parsing complex responses)
var _ = json.Marshal

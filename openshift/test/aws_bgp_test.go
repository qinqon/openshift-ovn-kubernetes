package test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"

	ocpdeploymentconfig "github.com/ovn-kubernetes/ovn-kubernetes/openshift/test/deploymentconfig"
	ocpinfraprovider "github.com/ovn-kubernetes/ovn-kubernetes/openshift/test/infraprovider"

	// import OVN-Kubernetes E2E tests (kubevirt, route_advertisements, etc.)
	_ "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"

	"k8s.io/kubernetes/test/e2e/framework"

	// ensure providers are initialised
	_ "k8s.io/kubernetes/test/e2e/framework/providers/aws"

	// ensure logging flags
	_ "k8s.io/component-base/logs/testinit"
)

func TestAWSBGP(t *testing.T) {
	// Set kubeconfig from env if not already set via flags
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig != "" {
		framework.TestContext.KubeConfig = kubeconfig
	}

	// Register and parse framework flags
	framework.RegisterCommonFlags(flag.CommandLine)
	framework.RegisterClusterFlags(flag.CommandLine)
	flag.Parse()
	framework.AfterReadingAllFlags(&framework.TestContext)

	// Set provider
	if os.Getenv("TEST_PROVIDER") == "" {
		os.Setenv("TEST_PROVIDER", `{"type":"aws"}`)
	}

	// Set up OpenShift infra provider for AWS
	cfg, err := framework.LoadConfig()
	if err != nil {
		t.Fatalf("Failed to load kubeconfig: %v", err)
	}

	ocpInfra, err := ocpinfraprovider.New(cfg)
	if err != nil {
		t.Fatalf("Failed to create OpenShift infra provider: %v", err)
	}
	infraprovider.Set(ocpInfra)
	deploymentconfig.Set(ocpdeploymentconfig.New())

	// Set CreateTestingNS for OpenShift (wrapper to match framework signature)
	framework.TestContext.CreateTestingNS = func(ctx context.Context, baseName string, c clientset.Interface, labels map[string]string) (*corev1.Namespace, error) {
		return CreateTestingNS(ctx, baseName, c, labels, true)
	}

	gomega.RegisterFailHandler(ginkgo.Fail)

	// Focus on the routed L2 primary UDN live migration spec.
	// The regex excludes "statics IPs and MAC" (line 2291) and "over evpn" (line 2305).
	// Override with GINKGO_FOCUS env var for a different spec.
	focus := os.Getenv("GINKGO_FOCUS")
	if focus == "" {
		focus = `should keep ip after live migration of VirtualMachine with interface binding for UDN with Primary/Layer2 ingress routed$`
	}
	// Pause on failure for forensics when AWS_PAUSE_ON_FAILURE is set.
	// Uses JustAfterEach so it runs BEFORE any DeferCleanup/AfterEach,
	// keeping the VM, pods, routes, and namespace alive for inspection.
	// The value is parsed as a Go duration (e.g. "30m", "1h"). Default 30m.
	pauseOnFailure := os.Getenv("AWS_PAUSE_ON_FAILURE")
	if pauseOnFailure != "" {
		pauseDuration := 30 * time.Minute
		if d, err := time.ParseDuration(pauseOnFailure); err == nil {
			pauseDuration = d
		}
		ginkgo.JustAfterEach(func() {
			report := ginkgo.CurrentSpecReport()
			if report.Failed() {
				fmt.Fprintf(ginkgo.GinkgoWriter,
					"\n\n*** PAUSED FOR FORENSICS (%s) — spec %q failed ***\n"+
						"    VM, pods, namespace, and AWS infra are still live.\n"+
						"    Inspect with kubectl, aws cli, podman exec, etc.\n"+
						"    Will resume teardown at %s\n\n",
					pauseDuration,
					report.FullText(),
					time.Now().Add(pauseDuration).Format(time.RFC3339))
				time.Sleep(pauseDuration)
			}
		})
	}

	suiteConfig, reporterConfig := ginkgo.GinkgoConfiguration()
	suiteConfig.FocusStrings = []string{focus}
	reporterConfig.VeryVerbose = true
	ginkgo.RunSpecs(t, "AWS BGP E2E Suite", suiteConfig, reporterConfig)
}

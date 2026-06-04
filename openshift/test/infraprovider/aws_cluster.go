package infraprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
)

// AWSClusterInfo holds information about the AWS OpenShift cluster
// needed for setting up BGP infrastructure.
type AWSClusterInfo struct {
	Region  string
	VPCID   string
	Workers []AWSWorkerInfo
	// PrivateSubnetIDs are the subnets where workers are deployed.
	PrivateSubnetIDs []string
	// PrivateRouteTableIDs are the route tables for the private subnets.
	PrivateRouteTableIDs []string
	// WorkerSecurityGroupID is the security group used by worker nodes.
	WorkerSecurityGroupID string
}

// AWSWorkerInfo holds information about a single worker node.
type AWSWorkerInfo struct {
	NodeName   string
	InstanceID string
	PrivateIP  string
	SubnetID   string
	AZ         string
}

// readAWSClusterInfo reads cluster information from the Kubernetes API
// and the AWS CLI. It extracts VPC ID, worker node details, subnets,
// and route tables needed for BGP infrastructure setup.
func readAWSClusterInfo(config *rest.Config, region string) (*AWSClusterInfo, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	info := &AWSClusterInfo{
		Region: region,
	}

	// Get worker nodes
	nodes, err := clientset.CoreV1().Nodes().List(
		context.Background(),
		metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/worker=",
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list worker nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("no worker nodes found")
	}

	for _, node := range nodes.Items {
		worker := AWSWorkerInfo{
			NodeName: node.Name,
		}

		// Extract instance ID from provider ID
		// Format: aws:///eu-north-1a/i-0abcdef1234567890
		worker.InstanceID, worker.AZ = parseProviderID(node.Spec.ProviderID)
		if worker.InstanceID == "" {
			return nil, fmt.Errorf("failed to parse provider ID for node %s: %s",
				node.Name, node.Spec.ProviderID)
		}

		// Get internal IP
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				worker.PrivateIP = addr.Address
				break
			}
		}
		if worker.PrivateIP == "" {
			return nil, fmt.Errorf("no internal IP found for node %s", node.Name)
		}

		info.Workers = append(info.Workers, worker)
	}

	// Use AWS CLI to get VPC, subnet, security group info from the first worker
	firstInstanceID := info.Workers[0].InstanceID
	instanceInfo, err := describeInstance(firstInstanceID, region)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instance %s: %w", firstInstanceID, err)
	}

	info.VPCID = instanceInfo.VPCID
	info.WorkerSecurityGroupID = instanceInfo.SecurityGroupID

	// Get subnet and route table info for each worker
	subnetSet := make(map[string]bool)
	for i := range info.Workers {
		subnetInfo, err := describeInstanceSubnet(info.Workers[i].InstanceID, region)
		if err != nil {
			return nil, fmt.Errorf("failed to get subnet for %s: %w",
				info.Workers[i].InstanceID, err)
		}
		info.Workers[i].SubnetID = subnetInfo
		subnetSet[subnetInfo] = true
	}
	for subnetID := range subnetSet {
		info.PrivateSubnetIDs = append(info.PrivateSubnetIDs, subnetID)
	}

	// Get route table IDs for private subnets
	for _, subnetID := range info.PrivateSubnetIDs {
		rtID, err := getRouteTableForSubnet(subnetID, region)
		if err != nil {
			framework.Logf("WARNING: failed to get route table for subnet %s: %v", subnetID, err)
			continue
		}
		info.PrivateRouteTableIDs = append(info.PrivateRouteTableIDs, rtID)
	}

	framework.Logf("AWS Cluster Info: region=%s vpc=%s workers=%d subnets=%v",
		info.Region, info.VPCID, len(info.Workers), info.PrivateSubnetIDs)
	for _, w := range info.Workers {
		framework.Logf("  Worker: %s instance=%s ip=%s subnet=%s az=%s",
			w.NodeName, w.InstanceID, w.PrivateIP, w.SubnetID, w.AZ)
	}

	return info, nil
}

// parseProviderID extracts the instance ID and AZ from an AWS provider ID.
// Format: aws:///eu-north-1a/i-0abcdef1234567890
func parseProviderID(providerID string) (instanceID, az string) {
	// Remove the aws:/// prefix
	trimmed := strings.TrimPrefix(providerID, "aws:///")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[1], parts[0]
}

type instanceDescribeResult struct {
	VPCID           string
	SecurityGroupID string
}

func describeInstance(instanceID, region string) (*instanceDescribeResult, error) {
	out, err := awsCLI(region,
		"ec2", "describe-instances",
		"--instance-ids", instanceID,
		"--query", "Reservations[0].Instances[0].[VpcId,SecurityGroups[?contains(GroupName,`node`)].GroupId|[0]]",
		"--output", "json",
	)
	if err != nil {
		return nil, err
	}

	var result []interface{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, fmt.Errorf("failed to parse instance info: %w", err)
	}
	if len(result) < 2 {
		return nil, fmt.Errorf("unexpected instance info format")
	}

	info := &instanceDescribeResult{}
	if v, ok := result[0].(string); ok {
		info.VPCID = v
	}
	if v, ok := result[1].(string); ok {
		info.SecurityGroupID = v
	}

	// If we didn't find a -node security group, get the first one
	if info.SecurityGroupID == "" {
		out2, err := awsCLI(region,
			"ec2", "describe-instances",
			"--instance-ids", instanceID,
			"--query", "Reservations[0].Instances[0].SecurityGroups[0].GroupId",
			"--output", "text",
		)
		if err == nil {
			info.SecurityGroupID = strings.TrimSpace(out2)
		}
	}

	return info, nil
}

func describeInstanceSubnet(instanceID, region string) (string, error) {
	out, err := awsCLI(region,
		"ec2", "describe-instances",
		"--instance-ids", instanceID,
		"--query", "Reservations[0].Instances[0].SubnetId",
		"--output", "text",
	)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func getRouteTableForSubnet(subnetID, region string) (string, error) {
	// Try explicit association first
	out, err := awsCLI(region,
		"ec2", "describe-route-tables",
		"--filters", fmt.Sprintf("Name=association.subnet-id,Values=%s", subnetID),
		"--query", "RouteTables[0].RouteTableId",
		"--output", "text",
	)
	if err == nil {
		rtID := strings.TrimSpace(out)
		if rtID != "" && rtID != "None" {
			return rtID, nil
		}
	}

	// Fall back to main route table for the VPC
	out, err = awsCLI(region,
		"ec2", "describe-route-tables",
		"--filters", fmt.Sprintf("Name=association.subnet-id,Values=%s", subnetID),
		"--query", "RouteTables[0].RouteTableId",
		"--output", "text",
	)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// awsCLI executes an AWS CLI command and returns the output.
func awsCLI(region string, args ...string) (string, error) {
	fullArgs := append([]string{"--region", region, "--output", "json"}, args...)
	cmd := exec.Command("aws", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("aws %s failed: %w\noutput: %s",
			strings.Join(args[:2], " "), err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// awsCLIText executes an AWS CLI command and returns text output.
func awsCLIText(region string, args ...string) (string, error) {
	fullArgs := append([]string{"--region", region, "--output", "text"}, args...)
	cmd := exec.Command("aws", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("aws %s failed: %w\noutput: %s",
			strings.Join(args[:2], " "), err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

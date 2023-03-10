package kubevirt

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/pointer"

	kubevirtv1 "kubevirt.io/api/core/v1"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func SyncVirtualMachines(nbClient libovsdbclient.Client, vms map[ktypes.NamespacedName]bool) error {
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(nbClient, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterStaticRoute) bool {
		return OwnsItAndIsOrphanOrWrongZone(item.ExternalIDs, vms)
	}); err != nil {
		return fmt.Errorf("failed deleting stale vm static routes: %v", err)
	}
	if err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nbClient, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterPolicy) bool {
		return OwnsItAndIsOrphanOrWrongZone(item.ExternalIDs, vms)
	}); err != nil {
		return fmt.Errorf("failed deleting stale vm policies: %v", err)
	}
	return nil
}

func DeleteRoutingForMigratedPodWithZone(nbClient libovsdbclient.Client, pod *corev1.Pod, zone *string) error {
	vm := ExtractVMNameFromPod(pod)
	predicate := func(itemExternalIDs map[string]string) bool {
		containsZone := true
		if zone != nil {
			containsZone = itemExternalIDs[OvnZoneExternalIDKey] == *zone
		}
		return containsZone && ExternalIDsContainsVM(itemExternalIDs, vm)
	}
	routePredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return predicate(item.ExternalIDs)
	}
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(nbClient, types.OVNClusterRouter, routePredicate); err != nil {
		return fmt.Errorf("failed deleting pod routing when deleting the LR static routes: %v", err)
	}
	policyPredicate := func(item *nbdb.LogicalRouterPolicy) bool {
		return predicate(item.ExternalIDs)
	}
	if err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nbClient, types.OVNClusterRouter, policyPredicate); err != nil {
		return fmt.Errorf("failed deleting pod routing when deleting the LR policies: %v", err)
	}
	return nil
}

func DeleteRoutingForMigratedPod(nbClient libovsdbclient.Client, pod *corev1.Pod) error {
	return DeleteRoutingForMigratedPodWithZone(nbClient, pod, nil)
}

// EnsureLocalZonePodAddressesToNodeRoute will add static routes and policies to ovn_cluster_route logical router
// to ensure VM traffic work as expected after live migration if the pod is running at the local/global zone.
//
// Following is the list of NB logical resources created depending if it's interconnected or not:
//
// IC:
//   - static route with cluster wide CIDR as src-ip prefix and nexthop GR, it has less
//     priority than route to use overlay in case of pod to pod communication
//
// NO IC:
//   - low priority policy with src VM ip and reroute GR, since it has low priority
//     it will not override the policy to enroute pod to pod traffic using overlay
//
// Both:
//   - static route with VM ip as dst-ip prefix and output port the LRP pointing to the VM's node switch
func EnsureLocalZonePodAddressesToNodeRoute(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client, switchNames SwitchNames, pod *corev1.Pod) error {
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
	if err != nil {
		return fmt.Errorf("failed reading ovn annotation: %v", err)
	}

	// VM is running at the node that owns the subnet the point to point
	// routing is not needed
	if switchNames.Current == switchNames.Original {
		if err := DeleteRoutingForMigratedPod(nbClient, pod); err != nil {
			return fmt.Errorf("failed configuring pod routing when deleting stale static routes or policies for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		return nil
	}
	lrpName := types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + pod.Spec.NodeName
	lrpAddresses, err := util.GetLRPAddrs(nbClient, lrpName)
	if err != nil {
		return fmt.Errorf("failed configuring pod routing when reading LRP %s addresses: %v", lrpName, err)
	}

	// For interconnect at static route with a cluster-wide src-ip address is
	// needed to route egress n/s traffic
	if config.OVNKubernetesFeature.EnableInterconnect {
		for _, clusterSubnet := range config.Default.ClusterSubnets {
			// Policy to with low priority to route traffic to the gateway
			ipFamily := utilnet.IPFamilyOfCIDR(clusterSubnet.CIDR)
			nodeGRAddress, err := util.MatchFirstIPNetFamily(ipFamily == utilnet.IPv6, lrpAddresses)
			if err != nil {
				return err
			}

			egressStaticRoute := nbdb.LogicalRouterStaticRoute{
				IPPrefix: clusterSubnet.CIDR.String(),
				Nexthop:  nodeGRAddress.IP.String(),
				Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
			}
			if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient, types.OVNClusterRouter, &egressStaticRoute, func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.IPPrefix == egressStaticRoute.IPPrefix && item.Nexthop == egressStaticRoute.Nexthop && item.Policy != nil && *item.Policy == *egressStaticRoute.Policy
			}); err != nil {
				return fmt.Errorf("failed adding static route for n/s egress traffic: %v", err)
			}
		}
	}
	for _, podIP := range podAnnotation.IPs {
		podAddress := podIP.IP.String()

		if !config.OVNKubernetesFeature.EnableInterconnect {
			// Policy to with low priority to route traffic to the gateway
			ipFamily := utilnet.IPFamilyOfCIDR(podIP)
			nodeGRAddress, err := util.MatchFirstIPNetFamily(ipFamily == utilnet.IPv6, lrpAddresses)
			if err != nil {
				return err
			}

			// adds a policy so that a migrated pods egress traffic
			// will be routed to the local GR where it now resides
			match := fmt.Sprintf("ip%s.src == %s", ipFamily, podAddress)
			egressPolicy := nbdb.LogicalRouterPolicy{
				Match:    match,
				Action:   nbdb.LogicalRouterPolicyActionReroute,
				Nexthops: []string{nodeGRAddress.IP.String()},
				Priority: types.EgressLiveMigrationReroutePiority,
				ExternalIDs: map[string]string{
					OvnZoneExternalIDKey:                  OvnLocalZone,
					string(libovsdbops.VirtualMachineKey): pod.Labels[kubevirtv1.VirtualMachineNameLabel],
					string(libovsdbops.NamespaceKey):      pod.Namespace,
				},
			}
			if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(nbClient, types.OVNClusterRouter, &egressPolicy, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Priority == egressPolicy.Priority && item.Match == egressPolicy.Match && item.Action == egressPolicy.Action
			}); err != nil {
				return fmt.Errorf("failed adding point to point policy for pod %s/%s : %v", pod.Namespace, pod.Name, err)
			}
		}
		// Add a route for reroute ingress traffic to the VM port since
		// the subnet is alien to ovn_cluster_router
		outputPort := types.RouterToSwitchPrefix + pod.Spec.NodeName
		ingressRoute := nbdb.LogicalRouterStaticRoute{
			IPPrefix:   podAddress,
			Nexthop:    podAddress,
			Policy:     &nbdb.LogicalRouterStaticRoutePolicyDstIP,
			OutputPort: &outputPort,
			ExternalIDs: map[string]string{
				OvnZoneExternalIDKey:                  OvnLocalZone,
				string(libovsdbops.VirtualMachineKey): pod.Labels[kubevirtv1.VirtualMachineNameLabel],
				string(libovsdbops.NamespaceKey):      pod.Namespace,
			},
		}
		if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient, types.OVNClusterRouter, &ingressRoute, func(item *nbdb.LogicalRouterStaticRoute) bool {
			matches := item.IPPrefix == ingressRoute.IPPrefix && item.Nexthop == ingressRoute.Nexthop && item.Policy != nil && *item.Policy == *ingressRoute.Policy
			return matches
		}); err != nil {
			return fmt.Errorf("failed adding static route: %v", err)
		}
	}
	return nil
}

// EnsureRemoteZonePodAddressesToNodeRoute will add static routes when live
// migrated pod belongs to remote zone to send traffic over transwitch switch
// port of the node where the pod is running:
//   - A dst-ip with live migrated pod ip as prefix and nexthop the pod's
//     current node transit switch port.
func EnsureRemoteZonePodAddressesToNodeRoute(controllerName string, watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client, switchNames SwitchNames, pod *corev1.Pod, networkName string) error {
	// DHCPOptions are only needed at the node is running the VM
	// at that's the local zone node not the remote zone
	if err := DeleteDHCPOptions(controllerName, nbClient, pod, networkName); err != nil {
		return err
	}
	if switchNames.Current == switchNames.Original {
		// Point to point routing is no longer needed if vm
		// is running at the node where it was created
		if err := DeleteRoutingForMigratedPod(nbClient, pod); err != nil {
			return err
		}
		return nil
	} else {
		// Since we are at remote zone we should not have local zone point to
		// to point routing
		if err := DeleteRoutingForMigratedPodWithZone(nbClient, pod, pointer.String(OvnLocalZone)); err != nil {
			return err
		}
	}

	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
	if err != nil {
		return fmt.Errorf("failed reading ovn annotation: %v", err)
	}

	node, err := watchFactory.GetNode(pod.Spec.NodeName)
	if err != nil {
		return err
	}
	transitSwitchPortAddrs, err := util.ParseNodeTransitSwitchPortAddrs(node)
	if err != nil {
		return err
	}
	for _, podIP := range podAnnotation.IPs {
		ipFamily := utilnet.IPFamilyOfCIDR(podIP)
		transitSwitchPortAddr, err := util.MatchFirstIPNetFamily(ipFamily == utilnet.IPv6, transitSwitchPortAddrs)
		if err != nil {
			return err
		}
		route := nbdb.LogicalRouterStaticRoute{
			IPPrefix: podIP.IP.String(),
			Nexthop:  transitSwitchPortAddr.IP.String(),
			Policy:   &nbdb.LogicalRouterStaticRoutePolicyDstIP,
			ExternalIDs: map[string]string{
				OvnZoneExternalIDKey:                  OvnRemoteZone,
				string(libovsdbops.VirtualMachineKey): pod.Labels[kubevirtv1.VirtualMachineNameLabel],
				string(libovsdbops.NamespaceKey):      pod.Namespace,
			},
		}
		if err := libovsdbops.CreateOrReplaceLogicalRouterStaticRouteWithPredicate(nbClient, types.OVNClusterRouter, &route, func(item *nbdb.LogicalRouterStaticRoute) bool {
			matches := item.IPPrefix == route.IPPrefix && item.Nexthop == route.Nexthop && item.Policy != nil && *item.Policy == *route.Policy
			return matches
		}); err != nil {
			return fmt.Errorf("failed adding static route at remote zone: %v", err)
		}
	}
	return nil
}

func EnsureRoutingForVM(controllerName string, watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client, switchNames SwitchNames, isPodScheduledinLocalZone bool, pod *corev1.Pod, networkName string) error {
	isMigratedSourcePodStale, err := IsMigratedSourcePodStale(watchFactory, pod)
	if err != nil {
		return err
	}
	if util.PodWantsHostNetwork(pod) || !IsPodLiveMigratable(pod) || isMigratedSourcePodStale {
		return nil
	}

	// When the live migration is finish this label will appear at the
	// virt-launcher with the same value as pod.Spec.NodeName
	targetNode := pod.Labels[kubevirtv1.NodeNameLabel]

	// If the live migration has not yet finish but target is ready to receive
	// traffic this annotation will be added with the readiness timestamp
	targetReadyTimestamp := pod.Annotations[kubevirtv1.MigrationTargetReadyTimestamp]

	// No live migration or target node was reached || qemu is already ready
	if targetNode == pod.Spec.NodeName || targetReadyTimestamp != "" {
		if isPodScheduledinLocalZone {
			if err := EnsureLocalZonePodAddressesToNodeRoute(watchFactory, nbClient, switchNames, pod); err != nil {
				return fmt.Errorf("failed ensureLocalZonePodAddressesToNodeRoute for %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		} else {
			if err := EnsureRemoteZonePodAddressesToNodeRoute(controllerName, watchFactory, nbClient, switchNames, pod, networkName); err != nil {
				return fmt.Errorf("failed ensureRemoteZonePodAddressesToNodeRoute for %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		}
	}
	return nil
}

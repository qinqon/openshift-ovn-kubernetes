package kubevirt

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	kubevirtv1 "kubevirt.io/api/core/v1"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// IsPodLiveMigratable will return true if the pod belongs
// to kubevirt and should use the live migration features
func IsPodLiveMigratable(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation]
	return ok
}

// FindVMRelatedPods will return pods belong to the same vm annotated at pod
func FindVMRelatedPods(client *factory.WatchFactory, pod *corev1.Pod) ([]*corev1.Pod, error) {
	vmName, ok := pod.Labels[kubevirtv1.VirtualMachineNameLabel]
	if !ok {
		return nil, nil
	}
	vmPods, err := client.GetPodsBySelector(pod.Namespace, metav1.LabelSelector{MatchLabels: map[string]string{kubevirtv1.VirtualMachineNameLabel: vmName}})
	if err != nil {
		return nil, err
	}
	return vmPods, nil
}

// FindNetworkInfo will return the original switch name and the OVN pod
// annotation from any other pod annotated with the same VM as pod
func FindNetworkInfo(client *factory.WatchFactory, pod *corev1.Pod, networkName string) (NetworkInfo, error) {
	vmPods, err := FindVMRelatedPods(client, pod)
	if err != nil {
		return NetworkInfo{}, fmt.Errorf("failed finding related pods for pod %s/%s when looking for network info: %v", pod.Namespace, pod.Name, err)
	}

	if len(vmPods) == 0 {
		return NetworkInfo{}, fmt.Errorf("missing vm related pods for pod %s/%s", pod.Namespace, pod.Name)
	}

	// By default take pod.Spec.NodeName and annotation from the very same pod
	networkInfo := NetworkInfo{
		OriginalSwitchName: pod.Spec.NodeName,
	}

	ovnPodAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, networkName)
	if err == nil {
		networkInfo.OriginalOvnPodAnnotation = ovnPodAnnotation
	}

	if len(vmPods) == 1 {
		return networkInfo, nil
	}

	originalSwitchNameFound := false
	for _, vmPod := range vmPods {
		if vmPod.Name == pod.Name {
			continue
		}
		if !originalSwitchNameFound {
			networkInfo.OriginalSwitchName, originalSwitchNameFound = vmPod.Labels[OriginalSwitchNameLabel]
		}
		if networkInfo.OriginalOvnPodAnnotation == nil {
			ovnPodAnnotation, err := util.UnmarshalPodAnnotation(vmPod.Annotations, networkName)
			if err != nil {
				klog.Warningf("Failed or not found vm ovn pod annotation: %v", err)
			} else {
				networkInfo.OriginalOvnPodAnnotation = ovnPodAnnotation
			}
		}
		if networkInfo.OriginalOvnPodAnnotation != nil && originalSwitchNameFound {
			break
		}
	}
	if networkInfo.OriginalOvnPodAnnotation == nil {
		return networkInfo, fmt.Errorf("missing ovn pod annotations for vm pod %s/%s", pod.Namespace, pod.Name)
	}
	if !originalSwitchNameFound {
		return networkInfo, fmt.Errorf("missing original switch name label for vm pod %s/%s", pod.Namespace, pod.Name)
	}
	return networkInfo, nil
}

// EnsureNetworkInfoForVM will at live migration extract the ovn pod
// annotations and original switch name from the source vm pod and copy it
// to the target vm pod so ip address follow vm during migration. This has to
// done before creating the LSP to be sure that Address field get configured
// correctly at the target VM pod LSP.
func EnsureNetworkInfoForVM(watchFactory *factory.WatchFactory, kube *kube.KubeOVN, pod *corev1.Pod, networkName string) (*corev1.Pod, error) {
	if !IsPodLiveMigratable(pod) {
		return pod, nil
	}

	// If NetworkInfo is already at the pod, do nothing
	if _, ok := pod.Labels[OriginalSwitchNameLabel]; ok {
		if _, err := util.UnmarshalPodAnnotation(pod.Annotations, networkName); err == nil {
			return pod, nil
		}
	}

	vmNetworkInfo, err := FindNetworkInfo(watchFactory, pod, networkName)
	if err != nil {
		return pod, err
	}
	var modifiedPod *corev1.Pod
	resultErr := retry.RetryOnConflict(util.OvnConflictBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		pod, err := watchFactory.GetPod(pod.Namespace, pod.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		modifiedPod = pod.DeepCopy()
		_, ok := modifiedPod.Labels[OriginalSwitchNameLabel]
		if !ok {
			modifiedPod.Labels[OriginalSwitchNameLabel] = vmNetworkInfo.OriginalSwitchName
		}
		if vmNetworkInfo.OriginalOvnPodAnnotation != nil {
			modifiedPod.Annotations, err = util.MarshalPodAnnotation(modifiedPod.Annotations, vmNetworkInfo.OriginalOvnPodAnnotation, networkName)
			if err != nil {
				return err
			}
		}
		return kube.UpdatePod(modifiedPod)
	})
	if resultErr != nil {
		return pod, fmt.Errorf("failed to update labels and annotations on pod %s/%s: %v", pod.Namespace, pod.Name, resultErr)
	}
	return modifiedPod, nil
}

// IsMigratedSourcePodStale return true if there are other pods related to
// to it and any of them has newer creation timestamp.
func IsMigratedSourcePodStale(client *factory.WatchFactory, pod *corev1.Pod) (bool, error) {
	vmPods, err := FindVMRelatedPods(client, pod)
	if err != nil {
		return false, fmt.Errorf("failed finding related pods for pod %s/%s when checking live migration left overs: %v", pod.Namespace, pod.Name, err)
	}

	for _, vmPod := range vmPods {
		if vmPod.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			return true, nil
		}
	}

	return false, nil
}

// ExternalIDContainsVM return true if the nbdb ExternalIDs has namespace
// and name entries matching the VM
func ExternalIDsContainsVM(externalIDs map[string]string, vm *ktypes.NamespacedName) bool {
	if vm == nil {
		return false
	}
	externalIDsVM := ExtractVMFromExternalIDs(externalIDs)
	if externalIDsVM == nil {
		return false
	}
	return *vm == *externalIDsVM
}

// ExtractVMNameFromPod retunes namespace and name of vm backed up but the pod
// for regular pods return nil
func ExtractVMNameFromPod(pod *corev1.Pod) *ktypes.NamespacedName {
	vmName, ok := pod.Labels[kubevirtv1.VirtualMachineNameLabel]
	if !ok {
		return nil
	}
	return &ktypes.NamespacedName{Namespace: pod.Namespace, Name: vmName}
}

func CleanUpForVM(controllerName string, nbClient libovsdbclient.Client, watchFactory *factory.WatchFactory, pod *corev1.Pod, networkName string) error {
	isMigratedSourcePodStale, err := IsMigratedSourcePodStale(watchFactory, pod)
	if err != nil {
		return fmt.Errorf("failed cleaning up VM when checking if pod is leftover: %v", err)
	}
	// Everything has already being cleand up since this is an old migration
	// pod
	if isMigratedSourcePodStale {
		return nil
	}
	// This pod is not part of ip migration so we don't need to clean up
	if !IsPodLiveMigratable(pod) {
		return nil
	}
	if err := DeleteDHCPOptions(controllerName, nbClient, pod, networkName); err != nil {
		return err
	}
	return nil
}

package apbroute

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	adminpolicybasedrouteinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/informers/externalversions"

	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
)

// Admin Policy Based Route Node controller

type ExternalGatewayNodeController struct {
	stopCh <-chan struct{}

	// route policies

	// routerInformer v1apbinformer.AdminPolicyBasedExternalRouteInformer
	routeLister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister
	routeSynced cache.InformerSynced
	routeQueue  workqueue.RateLimitingInterface

	// Pods
	podLister corev1listers.PodLister
	podSynced cache.InformerSynced
	podQueue  workqueue.RateLimitingInterface

	// Namespaces
	namespaceQueue  workqueue.RateLimitingInterface
	namespaceLister corev1listers.NamespaceLister
	namespaceSynced cache.InformerSynced

	//external gateway caches
	//make them public so that they can be used by the annotation logic to lock on namespaces and share the same external route information
	ExternalGWCache map[ktypes.NamespacedName]*ExternalRouteInfo
	ExGWCacheMutex  *sync.RWMutex

	routePolicyInformer adminpolicybasedrouteinformer.SharedInformerFactory

	mgr *externalPolicyManager
}

func NewExternalNodeController(
	apbRoutePolicyClient adminpolicybasedrouteclient.Interface,
	podInformer coreinformers.PodInformer,
	namespaceInformer coreinformers.NamespaceInformer,
	stopCh <-chan struct{},
) (*ExternalGatewayNodeController, error) {

	namespaceLister := namespaceInformer.Lister()
	routePolicyInformer := adminpolicybasedrouteinformer.NewSharedInformerFactory(apbRoutePolicyClient, resyncInterval)
	externalRouteInformer := routePolicyInformer.K8s().V1().AdminPolicyBasedExternalRoutes()

	c := &ExternalGatewayNodeController{
		stopCh:              stopCh,
		routePolicyInformer: routePolicyInformer,
		routeLister:         routePolicyInformer.K8s().V1().AdminPolicyBasedExternalRoutes().Lister(),
		routeSynced:         routePolicyInformer.K8s().V1().AdminPolicyBasedExternalRoutes().Informer().HasSynced,
		routeQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
			"apbexternalroutes",
		),
		podLister: podInformer.Lister(),
		podSynced: podInformer.Informer().HasSynced,
		podQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
			"apbexternalroutepods",
		),
		namespaceLister: namespaceLister,
		namespaceSynced: namespaceInformer.Informer().HasSynced,
		namespaceQueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
			"apbexternalroutenamespaces",
		),
		mgr: newExternalPolicyManager(
			stopCh,
			podInformer.Lister(),
			namespaceInformer.Lister(),
			routePolicyInformer.K8s().V1().AdminPolicyBasedExternalRoutes().Lister(),
			&conntrackClient{podLister: podInformer.Lister()}),
	}

	_, err := namespaceInformer.Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onNamespaceAdd,
			UpdateFunc: c.onNamespaceUpdate,
			DeleteFunc: c.onNamespaceDelete,
		}))
	if err != nil {
		return nil, err
	}

	_, err = podInformer.Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPodAdd,
			UpdateFunc: c.onPodUpdate,
			DeleteFunc: c.onPodDelete,
		}))
	if err != nil {
		return nil, err
	}
	_, err = externalRouteInformer.Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onPolicyAdd,
			UpdateFunc: c.onPolicyUpdate,
			DeleteFunc: c.onPolicyDelete,
		}))
	if err != nil {
		return nil, err
	}

	return c, nil

}

func (c *ExternalGatewayNodeController) Run(threadiness int) {
	defer utilruntime.HandleCrash()
	klog.V(4).InfoS("Starting Admin Policy Based Route Node Controller")

	c.routePolicyInformer.Start(c.stopCh)

	if !cache.WaitForNamedCacheSync("apbexternalroutenamespaces", c.stopCh, c.namespaceSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.V(4).InfoS("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("apbexternalroutepods", c.stopCh, c.podSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.V(4).InfoS("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("adminpolicybasedexternalroutes", c.stopCh, c.routeSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.V(4).InfoS("Synchronization failed")
		return
	}

	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				// processes route policies
				c.runPolicyWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				// detects gateway pod changes and updates the pod's IP and MAC in the northbound DB
				c.runPodWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				// detects namespace changes and applies polices that match the namespace selector in the `From` policy field
				c.runNamespaceWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	// wait until we're told to stop
	<-c.stopCh

	c.podQueue.ShutDown()
	c.routeQueue.ShutDown()
	c.namespaceQueue.ShutDown()

	wg.Wait()

}

func (c *ExternalGatewayNodeController) onNamespaceAdd(obj interface{}) {
	ns, ok := obj.(*v1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Namespace{}, obj))
		return
	}
	if ns == nil {
		utilruntime.HandleError(errors.New("invalid Namespace provided to onNamespaceAdd()"))
		return
	}
	c.namespaceQueue.Add(ns)
}

func (c *ExternalGatewayNodeController) onNamespaceUpdate(oldObj, newObj interface{}) {
	oldNamespace, ok := oldObj.(*v1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Namespace{}, oldObj))
		return
	}
	newNamespace, ok := newObj.(*v1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Namespace{}, newObj))
		return
	}

	if oldNamespace == nil || newNamespace == nil {
		utilruntime.HandleError(errors.New("invalid Namespace provided to onNamespaceUpdate()"))
		return
	}
	if oldNamespace.ResourceVersion == newNamespace.ResourceVersion || !newNamespace.GetDeletionTimestamp().IsZero() {
		return
	}
	c.namespaceQueue.Add(newNamespace)
}

func (c *ExternalGatewayNodeController) onNamespaceDelete(obj interface{}) {
	ns, ok := obj.(*v1.Namespace)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		ns, ok = tombstone.Obj.(*v1.Namespace)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Namespace: %#v", tombstone.Obj))
			return
		}
	}
	if ns != nil {
		c.namespaceQueue.Add(ns)
	}
}

func (c *ExternalGatewayNodeController) runPolicyWorker(wg *sync.WaitGroup) {
	for c.processNextPolicyWorkItem(wg) {
	}
}

func (c *ExternalGatewayNodeController) processNextPolicyWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, shutdown := c.routeQueue.Get()
	if shutdown {
		return false
	}
	defer c.routeQueue.Done(key)

	klog.V(4).InfoS("Processing policy %s", key)
	_, err := c.mgr.syncRoutePolicy(key.(string), c.routeQueue)
	if err != nil {
		klog.Errorf("Failed to sync APB policy %s: %v", key, err)
	}
	if err != nil {
		if c.routeQueue.NumRequeues(key) < maxRetries {
			klog.V(4).InfoS("Error found while processing policy %s: %w", key, err)
			c.routeQueue.AddRateLimited(key)
			return true
		}
		klog.Warningf("Dropping policy %q out of the queue: %w", key, err)
		utilruntime.HandleError(err)
	}
	c.routeQueue.Forget(key)
	return true
}

func (c *ExternalGatewayNodeController) onPolicyAdd(obj interface{}) {
	_, ok := obj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{}, obj))
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding policy %s", key)
	c.routeQueue.Add(key)
}

func (c *ExternalGatewayNodeController) onPolicyUpdate(oldObj, newObj interface{}) {
	oldRoutePolicy, ok := oldObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{}, oldObj))
		return
	}
	newRoutePolicy, ok := newObj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{}, newObj))
		return
	}

	if oldRoutePolicy == nil || newRoutePolicy == nil {
		utilruntime.HandleError(errors.New("invalid Admin Policy Based External Route provided to onPolicyUpdate()"))
		return
	}
	if oldRoutePolicy.Generation == newRoutePolicy.Generation ||
		!newRoutePolicy.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", newObj, err))
	}
	c.routeQueue.Add(key)
}

func (c *ExternalGatewayNodeController) onPolicyDelete(obj interface{}) {
	_, ok := obj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tomstone %#v", obj))
			return
		}
		_, ok = tombstone.Obj.(*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not an Admin Policy Based External Route %#v", tombstone.Obj))
			return
		}
	}
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.routeQueue.Add(key)
}

func (c *ExternalGatewayNodeController) runNamespaceWorker(wg *sync.WaitGroup) {
	for c.processNextNamespaceWorkItem(wg) {

	}
}

func (c *ExternalGatewayNodeController) processNextNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	obj, shutdown := c.namespaceQueue.Get()
	if shutdown {
		return false
	}
	defer c.namespaceQueue.Done(obj)

	err := c.mgr.syncNamespace(obj.(*v1.Namespace), c.routeQueue)
	if err != nil {
		if c.namespaceQueue.NumRequeues(obj) < maxRetries {
			klog.V(4).InfoS("Error found while processing namespace %s:%w", obj.(*v1.Namespace), err)
			c.namespaceQueue.AddRateLimited(obj)
			return true
		}
		klog.Warningf("Dropping namespace %q out of the queue: %v", obj.(*v1.Namespace).Name, err)
		utilruntime.HandleError(err)
	}
	c.namespaceQueue.Forget(obj)
	return true
}

func (c *ExternalGatewayNodeController) onPodAdd(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Pod{}, obj))
		return
	}
	if pod == nil {
		utilruntime.HandleError(errors.New("invalid Pod provided to onPodAdd()"))
		return
	}
	// if the pod does not have IPs AND there are no multus network status annotations found, skip it
	if len(pod.Status.PodIPs) == 0 && len(pod.Annotations[nettypes.NetworkStatusAnnot]) == 0 {
		return
	}
	c.podQueue.Add(pod)
}

func (c *ExternalGatewayNodeController) onPodUpdate(oldObj, newObj interface{}) {
	o, ok := oldObj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Pod{}, o))
		return
	}
	n, ok := newObj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting %T but received %T", &v1.Pod{}, n))
		return
	}

	if o == nil || n == nil {
		utilruntime.HandleError(errors.New("invalid Pod provided to onPodUpdate()"))
		return
	}
	// if labels AND assigned Pod IPs AND the multus network status annotations are the same, skip processing changes to the pod.
	if reflect.DeepEqual(o.Labels, n.Labels) &&
		reflect.DeepEqual(o.Status.PodIPs, n.Status.PodIPs) &&
		reflect.DeepEqual(o.Annotations[nettypes.NetworkStatusAnnot], n.Annotations[nettypes.NetworkStatusAnnot]) {
		return
	}
	c.podQueue.Add(newObj)
}

func (c *ExternalGatewayNodeController) onPodDelete(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Pod: %#v", tombstone.Obj))
			return
		}
	}
	if pod != nil {
		c.podQueue.Add(pod)
	}
}

func (c *ExternalGatewayNodeController) runPodWorker(wg *sync.WaitGroup) {
	for c.processNextPodWorkItem(wg) {
	}
}

func (c *ExternalGatewayNodeController) processNextPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	obj, shutdown := c.podQueue.Get()
	if shutdown {
		return false
	}
	defer c.podQueue.Done(obj)

	p := obj.(*v1.Pod)
	err := c.mgr.syncPod(p, c.podLister, c.routeQueue)
	if err != nil {
		if c.podQueue.NumRequeues(obj) < maxRetries {
			klog.V(4).InfoS("Error found while processing pod %s/%s:%w", p.Namespace, p.Name, err)
			c.podQueue.AddRateLimited(obj)
			return true
		}
		klog.Warningf("Dropping pod %s/%s out of the queue: %s", p.Namespace, p.Name, err)
		utilruntime.HandleError(err)
	}

	c.podQueue.Forget(obj)
	return true
}

func (c *ExternalGatewayNodeController) GetAdminPolicyBasedExternalRouteIPsForTargetNamespace(namespaceName string) (sets.Set[string], error) {
	gwIPs, err := c.mgr.getDynamicGatewayIPsForTargetNamespace(namespaceName)
	if err != nil {
		return nil, err
	}
	tmpIPs, err := c.mgr.getStaticGatewayIPsForTargetNamespace(namespaceName)
	if err != nil {
		return nil, err
	}

	return gwIPs.Union(tmpIPs), nil
}

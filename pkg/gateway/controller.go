/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	clusterv3service "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerv3service "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routev3service "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	runtimev3 "github.com/envoyproxy/go-control-plane/envoy/service/runtime/v3"
	secretv3 "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	envoyproxytypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/tunnels"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions/apis/v1"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

const (
	controllerName = "kind.sigs.k8s.io/gateway-controller"
	GWClassName    = "cloud-provider-kind"
	maxRetries     = 5
	workers        = 5
)

const (
	grpcKeepaliveTime        = 30 * time.Second
	grpcKeepaliveTimeout     = 5 * time.Second
	grpcKeepaliveMinTime     = 30 * time.Second
	grpcMaxConcurrentStreams = 1000000
)

type ControllerError struct {
	Reason  string
	Message string
}

// Error implements the error interface.
func (e *ControllerError) Error() string {
	return e.Message
}

type Controller struct {
	clusterName       string
	clusterNameserver string

	client   kubernetes.Interface
	gwClient gatewayclient.Interface

	namespaceLister       corev1listers.NamespaceLister
	namespaceListerSynced cache.InformerSynced

	serviceLister       corev1listers.ServiceLister
	serviceListerSynced cache.InformerSynced

	secretLister       corev1listers.SecretLister
	secretListerSynced cache.InformerSynced

	gatewayClassLister       gatewaylisters.GatewayClassLister
	gatewayClassListerSynced cache.InformerSynced

	gatewayLister       gatewaylisters.GatewayLister
	gatewayListerSynced cache.InformerSynced
	gatewayqueue        workqueue.TypedRateLimitingInterface[string]

	httprouteLister       gatewaylisters.HTTPRouteLister
	httprouteListerSynced cache.InformerSynced

	grpcrouteLister       gatewaylisters.GRPCRouteLister
	grpcrouteListerSynced cache.InformerSynced

	xdscache        cachev3.SnapshotCache
	xdsserver       serverv3.Server
	xdsLocalAddress string
	xdsLocalPort    int
	xdsVersion      atomic.Uint64

	tunnelManager *tunnels.TunnelManager
}

func New(
	clusterName string,
	client *kubernetes.Clientset,
	gwClient *gatewayclient.Clientset,
	namespaceInformer corev1informers.NamespaceInformer,
	serviceInformer corev1informers.ServiceInformer,
	secretInformer corev1informers.SecretInformer,
	gatewayClassInformer gatewayinformers.GatewayClassInformer,
	gatewayInformer gatewayinformers.GatewayInformer,
	httprouteInformer gatewayinformers.HTTPRouteInformer,
	grpcrouteInformer gatewayinformers.GRPCRouteInformer,
) (*Controller, error) {
	c := &Controller{
		clusterName:              clusterName,
		client:                   client,
		namespaceLister:          namespaceInformer.Lister(),
		namespaceListerSynced:    namespaceInformer.Informer().HasSynced,
		serviceLister:            serviceInformer.Lister(),
		serviceListerSynced:      serviceInformer.Informer().HasSynced,
		secretLister:             secretInformer.Lister(),
		secretListerSynced:       secretInformer.Informer().HasSynced,
		gwClient:                 gwClient,
		gatewayClassLister:       gatewayClassInformer.Lister(),
		gatewayClassListerSynced: gatewayClassInformer.Informer().HasSynced,
		gatewayLister:            gatewayInformer.Lister(),
		gatewayListerSynced:      gatewayInformer.Informer().HasSynced,
		gatewayqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "gateway"},
		),
		httprouteLister:       httprouteInformer.Lister(),
		httprouteListerSynced: httprouteInformer.Informer().HasSynced,
		grpcrouteLister:       grpcrouteInformer.Lister(),
		grpcrouteListerSynced: grpcrouteInformer.Informer().HasSynced,
	}
	_, err := gatewayClassInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.syncGatewayClass(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(newObj)
			if err == nil {
				c.syncGatewayClass(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.syncGatewayClass(key)
			}
		},
	})
	if err != nil {
		return nil, err
	}

	_, err = gatewayInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			gw := obj.(*gatewayv1.Gateway)
			if gw.Spec.GatewayClassName != GWClassName {
				return
			}
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.gatewayqueue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			gw := newObj.(*gatewayv1.Gateway)
			if gw.Spec.GatewayClassName != GWClassName {
				return
			}
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(newObj)
			if err == nil {
				c.gatewayqueue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			gw := obj.(*gatewayv1.Gateway)
			if gw.Spec.GatewayClassName != GWClassName {
				return
			}
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.gatewayqueue.Add(key)
			}
		},
	})
	if err != nil {
		return nil, err
	}

	_, err = httprouteInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			httproute := obj.(*gatewayv1.HTTPRoute)
			c.processGateways(httproute.Spec.ParentRefs, httproute.Namespace)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldHTTPRoute := oldObj.(*gatewayv1.HTTPRoute)
			newHTTPRoute := newObj.(*gatewayv1.HTTPRoute)
			c.processGateways(append(oldHTTPRoute.Spec.ParentRefs, newHTTPRoute.Spec.ParentRefs...), newHTTPRoute.Namespace)
		},
		DeleteFunc: func(obj interface{}) {
			httproute, ok := obj.(*gatewayv1.HTTPRoute)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					runtime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
					return
				}
				httproute, ok = tombstone.Obj.(*gatewayv1.HTTPRoute)
				if !ok {
					runtime.HandleError(fmt.Errorf("tombstone contained object that is not a HTTPRoute: %#v", obj))
					return
				}
			}
			c.processGateways(httproute.Spec.ParentRefs, httproute.Namespace)
		},
	})
	if err != nil {
		return nil, err
	}

	_, err = grpcrouteInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			grpcroute := obj.(*gatewayv1.GRPCRoute)
			c.processGateways(grpcroute.Spec.ParentRefs, grpcroute.Namespace)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldGRPCRoute := oldObj.(*gatewayv1.GRPCRoute)
			newGRPCRoute := newObj.(*gatewayv1.GRPCRoute)
			c.processGateways(append(oldGRPCRoute.Spec.ParentRefs, newGRPCRoute.Spec.ParentRefs...), newGRPCRoute.Namespace)
		},
		DeleteFunc: func(obj interface{}) {
			grpcroute, ok := obj.(*gatewayv1.GRPCRoute)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					runtime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
					return
				}
				grpcroute, ok = tombstone.Obj.(*gatewayv1.GRPCRoute)
				if !ok {
					runtime.HandleError(fmt.Errorf("tombstone contained object that is not a GRPCRoute: %#v", obj))
					return
				}
			}
			c.processGateways(grpcroute.Spec.ParentRefs, grpcroute.Namespace)
		},
	})
	if err != nil {
		return nil, err
	}

	if config.DefaultConfig.LoadBalancerConnectivity == config.Tunnel {
		c.tunnelManager = tunnels.NewTunnelManager()
	}

	return c, nil
}

func (c *Controller) Init(ctx context.Context) error {
	defer runtime.HandleCrashWithContext(ctx)

	kindGwClass := gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: GWClassName,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: controllerName,
			Description:    ptr.To("cloud-provider-kind gateway API"),
		},
	}

	_, err := c.gwClient.GatewayV1().GatewayClasses().Get(ctx, GWClassName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.gwClient.GatewayV1().GatewayClasses().Create(ctx, &kindGwClass, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create cloud-provider-kind GatewayClass: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get cloud-provider-kind GatewayClass: %w", err)
	}

	// This is kind/kubeadm specific as it uses kube-system/kube-dns as the nameserver
	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		svc, err := c.serviceLister.Services(metav1.NamespaceSystem).Get("kube-dns")
		if err != nil {
			return false, nil
		}
		if svc.Spec.ClusterIP == "" {
			return false, nil
		}
		c.clusterNameserver = svc.Spec.ClusterIP
		return true, nil
	})

	if err != nil {
		return fmt.Errorf("failed to get kube-dns service: %w", err)
	}
	return nil
}

func (c *Controller) syncGatewayClass(key string) {
	startTime := time.Now()
	klog.V(2).Infof("Started syncing gatewayclass %q (%v)", key, time.Since(startTime))
	defer func() {
		klog.V(2).Infof("Finished syncing gatewayclass %q (%v)", key, time.Since(startTime))
	}()

	gwc, err := c.gatewayClassLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(2).Infof("GatewayClass %q has been deleted", key)
		}
		return
	}

	// We only care about the GatewayClass that matches our controller name.
	if gwc.Spec.ControllerName != controllerName {
		return
	}

	newGwc := gwc.DeepCopy()
	// Set the "Accepted" condition to True and update the observedGeneration.
	meta.SetStatusCondition(&newGwc.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gatewayv1.GatewayClassReasonAccepted),
		Message:            "GatewayClass is accepted by this controller.",
		ObservedGeneration: gwc.Generation,
	})

	// Update the status on the API server.
	if _, err := c.gwClient.GatewayV1().GatewayClasses().UpdateStatus(context.Background(), newGwc, metav1.UpdateOptions{}); err != nil {
		klog.Errorf("failed to update gatewayclass status: %v", err)
	}
}

func (c *Controller) Run(ctx context.Context) error {
	defer runtime.HandleCrashWithContext(ctx)

	klog.Info("Starting Envoy proxy controller")
	c.xdscache = cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	c.xdsserver = serverv3.NewServer(ctx, c.xdscache, &xdsCallbacks{})

	var grpcOptions []grpc.ServerOption
	grpcOptions = append(grpcOptions,
		grpc.MaxConcurrentStreams(grpcMaxConcurrentStreams),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    grpcKeepaliveTime,
			Timeout: grpcKeepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             grpcKeepaliveMinTime,
			PermitWithoutStream: true,
		}),
	)
	grpcServer := grpc.NewServer(grpcOptions...)

	discoveryv3.RegisterAggregatedDiscoveryServiceServer(grpcServer, c.xdsserver)
	endpointv3.RegisterEndpointDiscoveryServiceServer(grpcServer, c.xdsserver)
	clusterv3service.RegisterClusterDiscoveryServiceServer(grpcServer, c.xdsserver)
	routev3service.RegisterRouteDiscoveryServiceServer(grpcServer, c.xdsserver)
	listenerv3service.RegisterListenerDiscoveryServiceServer(grpcServer, c.xdsserver)
	secretv3.RegisterSecretDiscoveryServiceServer(grpcServer, c.xdsserver)
	runtimev3.RegisterRuntimeDiscoveryServiceServer(grpcServer, c.xdsserver)

	address, err := GetControlPlaneAddress()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:0", address))
	if err != nil {
		return err
	}
	defer listener.Close()

	addr := listener.Addr()
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("could not assert listener address to TCPAddr: %s", addr.String())
	}

	c.xdsLocalAddress = address
	c.xdsLocalPort = tcpAddr.Port
	go func() {
		klog.Infof("XDS management server listening on %s %d\n", c.xdsLocalAddress, c.xdsLocalPort)
		if err = grpcServer.Serve(listener); err != nil {
			klog.Errorln("gRPC server error:", err)
		}
		grpcServer.Stop()
	}()

	defer c.gatewayqueue.ShutDown()
	klog.Info("Starting Gateway API controller")

	if !cache.WaitForNamedCacheSync(controllerName, ctx.Done(),
		c.gatewayClassListerSynced,
		c.gatewayListerSynced,
		c.httprouteListerSynced,
		c.grpcrouteListerSynced,
		c.namespaceListerSynced,
		c.serviceListerSynced,
		c.secretListerSynced,
	) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runGatewayWorker, time.Second)
	}

	<-ctx.Done()
	klog.Info("Stopping Gateway API controller")
	return nil
}

func (c *Controller) processGateways(references []gatewayv1.ParentReference, localNamespace string) {
	gatewaysToEnqueue := make(map[string]struct{})
	for _, ref := range references {
		if (ref.Group != nil && string(*ref.Group) != gatewayv1.GroupName) ||
			(ref.Kind != nil && string(*ref.Kind) != "Gateway") {
			continue
		}
		namespace := localNamespace
		if ref.Namespace != nil {
			namespace = string(*ref.Namespace)
		}
		key := namespace + "/" + string(ref.Name)
		gatewaysToEnqueue[key] = struct{}{}
	}

	for key := range gatewaysToEnqueue {
		c.gatewayqueue.Add(key)
	}
}

func (c *Controller) runGatewayWorker(ctx context.Context) {
	for c.processNextGatewayItem(ctx) {
	}
}

func (c *Controller) processNextGatewayItem(ctx context.Context) bool {
	key, quit := c.gatewayqueue.Get()
	if quit {
		return false
	}
	defer c.gatewayqueue.Done(key)

	err := c.syncGateway(ctx, key)
	c.handleGatewayErr(err, key)
	return true
}

func (c *Controller) handleGatewayErr(err error, key string) {
	if err == nil {
		c.gatewayqueue.Forget(key)
		return
	}

	if c.gatewayqueue.NumRequeues(key) < maxRetries {
		klog.Infof("Error syncing Gateway %v: %v", key, err)
		c.gatewayqueue.AddRateLimited(key)
		return
	}

	c.gatewayqueue.Forget(key)
	runtime.HandleError(err)
	klog.Infof("Dropping Gateway %q out of the queue: %v", key, err)
}

func GetControlPlaneAddress() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	sort.Slice(interfaces, func(i, j int) bool {
		nameI := interfaces[i].Name
		nameJ := interfaces[j].Name

		if nameI == "docker0" {
			return true
		}
		if nameJ == "docker0" {
			return false
		}

		if nameI == "eth0" {
			return nameJ != "docker0"
		}
		if nameJ == "eth0" {
			return false
		}

		return nameI < nameJ
	})

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && ipNet.IP.To4() != nil && !ipNet.IP.IsLinkLocalUnicast() && !ipNet.IP.IsLoopback() {
				return ipNet.IP.String(), nil
			}
		}
	}

	return "", fmt.Errorf("no suitable global unicast IPv4 address found on any active non-loopback interface")
}

func (c *Controller) UpdateXDSServer(ctx context.Context, nodeid string, resources map[resourcev3.Type][]envoyproxytypes.Resource) error {
	c.xdsVersion.Add(1)

	snapshot, err := cachev3.NewSnapshot(fmt.Sprintf("%d", c.xdsVersion.Load()), resources)
	if err != nil {
		return fmt.Errorf("failed to create new snapshot cache: %v", err)

	}

	if err := c.xdscache.SetSnapshot(ctx, nodeid, snapshot); err != nil {
		return fmt.Errorf("failed to update resource snapshot in management server: %v", err)
	}
	klog.V(4).Infof("Updated snapshot cache with resource snapshot...")
	return nil
}

var _ serverv3.Callbacks = &xdsCallbacks{}

type xdsCallbacks struct{}

func (cb *xdsCallbacks) OnStreamOpen(ctx context.Context, id int64, typ string) error {
	klog.V(2).Infof("xDS stream %d opened for type %s", id, typ)
	return nil
}
func (cb *xdsCallbacks) OnStreamClosed(id int64, node *corev3.Node) {
	nodeID := "unknown"
	if node != nil {
		nodeID = node.GetId()
	}
	klog.V(2).Infof("xDS stream %d closed for node %s", id, nodeID)
}
func (cb *xdsCallbacks) OnStreamRequest(id int64, req *discoveryv3.DiscoveryRequest) error {
	klog.V(5).Infof("xDS stream %d received request for type %s from node %s", id, req.TypeUrl, req.Node.GetId())
	return nil
}
func (cb *xdsCallbacks) OnStreamResponse(ctx context.Context, id int64, req *discoveryv3.DiscoveryRequest, resp *discoveryv3.DiscoveryResponse) {
	klog.V(5).Infof("xDS stream %d sent response for type %s to node %s", id, resp.TypeUrl, req.Node.GetId())
}
func (cb *xdsCallbacks) OnFetchRequest(ctx context.Context, req *discoveryv3.DiscoveryRequest) error {
	klog.V(5).Infof("xDS fetch request received for type %s from node %s", req.TypeUrl, req.Node.GetId())
	return nil
}
func (cb *xdsCallbacks) OnFetchResponse(req *discoveryv3.DiscoveryRequest, resp *discoveryv3.DiscoveryResponse) {
	klog.V(5).Infof("xDS fetch response sent for type %s to node %s", resp.TypeUrl, req.Node.GetId())
}
func (cb *xdsCallbacks) OnStreamDeltaRequest(id int64, req *discoveryv3.DeltaDiscoveryRequest) error {
	return nil
}
func (cb *xdsCallbacks) OnStreamDeltaResponse(id int64, req *discoveryv3.DeltaDiscoveryRequest, resp *discoveryv3.DeltaDiscoveryResponse) {
}
func (cb *xdsCallbacks) OnDeltaStreamClosed(int64, *corev3.Node) {}
func (cb *xdsCallbacks) OnDeltaStreamOpen(context.Context, int64, string) error {
	return nil
}

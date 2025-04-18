package gateway

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

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

type Controller struct {
	gwClient            gatewayclient.Interface
	gatewayLister       gatewaylisters.GatewayLister
	gatewayListerSynced cache.InformerSynced
	gatewayqueue        workqueue.TypedRateLimitingInterface[string]

	httprouteLister       gatewaylisters.HTTPRouteLister
	httprouteListerSynced cache.InformerSynced
	httproutequeue        workqueue.TypedRateLimitingInterface[string]

	grpcrouteLister       gatewaylisters.GRPCRouteLister
	grpcrouteListerSynced cache.InformerSynced
	grpcroutequeue        workqueue.TypedRateLimitingInterface[string]
}

func New(
	gwClient *gatewayclient.Clientset,
	gatewayInformer gatewayinformers.GatewayInformer,
	httprouteInformer gatewayinformers.HTTPRouteInformer,
	grpcrouteInformer gatewayinformers.GRPCRouteInformer,
) (*Controller, error) {
	c := &Controller{
		gwClient:            gwClient,
		gatewayLister:       gatewayInformer.Lister(),
		gatewayListerSynced: gatewayInformer.Informer().HasSynced,
		gatewayqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "gateway"},
		),
		httprouteLister:       httprouteInformer.Lister(),
		httprouteListerSynced: httprouteInformer.Informer().HasSynced,
		httproutequeue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "httproute"},
		),
		grpcrouteLister:       grpcrouteInformer.Lister(),
		grpcrouteListerSynced: grpcrouteInformer.Informer().HasSynced,
		grpcroutequeue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "grpcroute"},
		),
	}

	_, err := gatewayInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
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
			if !c.isOwned(httproute.Spec.ParentRefs) {
				return
			}
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.httproutequeue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			httproute := newObj.(*gatewayv1.HTTPRoute)
			if !c.isOwned(httproute.Spec.ParentRefs) {
				return
			}
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(newObj)
			if err == nil {
				c.httproutequeue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			httproute := obj.(*gatewayv1.HTTPRoute)
			if !c.isOwned(httproute.Spec.ParentRefs) {
				return
			}
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.httproutequeue.Add(key)
			}
		},
	})
	if err != nil {
		return nil, err
	}

	_, err = grpcrouteInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			grpcroute := obj.(*gatewayv1.GRPCRoute)
			if !c.isOwned(grpcroute.Spec.ParentRefs) {
				return
			}
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.grpcroutequeue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			grpcroute := newObj.(*gatewayv1.GRPCRoute)
			if !c.isOwned(grpcroute.Spec.ParentRefs) {
				return
			}
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(newObj)
			if err == nil {
				c.grpcroutequeue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			grpcroute := obj.(*gatewayv1.GRPCRoute)
			if !c.isOwned(grpcroute.Spec.ParentRefs) {
				return
			}
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.grpcroutequeue.Add(key)
			}
		},
	})
	if err != nil {
		return nil, err
	}

	return c, nil
}

// Init install CRDs and creates GatewayClass
func (c *Controller) Init(ctx context.Context) error {
	defer runtime.HandleCrashWithContext(ctx)
	// Install GatewayAPI CRDs if do not exist

	// Create GatewayClass if it does not exist
	gwClass := gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: GWClassName,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: controllerName,
			Description:    ptr.To("cloud-provider-kind gateway API"),
		},
	}

	if _, err := c.gwClient.GatewayV1().GatewayClasses().Get(ctx, GWClassName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		_, err := c.gwClient.GatewayV1().GatewayClasses().Create(ctx, &gwClass, metav1.CreateOptions{})
		if err != nil {
			klog.Infof("faile to create cloud-provider-kind GatewayClass: %v")
			return err
		}
	}

	return nil
}

// Run begins watching and syncing.
func (c *Controller) Run(ctx context.Context) error {
	defer runtime.HandleCrashWithContext(ctx)

	// Let the workers stop when we are done
	defer c.gatewayqueue.ShutDown()
	defer c.httproutequeue.ShutDown()
	defer c.grpcroutequeue.ShutDown()
	klog.Info("Starting Gateway API controller")

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForNamedCacheSync(controllerName, ctx.Done(), c.gatewayListerSynced, c.httprouteListerSynced, c.grpcrouteListerSynced) {
		return fmt.Errorf("Timed out waiting for caches to sync")
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runGatewayWorker, time.Second)
		go wait.UntilWithContext(ctx, c.runHTTPRouteWorker, time.Second)
		go wait.UntilWithContext(ctx, c.runGRPCrouteWorker, time.Second)
	}

	<-ctx.Done()
	klog.Info("Stopping Gateway API controller")
	return nil
}

func (c *Controller) isOwned(references []gatewayv1.ParentReference) bool {
	for _, ref := range references {
		if string(*ref.Group) != "gateway.networking.k8s.io" && string(*ref.Group) != "" {
			continue
		}
		if string(*ref.Kind) != "Gateway" {
			continue
		}

		gw, err := c.gatewayLister.Gateways(string(*ref.Namespace)).Get(string(ref.Name))
		if err != nil {
			continue
		}
		if gw.Spec.GatewayClassName == GWClassName {
			return true
		}
	}
	return false
}

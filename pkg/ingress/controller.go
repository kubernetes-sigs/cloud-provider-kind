package ingress

import (
	"context"
	"crypto/sha256" // New import
	"encoding/hex"  // New import
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	networkingv1informers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	networkinglisters "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions/apis/v1"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

const (
	// IngressClassName is the name of the IngressClass resource
	IngressClassName = "cloud-provider-kind"
	// IngressClassController is the value of the spec.controller field
	IngressClassController = "kind.sigs.k8s.io/ingress-controller"
	// GatewayName is the well-defined name for the Gateway we create in each namespace
	GatewayName = "kind-ingress-gateway"
)

// Controller is the controller implementation for Ingress resources
type Controller struct {
	gatewayClassName string
	clientset        kubernetes.Interface
	gwClientset      gatewayclient.Interface

	ingressLister   networkinglisters.IngressLister
	classLister     networkinglisters.IngressClassLister
	serviceLister   corelisters.ServiceLister
	secretLister    corelisters.SecretLister
	httpRouteLister gatewaylisters.HTTPRouteLister
	gatewayLister   gatewaylisters.GatewayLister

	ingressSynced   cache.InformerSynced
	classSynced     cache.InformerSynced
	serviceSynced   cache.InformerSynced
	secretSynced    cache.InformerSynced
	httpRouteSynced cache.InformerSynced
	gatewaySynced   cache.InformerSynced

	isDefaultClass atomic.Bool
	workqueue      workqueue.TypedRateLimitingInterface[string]

	logger logr.Logger
}

// NewController returns a new ingress controller
func NewController(
	ctx context.Context,
	clientset kubernetes.Interface,
	gwClientset gatewayclient.Interface,
	gatewayClassName string, // Class for managed Gateways
	ingressInformer networkingv1informers.IngressInformer,
	ingressClassInformer networkingv1informers.IngressClassInformer,
	serviceInformer corev1informers.ServiceInformer, // Add Service informer
	secretInformer corev1informers.SecretInformer, // Add Secret informer
	httpRouteInformer gatewayinformers.HTTPRouteInformer, // Add HTTPRoute informer
	gatewayInformer gatewayinformers.GatewayInformer, // Add Gateway informer
) (*Controller, error) {
	logger := klog.FromContext(ctx).WithValues("controller", "ingress")
	controller := &Controller{
		clientset:        clientset,
		gwClientset:      gwClientset,
		gatewayClassName: gatewayClassName,
		ingressLister:    ingressInformer.Lister(),
		classLister:      ingressClassInformer.Lister(),
		serviceLister:    serviceInformer.Lister(),
		secretLister:     secretInformer.Lister(),
		httpRouteLister:  httpRouteInformer.Lister(),
		gatewayLister:    gatewayInformer.Lister(),
		ingressSynced:    ingressInformer.Informer().HasSynced,
		classSynced:      ingressClassInformer.Informer().HasSynced,
		serviceSynced:    serviceInformer.Informer().HasSynced,
		secretSynced:     secretInformer.Informer().HasSynced,
		httpRouteSynced:  httpRouteInformer.Informer().HasSynced,
		gatewaySynced:    gatewayInformer.Informer().HasSynced,
		workqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "ingress"},
		),
		logger: logger,
	}

	logger.Info("Setting up event handlers for Ingress controller")
	// Watch Ingresses
	_, err := ingressInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueIngress,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueIngress(new)
		},
		DeleteFunc: controller.enqueueIngress,
	})
	if err != nil {
		return nil, err
	}

	// Watch IngressClasses
	_, err = ingressClassInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.handleIngressClass,
		UpdateFunc: func(old, new interface{}) { controller.handleIngressClass(new) },
		DeleteFunc: controller.handleIngressClass,
	})
	if err != nil {
		return nil, err
	}

	// Watch Services
	_, err = serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.handleObject,
		UpdateFunc: func(old, new interface{}) { controller.handleObject(new) },
		DeleteFunc: controller.handleObject,
	})
	if err != nil {
		return nil, err
	}

	// Watch Secrets
	_, err = secretInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.handleObject,
		UpdateFunc: func(old, new interface{}) { controller.handleObject(new) },
		DeleteFunc: controller.handleObject,
	})
	if err != nil {
		return nil, err
	}

	// Watch HTTPRoutes
	_, err = httpRouteInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueIngressFromRoute,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueIngressFromRoute(new)
		},
		DeleteFunc: controller.enqueueIngressFromRoute,
	})
	if err != nil {
		return nil, err
	}

	// Watch Gateways
	_, err = gatewayInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.handleGateway,
		UpdateFunc: func(old, new interface{}) { controller.handleGateway(new) },
		DeleteFunc: controller.handleGateway,
	})
	if err != nil {
		return nil, err
	}

	return controller, nil
}

func (c *Controller) Init(ctx context.Context) error {
	l := c.logger.WithValues("ingressClass", IngressClassName)

	// Wait for the caches to be synced before starting workers
	l.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(),
		c.ingressSynced,
		c.classSynced,
		c.serviceSynced,
		c.secretSynced,
		c.httpRouteSynced,
		c.gatewaySynced, // Wait for Gateway cache
	) {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	_, err := c.classLister.Get(IngressClassName)
	if err == nil {
		l.Info("IngressClass already exists")
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get IngressClass '%s': %v", IngressClassName, err)
	}

	l.Info("IngressClass not found, creating...")
	ingressClass := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: IngressClassName,
		},
		Spec: networkingv1.IngressClassSpec{
			Controller: IngressClassController,
		},
	}
	if config.DefaultConfig.IngressDefault {
		ingressClass.Annotations = map[string]string{
			networkingv1.AnnotationIsDefaultIngressClass: "true",
		}
	}
	_, createErr := c.clientset.NetworkingV1().IngressClasses().Create(ctx, ingressClass, metav1.CreateOptions{})
	return createErr
}

// Run will set up the event handlers for types we are interested in, as well
// as start processing components for the specified number of workers.
func (c *Controller) Run(ctx context.Context, workers int) {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	c.logger.Info("Starting Ingress controller")

	c.logger.Info("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	c.logger.Info("Started workers")
	<-ctx.Done()
	c.logger.Info("Shutting down workers")
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	err := func() error {
		defer c.workqueue.Done(key)
		// Run the syncHandler
		if err := c.syncHandler(ctx, key); err != nil {
			// Requeue on error
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeueing", key, err.Error())
		}
		c.workqueue.Forget(key)
		return nil
	}()

	if err != nil {
		runtime.HandleError(err)
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two.
func (c *Controller) syncHandler(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the Ingress resource
	ingress, err := c.ingressLister.Ingresses(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// Kubernetes Garbage Collection will delete child HTTPRoutes due to OwnerReference.
			// We only need to reconcile the Gateway in case this Ingress
			// was the last one providing a particular TLS secret.
			c.logger.V(4).Info("Ingress deleted, reconciling Gateway", "ingress", klog.KRef(namespace, name))
			if _, err := c.reconcileNamespaceGateway(ctx, namespace); err != nil {
				// We still return an error to requeue, as Gateway reconciliation
				// might fail temporarily (e.g., API server issues).
				return fmt.Errorf("failed to reconcile Gateway after Ingress deletion: %w", err)
			}
			return nil
		}
		return err
	}

	// Check if this Ingress is for us
	if !c.isIngressForUs(ingress) {
		c.logger.V(4).Info("Skipping Ingress not for this controller", "ingress", klog.KRef(namespace, name))
		// TODO: If we *used* to own it, we should delete the HTTPRoutes
		// and reconcile the Gateway. For now, we assume GC handles routes.
		return nil
	}

	// === 1. Ensure Namespace Gateway Exists & is Up-to-Date ===
	// This reconciles TLS secrets from ALL Ingresses in the namespace.
	gateway, err := c.reconcileNamespaceGateway(ctx, namespace)
	if err != nil {
		return fmt.Errorf("failed to reconcile Gateway %s/%s: %w", namespace, GatewayName, err)
	}

	// === 2. Reconcile HTTPRoutes (1-to-Many) ===
	c.logger.V(4).Info("Reconciling HTTPRoutes for Ingress", "ingress", klog.KRef(namespace, name))

	// Generate the desired state
	desiredRoutes, err := c.generateDesiredHTTPRoutes(ingress, gateway.Name, gateway.Namespace)
	if err != nil {
		return fmt.Errorf("failed to generate desired HTTPRoutes: %w", err)
	}

	// Get the actual state
	allRoutes, err := c.httpRouteLister.HTTPRoutes(namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list existing HTTPRoutes: %w", err)
	}

	existingRoutes := make(map[string]*gatewayv1.HTTPRoute)
	for _, route := range allRoutes {
		if metav1.IsControlledBy(route, ingress) {
			existingRoutes[route.Name] = route
		}
	}

	// Reconcile: Create/Update
	for routeName, desiredRoute := range desiredRoutes {
		l := c.logger.WithValues("route", klog.KObj(desiredRoute))
		existingRoute, exists := existingRoutes[routeName]
		if !exists {
			// Create
			l.V(2).Info("Creating HTTPRoute")
			_, createErr := c.gwClientset.GatewayV1().HTTPRoutes(namespace).Create(ctx, desiredRoute, metav1.CreateOptions{})
			if createErr != nil {
				l.Error(createErr, "Failed to create HTTPRoute")
				return fmt.Errorf("failed to create HTTPRoute: %w", createErr)
			}
		} else if !reflect.DeepEqual(existingRoute.Spec, desiredRoute.Spec) ||
			!reflect.DeepEqual(existingRoute.OwnerReferences, desiredRoute.OwnerReferences) {

			l.V(2).Info("Updating HTTPRoute")
			routeCopy := existingRoute.DeepCopy()
			routeCopy.Spec = desiredRoute.Spec
			routeCopy.OwnerReferences = desiredRoute.OwnerReferences

			_, updateErr := c.gwClientset.GatewayV1().HTTPRoutes(namespace).Update(ctx, routeCopy, metav1.UpdateOptions{})
			if updateErr != nil {
				l.Error(updateErr, "Failed to update HTTPRoute")
				return fmt.Errorf("failed to update HTTPRoute: %w", updateErr)
			}
		}
		// Remove from map so we can find stale routes
		delete(existingRoutes, routeName)
	}

	// Reconcile: Delete (stale routes)
	for routeName, routeToDelete := range existingRoutes {
		l := c.logger.WithValues("route", klog.KObj(routeToDelete))
		l.V(2).Info("Deleting stale HTTPRoute")
		deleteErr := c.gwClientset.GatewayV1().HTTPRoutes(namespace).Delete(ctx, routeName, metav1.DeleteOptions{})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			l.Error(deleteErr, "Failed to delete stale HTTPRoute")
			return fmt.Errorf("failed to delete stale HTTPRoute: %w", deleteErr)
		}
	}

	// === 3. Update Ingress Status ===
	// We use the Gateway object we found/created. Its status might be stale
	// from the lister if it was just created. We grab the latest.
	latestGateway, err := c.gwClientset.GatewayV1().Gateways(gateway.Namespace).Get(ctx, gateway.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get latest Gateway %s/%s for status update: %w", gateway.Namespace, gateway.Name, err)
	}

	return c.updateIngressStatus(ctx, ingress, latestGateway)
}

// reconcileNamespaceGateway ensures the namespace Gateway exists and
// its TLS configuration is in sync with ALL Ingresses in the namespace.
func (c *Controller) reconcileNamespaceGateway(ctx context.Context, namespace string) (*gatewayv1.Gateway, error) {
	// 1. Aggregate all unique secret names from all Ingresses in this namespace
	ingresses, err := c.ingressLister.Ingresses(namespace).List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list Ingresses in namespace %s: %w", namespace, err)
	}

	secretNames := make(map[string]struct{})
	for _, ing := range ingresses {
		if !c.isIngressForUs(ing) {
			continue
		}
		for _, tls := range ing.Spec.TLS {
			if tls.SecretName != "" {
				secretNames[tls.SecretName] = struct{}{}
			}
		}
	}

	// 2. Build the list of SecretObjectReferences
	certRefs := []gatewayv1.SecretObjectReference{}
	for secretName := range secretNames {
		certRefs = append(certRefs, gatewayv1.SecretObjectReference{
			Name: gatewayv1.ObjectName(secretName),
		})
	}
	// Sort the slice to prevent flapping updates
	sort.Slice(certRefs, func(i, j int) bool {
		return certRefs[i].Name < certRefs[j].Name
	})

	desiredListeners := []gatewayv1.Listener{
		{
			Name:     "http",
			Port:     80,
			Protocol: gatewayv1.HTTPProtocolType,
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: ptr.To(gatewayv1.NamespacesFromSame), // Only allow HTTPRoutes from the same namespace
				},
			},
		},
	}
	// 3. Define the desired listeners
	tlsMode := gatewayv1.TLSModeTerminate // always use Terminate for Ingress TLS
	var finalCertRefs []gatewayv1.SecretObjectReference
	if len(certRefs) > 0 {
		finalCertRefs = certRefs
		desiredListeners = append(desiredListeners, gatewayv1.Listener{
			Name:     "https",
			Port:     443,
			Protocol: gatewayv1.HTTPSProtocolType,
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: ptr.To(gatewayv1.NamespacesFromSame), // Only allow HTTPRoutes from the same namespace
				},
			},
			TLS: &gatewayv1.ListenerTLSConfig{
				Mode:            &tlsMode,
				CertificateRefs: finalCertRefs, // Set the aggregated certs
			},
		})
	}

	// 4. Get or Create the Gateway
	gw, err := c.gatewayLister.Gateways(namespace).Get(GatewayName)
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get Gateway %s/%s: %w", namespace, GatewayName, err)
		}

		// Not found, create it
		c.logger.V(2).Info(
			"Creating Gateway for class",
			"gateway", klog.KRef(namespace, GatewayName),
			"class", c.gatewayClassName)
		newGw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GatewayName,
				Namespace: namespace,
			},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: gatewayv1.ObjectName(c.gatewayClassName),
				Listeners:        desiredListeners,
			},
		}
		return c.gwClientset.GatewayV1().Gateways(namespace).Create(ctx, newGw, metav1.CreateOptions{})
	}

	// 5. Gateway exists, check if update is needed
	if !reflect.DeepEqual(gw.Spec.Listeners, desiredListeners) {
		c.logger.V(2).Info(
			"Updating Gateway with new listener configuration",
			"gateway", klog.KRef(namespace, GatewayName))
		// This avoids conflicts with the gateway-controller updating status.
		patch := map[string]interface{}{
			"spec": map[string]interface{}{
				"listeners": desiredListeners,
			},
		}
		patchBytes, err := json.Marshal(patch)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal patch for Gateway %s/%s: %w", namespace, GatewayName, err)
		}

		return c.gwClientset.GatewayV1().Gateways(namespace).Patch(ctx, gw.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	}

	// No update needed
	return gw, nil
}

// generateRouteName creates a stable, DNS-1123 compliant name for an HTTPRoute
// based on its parent Ingress and the host rule.
func generateRouteName(ingressName, host string) string {
	if host == "" {
		// This is for the default backend
		return fmt.Sprintf("%s-default-backend", ingressName)
	}
	// Use a hash of the host to ensure stability and DNS-1123 compliance
	hash := sha256.Sum256([]byte(host))
	// Truncate hash to 10 chars for brevity
	return fmt.Sprintf("%s-%s", ingressName, hex.EncodeToString(hash[:])[:10])
}

// resolveBackendPort resolves the port number for a given Ingress service backend
func (c *Controller) resolveBackendPort(ns string, svcBackend *networkingv1.IngressServiceBackend) (int32, error) {
	switch svcPort := svcBackend.Port; {
	case svcPort.Number != 0:
		return svcPort.Number, nil
	case svcPort.Name != "":
		// We need to look up the port number from the Service
		svc, err := c.serviceLister.Services(ns).Get(svcBackend.Name)
		if err != nil {
			return 0, fmt.Errorf("failed to get service %s/%s for port name %s: %w",
				ns, svcBackend.Name, svcPort.Name, err)
		}
		// Find the port
		for _, port := range svc.Spec.Ports {
			if port.Name == svcPort.Name {
				return port.Port, nil
			}
		}
		return 0, fmt.Errorf("port name %s not found in service %s/%s",
			svcPort.Name, ns, svcBackend.Name)
	default:
		return 0, fmt.Errorf("backend service %s has no port defined", svcBackend.Name)
	}
}

// translateIngressPaths translates Ingress paths to HTTPRouteRules,
// ensuring Ingress precedence (Exact > Prefix, Longest > Shortest).
func (c *Controller) translateIngressPaths(ns string, paths []networkingv1.HTTPIngressPath) ([]gatewayv1.HTTPRouteRule, error) {
	var rules []gatewayv1.HTTPRouteRule

	// Create a copy to sort
	pathCopy := make([]networkingv1.HTTPIngressPath, len(paths))
	copy(pathCopy, paths)

	// Sort paths to respect Ingress precedence
	sort.Slice(pathCopy, func(i, j int) bool {
		pathI := pathCopy[i]
		pathJ := pathCopy[j]

		typeI := ptr.Deref(pathI.PathType, networkingv1.PathTypePrefix)
		typeJ := ptr.Deref(pathJ.PathType, networkingv1.PathTypePrefix)

		// 1. Exact comes before Prefix
		if typeI == networkingv1.PathTypeExact && typeJ != networkingv1.PathTypeExact {
			return true
		}
		if typeI != networkingv1.PathTypeExact && typeJ == networkingv1.PathTypeExact {
			return false
		}

		// 2. If both are same type, longest path wins
		return len(pathI.Path) > len(pathJ.Path)
	})

	for _, ingressPath := range pathCopy {
		// Determine Path Match Type
		var pathMatch gatewayv1.HTTPPathMatch
		pathType := ptr.Deref(ingressPath.PathType, networkingv1.PathTypePrefix)

		switch pathType {
		case networkingv1.PathTypePrefix:
			pathMatch.Type = ptr.To(gatewayv1.PathMatchPathPrefix)
			pathMatch.Value = &ingressPath.Path
		case networkingv1.PathTypeExact:
			pathMatch.Type = ptr.To(gatewayv1.PathMatchExact)
			pathMatch.Value = &ingressPath.Path
		case networkingv1.PathTypeImplementationSpecific:
			// Fallback (e.g., ImplementationSpecific)
			pathMatch.Type = ptr.To(gatewayv1.PathMatchPathPrefix)
			pathMatch.Value = &ingressPath.Path
		default:
			return nil, fmt.Errorf("unsupported path type: %s", pathType)
		}

		// Determine Backend Port
		port, err := c.resolveBackendPort(ns, ingressPath.Backend.Service)
		if err != nil {
			// This error will cause the Ingress to be requeued
			return nil, fmt.Errorf("error resolving port for path %s: %w", ingressPath.Path, err)
		}
		portPtr := port // Take address of a copy

		// Create BackendRef
		backendRef := gatewayv1.HTTPBackendRef{
			BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name:  gatewayv1.ObjectName(ingressPath.Backend.Service.Name),
					Kind:  ptr.To(gatewayv1.Kind("Service")),
					Group: ptr.To(gatewayv1.Group("")),
					Port:  &portPtr,
				},
				Weight: ptr.To(int32(1)),
			},
		}

		// Create HTTPRouteRule
		httpRule := gatewayv1.HTTPRouteRule{
			Matches: []gatewayv1.HTTPRouteMatch{
				{
					Path: &pathMatch,
				},
			},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendRef},
		}
		rules = append(rules, httpRule)
	}

	return rules, nil
}

// generateDesiredHTTPRoutes generates a map of all HTTPRoutes that should
// exist for a given Ingress.
func (c *Controller) generateDesiredHTTPRoutes(ingress *networkingv1.Ingress, gatewayName string, gatewayNamespace string) (map[string]*gatewayv1.HTTPRoute, error) {
	desiredRoutes := make(map[string]*gatewayv1.HTTPRoute)

	// Set OwnerReference
	ownerRef := metav1.NewControllerRef(ingress, networkingv1.SchemeGroupVersion.WithKind("Ingress"))

	// Base ParentReference
	parentRef := gatewayv1.ParentReference{
		Name:      gatewayv1.ObjectName(gatewayName),
		Namespace: ptr.To(gatewayv1.Namespace(gatewayNamespace)),
		Kind:      ptr.To(gatewayv1.Kind("Gateway")),
		Group:     ptr.To(gatewayv1.Group(gatewayv1.GroupName)),
	}

	var defaultPaths []networkingv1.HTTPIngressPath
	var hasDefaultRule bool

	// 1. Separate per-host rules from default (host: "") rules
	for _, ingressRule := range ingress.Spec.Rules {
		if ingressRule.Host == "" {
			// This is a default rule. Collect its paths.
			if ingressRule.HTTP != nil {
				defaultPaths = append(defaultPaths, ingressRule.HTTP.Paths...)
				hasDefaultRule = true
			}
			continue // Skip per-host route creation
		}

		if ingressRule.HTTP == nil {
			continue
		}
		routeName := generateRouteName(ingress.Name, ingressRule.Host)
		hostnames := []gatewayv1.Hostname{gatewayv1.Hostname(ingressRule.Host)}

		// Translate paths for this rule
		rules, err := c.translateIngressPaths(ingress.Namespace, ingressRule.HTTP.Paths)
		if err != nil {
			// Propagate error (e.g., service port not found)
			return nil, fmt.Errorf("failed to translate paths for host %s: %w", ingressRule.Host, err)
		}

		// Construct the per-host HTTPRoute
		httpRoute := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:            routeName,
				Namespace:       ingress.Namespace,
				OwnerReferences: []metav1.OwnerReference{*ownerRef},
			},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{parentRef},
				},
				Hostnames: hostnames, // This is correct, as host is not empty
				Rules:     rules,
			},
		}
		desiredRoutes[routeName] = httpRoute
	}

	// 2. Handle DefaultBackend (either from explicit rule or spec.defaultBackend)
	var defaultRules []gatewayv1.HTTPRouteRule
	var err error

	if hasDefaultRule { //nolint:gocritic
		// Case 1: A rule with host: "" exists. This takes precedence.
		defaultRules, err = c.translateIngressPaths(ingress.Namespace, defaultPaths)
		if err != nil {
			return nil, fmt.Errorf("failed to translate paths for default rule: %w", err)
		}
	} else if ingress.Spec.DefaultBackend != nil {
		// Case 2: No host: "" rule, but spec.defaultBackend exists.
		port, err := c.resolveBackendPort(ingress.Namespace, ingress.Spec.DefaultBackend.Service)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve port for default backend: %w", err)
		}
		portPtr := port // Take address of a copy

		defaultRules = []gatewayv1.HTTPRouteRule{
			{
				// No matches means it's the default
				BackendRefs: []gatewayv1.HTTPBackendRef{
					{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name:  gatewayv1.ObjectName(ingress.Spec.DefaultBackend.Service.Name),
								Kind:  ptr.To(gatewayv1.Kind("Service")),
								Group: ptr.To(gatewayv1.Group("")),
								Port:  &portPtr,
							},
							Weight: ptr.To(int32(1)),
						},
					},
				},
			},
		}
	} else {
		// Case 3: No default backend rules and no spec.defaultBackend.
		// We are done.
		return desiredRoutes, nil
	}

	// 3. Create the single default HTTPRoute
	routeName := generateRouteName(ingress.Name, "") // Name is the same for both default cases
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:            routeName,
			Namespace:       ingress.Namespace,
			OwnerReferences: []metav1.OwnerReference{*ownerRef},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{parentRef},
			},
			// Hostnames field is OMITTED (nil), which correctly
			// tells the Gateway to match all hosts.
			Rules: defaultRules,
		},
	}
	desiredRoutes[routeName] = httpRoute

	return desiredRoutes, nil
}

// updateIngressStatus updates the Ingress status with the Gateway's IP
func (c *Controller) updateIngressStatus(ctx context.Context, ingress *networkingv1.Ingress, gateway *gatewayv1.Gateway) error {

	// Get the *latest* version of the Ingress to avoid update conflicts
	latestIngress, err := c.clientset.NetworkingV1().Ingresses(ingress.Namespace).Get(ctx, ingress.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Ingress was deleted, nothing to update
			return nil
		}
		return fmt.Errorf("failed to get latest Ingress %s/%s: %w", ingress.Namespace, ingress.Name, err)
	}

	// Find an IP address in the Gateway status
	ips := []string{}
	hostnames := []string{}
	for _, addr := range gateway.Status.Addresses {
		if addr.Type != nil && *addr.Type == gatewayv1.HostnameAddressType {
			hostnames = append(hostnames, addr.Value)
		}
		if addr.Type != nil && (*addr.Type == gatewayv1.IPAddressType) {
			ips = append(ips, addr.Value)
		}
	}

	if len(ips) == 0 && len(hostnames) == 0 {
		// No IP address found yet. The Gateway controller hasn't finished.
		// Return an error to trigger a rate-limited requeue.
		return fmt.Errorf("gateway %s/%s has no IP or Hostname address in status yet", gateway.Namespace, gateway.Name)
	}

	// Construct the new status
	lbStatus := &networkingv1.IngressLoadBalancerStatus{}
	for _, ip := range ips {
		lbStatus.Ingress = append(lbStatus.Ingress, networkingv1.IngressLoadBalancerIngress{IP: ip})
	}
	for _, hostname := range hostnames {
		lbStatus.Ingress = append(lbStatus.Ingress, networkingv1.IngressLoadBalancerIngress{Hostname: hostname})
	}

	// Check if status is already up-to-date
	if reflect.DeepEqual(latestIngress.Status.LoadBalancer, *lbStatus) {
		c.logger.V(4).Info("Ingress status already up to date", "ingress", klog.KRef(ingress.Namespace, ingress.Name))
		return nil
	}

	ingressCopy := latestIngress.DeepCopy()
	ingressCopy.Status.LoadBalancer = *lbStatus

	_, err = c.clientset.NetworkingV1().Ingresses(ingress.Namespace).UpdateStatus(ctx, ingressCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ingress status: %w", err)
	}
	c.logger.V(2).Info(
		"Successfully updated status for Ingress",
		"ingress", klog.KRef(ingress.Namespace, ingress.Name),
		"IPs", ips,
		"host", hostnames)
	return nil
}

// enqueueIngress takes an Ingress resource and converts it into a
// namespace/name string which is then put onto the work queue.
func (c *Controller) enqueueIngress(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.logger.V(4).Info("Enqueuing Ingress", "ingress", key)
	c.workqueue.Add(key)
}

// enqueueIngressFromRoute finds the owning Ingress for an HTTPRoute and enqueues it
func (c *Controller) enqueueIngressFromRoute(obj interface{}) {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		route, ok = tombstone.Obj.(*gatewayv1.HTTPRoute)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
	}

	// Find the Ingress owner
	ownerRef := metav1.GetControllerOf(route)
	if ownerRef == nil {
		return
	}

	// Check if the owner is an Ingress
	if ownerRef.APIVersion == networkingv1.SchemeGroupVersion.String() && ownerRef.Kind == "Ingress" {
		// Enqueue the Ingress
		key := route.Namespace + "/" + ownerRef.Name
		c.logger.V(4).Info("Enqueuing Ingress due to change in HTTPRoute", "ingress", key, "route", route.Name)
		c.workqueue.Add(key)
	}
}

// handleIngressClass checks if we are the default class
func (c *Controller) handleIngressClass(obj interface{}) {
	l := c.logger.WithValues("ingressClass", IngressClassName)
	class, ok := obj.(*networkingv1.IngressClass)
	if !ok {
		return
	}

	if class.Name != IngressClassName {
		return
	}
	// Check if this is the default class
	isDefault := false
	val, ok := class.Annotations[networkingv1.AnnotationIsDefaultIngressClass]
	if ok && val == "true" {
		isDefault = true
	}
	if config.DefaultConfig.IngressDefault && !isDefault {
		l.Info("Set default IngressClass")
		_, err := c.clientset.NetworkingV1().IngressClasses().Patch(context.TODO(), IngressClassName, types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+networkingv1.AnnotationIsDefaultIngressClass+`":"true"}}}`), metav1.PatchOptions{})
		if err != nil {
			l.Error(err, "Failed to patch IngressClass")
		}
		isDefault = true
	}
	if isDefault != c.isDefaultClass.Load() {
		{
			if isDefault {
				l.Info("Set default IngressClass")
			} else {
				l.Info("No longer the default IngressClass")
			}
			c.isDefaultClass.Store(isDefault)
			// Re-enqueue all Ingresses that might be affected by this change
			c.enqueueAllIngresses()
		}

	}
}

// handleObject will take any resource implementing metav1.Object
// and find any Ingress that references it, adding that Ingress to the
// work queue.
func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		// handle delete tombstones
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
	}

	// List all ingresses
	ingresses, err := c.ingressLister.List(labels.Everything())
	if err != nil {
		c.logger.Error(err, "Failed to list Ingresses")
		return
	}

	for _, ingress := range ingresses {
		l := c.logger.WithValues("ingress", klog.KObj(ingress))
		if !c.isIngressForUs(ingress) {
			continue
		}

		// Check if this ingress references the object
		switch obj.(type) {
		case *corev1.Service:
			if c.ingressReferencesService(ingress, object.GetNamespace(), object.GetName()) {
				l.V(4).Info("Enqueuing Ingress due to change in Service", "service", klog.KObj(object))
				c.enqueueIngress(ingress)
			}
		case *corev1.Secret:
			if c.ingressReferencesSecret(ingress, object.GetNamespace(), object.GetName()) {
				l.V(4).Info("Enqueuing Ingress due to change in Secret", "service", klog.KObj(object))
				// When a secret changes, we must re-enqueue ALL ingresses in that namespace
				// to re-calculate the Gateway's aggregated certificate list.
				c.enqueueAllIngressesInNamespace(object.GetNamespace())
			}
		}
	}
}

// handleGateway re-enqueues all Ingresses in a namespace when its Gateway changes
func (c *Controller) handleGateway(obj interface{}) {
	gw, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		gw, ok = tombstone.Obj.(*gatewayv1.Gateway)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
	}

	// If this is one of our managed Gateways, re-enqueue all Ingresses in that namespace
	// This is critical for updating Ingress status when the Gateway gets an IP
	if gw.Name == GatewayName {
		c.logger.V(4).Info("Gateway changed, re-enqueuing all Ingresses in namespace", "gateway", klog.KObj(gw), "namespace", gw.Namespace)
		c.enqueueAllIngressesInNamespace(gw.Namespace)
	}
}

// enqueueAllIngressesInNamespace enqueues all Ingresses for a specific namespace
func (c *Controller) enqueueAllIngressesInNamespace(namespace string) {
	ingresses, err := c.ingressLister.Ingresses(namespace).List(labels.Everything())
	if err != nil {
		c.logger.Error(err, "Failed to list Ingresses in namespace", "namespace", namespace)
		return
	}
	for _, ingress := range ingresses {
		c.enqueueIngress(ingress)
	}
}

func (c *Controller) ingressReferencesService(ingress *networkingv1.Ingress, ns, name string) bool {
	if ingress.Namespace != ns {
		return false
	}
	if ingress.Spec.DefaultBackend != nil && ingress.Spec.DefaultBackend.Service.Name == name {
		return true
	}
	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service.Name == name {
				return true
			}
		}
	}
	return false
}

func (c *Controller) ingressReferencesSecret(ingress *networkingv1.Ingress, ns, name string) bool {
	if ingress.Namespace != ns {
		return false
	}
	for _, tls := range ingress.Spec.TLS {
		if tls.SecretName == name {
			return true
		}
	}
	return false
}

// isIngressForUs checks if an Ingress belongs to this controller
func (c *Controller) isIngressForUs(ingress *networkingv1.Ingress) bool {
	// Case 1: Ingress specifies IngressClassName
	if ingress.Spec.IngressClassName != nil {
		return *ingress.Spec.IngressClassName == IngressClassName
	}
	// Case 2: No IngressClassName, check if we are default
	return c.isDefaultClass.Load()
}

func (c *Controller) enqueueAllIngresses() {
	ingresses, err := c.ingressLister.List(labels.Everything())
	if err != nil {
		c.logger.Error(err, "Failed to list all Ingresses")
		return
	}
	c.logger.Info("Enqueuing all Ingresses due to IngressClass change")
	for _, ingress := range ingresses {
		c.enqueueIngress(ingress)
	}
}

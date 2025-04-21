package gateway

import (
	"context"
	"fmt"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	envoyproxytypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func (c *Controller) processNextGatewayItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.gatewayqueue.Get()
	if quit {
		return false
	}
	defer c.gatewayqueue.Done(key)

	err := c.syncGateway(key)

	c.handleGatewayErr(err, key)
	return true
}

// syncToStdout is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *Controller) syncGateway(key string) error {
	startTime := time.Now()
	defer func() {
		klog.V(2).Infof("Finished syncing gateway %q (%v)", key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	gw, err := c.gatewayLister.Gateways(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}
	containerName := gatewayName(c.clusterName, namespace, name)

	// Deleting
	if apierrors.IsNotFound(err) {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		klog.Infof("Gateway %s does not exist anymore, deleting \n", key)
		c.xdscache.ClearSnapshot(containerName)

		err := container.Delete(containerName)
		if err != nil {
			return fmt.Errorf("can not delete container %s for gateway %s/%s on cluster %s : %v", containerName, namespace, name, c.clusterName, err)
		}
		return nil
	}
	// Create or Update
	klog.Infof("Syncing Gateway %s\n", gw.GetName())
	if !container.IsRunning(containerName) {
		klog.Infof("container %s for gateway is not running", name)
		if container.Exist(containerName) {
			err := container.Delete(containerName)
			if err != nil {
				return err
			}
		}
	}
	if !container.Exist(containerName) {
		klog.V(2).Infof("creating container %s for gateway  %s/%s on cluster %s", containerName, namespace, name, c.clusterName)
		enableTunnels := c.tunnelManager != nil || config.DefaultConfig.LoadBalancerConnectivity == config.Portmap
		err := createGateway(c.clusterName, c.xdsLocalAddress, c.xdsLocalPort, gw, enableTunnels)
		if err != nil {
			return err
		}
	}

	// Update configuration
	resources := map[resourcev3.Type][]envoyproxytypes.Resource{}
	gwConditions := []metav1.Condition{{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gatewayv1.GatewayReasonAccepted),
		ObservedGeneration: gw.Generation,
		LastTransitionTime: metav1.Now(),
	}}

	lisStatus := make([]gatewayv1.ListenerStatus, len(gw.Spec.Listeners))
	for i, listener := range gw.Spec.Listeners {
		// Determine the Envoy protocol based on the Gateway API protocol
		var envoyProto corev3.SocketAddress_Protocol
		switch listener.Protocol {
		case gatewayv1.UDPProtocolType:
			envoyProto = corev3.SocketAddress_UDP
		default: // TCP, HTTP, HTTPS, TLS all use TCP at the transport layer
			envoyProto = corev3.SocketAddress_TCP
		}

		resources[resourcev3.ListenerType] = append(resources[resourcev3.ListenerType], &listenerv3.Listener{
			Name: string(listener.Name),
			Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
				Protocol: envoyProto,
				Address:  "0.0.0.0", // Or "::" for IPv6, or specific IP if needed
				PortSpecifier: &corev3.SocketAddress_PortValue{
					PortValue: uint32(listener.Port),
				},
			}}},
		})

		// Process HTTP Routes
		var attachedRoutes int32
		for _, route := range c.getHTTPRoutesForListener(gw, listener) {
			klog.V(2).Infof("Processing http route %s/%s for gw %s/%s", route.Namespace, route.Name, gw.Namespace, gw.Name)
			gwConditions = append(gwConditions, metav1.Condition{})
		}

		for _, route := range c.getGRPCRoutesForListener(gw, listener) {
			klog.V(2).Infof("Processing grpc route %s/%s for gw %s/%s", route.Namespace, route.Name, gw.Namespace, gw.Name)
			gwConditions = append(gwConditions, metav1.Condition{})
		}

		lisStatus[i] = gatewayv1.ListenerStatus{
			Name:           listener.Name,
			AttachedRoutes: attachedRoutes,
			Conditions: []metav1.Condition{{
				Type:               string(gatewayv1.ListenerConditionAccepted),
				Status:             metav1.ConditionTrue,
				Reason:             string(gatewayv1.ListenerReasonAccepted),
				ObservedGeneration: gw.Generation,
				LastTransitionTime: metav1.Now(),
			}},
		}
	}

	err = c.UpdateXDSServer(context.Background(), containerName, resources)
	if err != nil {
		gwConditions, _ = UpdateConditionIfChanged(gwConditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionFalse,
			Reason:             string(gatewayv1.GatewayReasonProgrammed),
			Message:            err.Error(),
			ObservedGeneration: gw.Generation,
			LastTransitionTime: metav1.Now(),
		})

	} else {
		gwConditions, _ = UpdateConditionIfChanged(gwConditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionTrue,
			Reason:             string(gatewayv1.GatewayReasonProgrammed),
			ObservedGeneration: gw.Generation,
			LastTransitionTime: metav1.Now(),
		})
	}

	gw.Status.Listeners = lisStatus
	gw.Status.Conditions = gwConditions

	_, err = c.gwClient.GatewayV1().Gateways(gw.Namespace).UpdateStatus(context.Background(), gw, metav1.UpdateOptions{})
	return err
}

// getHTTPRoutesForListener returns a slice of HTTPRoutes that reference the given Gateway and listener.
func (c *Controller) getHTTPRoutesForListener(gw *gatewayv1.Gateway, listener gatewayv1.Listener) []*gatewayv1.HTTPRoute {
	var matchingRoutes []*gatewayv1.HTTPRoute

	// TODO: optimize this
	// List all HTTPRoutes in all namespaces
	httpRoutes, err := c.httprouteLister.List(labels.Everything())
	if err != nil {
		klog.Infof("failed to list HTTPRoutes: %v", err)
		return matchingRoutes
	}

	for _, route := range httpRoutes {
		// Check 1: Does the route *want* to attach to this specific listener?
		// This verifies the route's parentRefs target this gateway and listener section/port.
		if !isRouteReferenced(gw, listener, route) {
			klog.V(5).Infof("Route %s/%s skipped for Gateway %s/%s Listener %s: not referenced in ParentRefs", route.Namespace, route.Name, gw.Namespace, gw.Name, listener.Name)
			continue
		}

		// Check 2: Does the listener *allow* this route to attach?
		// This verifies listener.spec.allowedRoutes (namespace and kind).
		// Assumes c.namespaceLister is populated.
		if !isRouteAllowed(gw, listener, route, c.namespaceLister) {
			klog.V(5).Infof("Route %s/%s skipped for Gateway %s/%s Listener %s: denied by AllowedRoutes", route.Namespace, route.Name, gw.Namespace, gw.Name, listener.Name)
			continue
		}

		// Check 3: Is the route kind compatible with the listener protocol?
		// For this function specifically getting HTTPRoutes, the listener must accept HTTP or HTTPS.
		if listener.Protocol != gatewayv1.HTTPProtocolType && listener.Protocol != gatewayv1.HTTPSProtocolType {
			klog.V(5).Infof("Route %s/%s skipped for Gateway %s/%s Listener %s: incompatible listener protocol %s", route.Namespace, route.Name, gw.Namespace, gw.Name, listener.Name, listener.Protocol)
			continue // Skip route if listener protocol isn't HTTP/HTTPS
		}

		// If all checks pass, add the route
		matchingRoutes = append(matchingRoutes, route)
		klog.V(4).Infof("Route %s/%s matched for Gateway %s/%s Listener %s", route.Namespace, route.Name, gw.Namespace, gw.Name, listener.Name)
	}

	return matchingRoutes
}

// getGRPCRoutesForListener returns a slice of GRPCRoutes that reference the given Gateway and listener.
func (c *Controller) getGRPCRoutesForListener(gw *gatewayv1.Gateway, listener gatewayv1.Listener) []*gatewayv1.GRPCRoute {
	var matchingRoutes []*gatewayv1.GRPCRoute

	// TODO: optimize this
	// List all GRPCRoutes in all namespaces
	grpcRoutes, err := c.grpcrouteLister.List(labels.Everything())
	if err != nil {
		klog.Infof("failed to list GRPCRoutes: %v", err)
		return matchingRoutes
	}

	for _, route := range grpcRoutes {
		// Check 1: Does the route *want* to attach to this specific listener?
		// This verifies the route's parentRefs target this gateway and listener section/port.
		if !isRouteReferenced(gw, listener, route) {
			klog.V(5).Infof("Route %s/%s skipped for Gateway %s/%s Listener %s: not referenced in ParentRefs", route.Namespace, route.Name, gw.Namespace, gw.Name, listener.Name)
			continue
		}

		// Check 2: Does the listener *allow* this route to attach?
		// This verifies listener.spec.allowedRoutes (namespace and kind).
		// Assumes c.namespaceLister is populated.
		if !isRouteAllowed(gw, listener, route, c.namespaceLister) {
			klog.V(5).Infof("Route %s/%s skipped for Gateway %s/%s Listener %s: denied by AllowedRoutes", route.Namespace, route.Name, gw.Namespace, gw.Name, listener.Name)
			continue
		}

		// Check 3: Is the route kind compatible with the listener protocol?
		// For this function specifically getting HTTPRoutes, the listener must accept HTTP or HTTPS.
		if listener.Protocol != gatewayv1.HTTPProtocolType && listener.Protocol != gatewayv1.HTTPSProtocolType {
			klog.V(5).Infof("Route %s/%s skipped for Gateway %s/%s Listener %s: incompatible listener protocol %s", route.Namespace, route.Name, gw.Namespace, gw.Name, listener.Name, listener.Protocol)
			continue // Skip route if listener protocol isn't HTTP/HTTPS
		}

		// If all checks pass, add the route
		matchingRoutes = append(matchingRoutes, route)
		klog.V(4).Infof("Route %s/%s matched for Gateway %s/%s Listener %s", route.Namespace, route.Name, gw.Namespace, gw.Name, listener.Name)
	}

	return matchingRoutes
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleGatewayErr(err error, key string) {
	if err == nil {
		c.gatewayqueue.Forget(key)
		return
	}

	if c.gatewayqueue.NumRequeues(key) < maxRetries {
		klog.Infof("Error syncing Gateway %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.gatewayqueue.AddRateLimited(key)
		return
	}

	c.gatewayqueue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping Gateway %q out of the queue: %v", key, err)
}

func (c *Controller) runGatewayWorker(ctx context.Context) {
	for c.processNextGatewayItem() {
	}
}

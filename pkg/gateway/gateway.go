package gateway

import (
	"context"
	"fmt"
	"net"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"

	envoyproxytypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"

	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

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
	newGw := gw.DeepCopy()
	ipv4, ipv6, err := container.IPs(containerName)
	if err != nil {
		if strings.Contains(err.Error(), "failed to get container details") {
			return err
		}
		return err
	}
	newGw.Status.Addresses = []gatewayv1.GatewayStatusAddress{}
	if net.ParseIP(ipv4) != nil {
		newGw.Status.Addresses = append(newGw.Status.Addresses,
			gatewayv1.GatewayStatusAddress{
				Type:  ptr.To(gatewayv1.IPAddressType),
				Value: ipv4,
			})
	}
	if net.ParseIP(ipv6) != nil {
		newGw.Status.Addresses = append(newGw.Status.Addresses,
			gatewayv1.GatewayStatusAddress{
				Type:  ptr.To(gatewayv1.IPAddressType),
				Value: ipv6,
			})
	}

	resources := map[resourcev3.Type][]envoyproxytypes.Resource{}
	newGw.Status.Conditions, _ = UpdateConditionIfChanged(newGw.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gatewayv1.GatewayReasonAccepted),
		ObservedGeneration: gw.Generation,
		LastTransitionTime: metav1.Now(),
	})

	lisStatus := make([]gatewayv1.ListenerStatus, len(gw.Spec.Listeners))
	for i, listener := range gw.Spec.Listeners {
		envoyListener, err := translateListenerToEnvoyListener(listener)
		if err != nil {
			klog.Errorf("Error translating listener %s: %v", listener.Name, err)
			// TODO: Set appropriate listener condition
			continue
		}

		mergeEnvoyResources(resources, envoyListener)

		// Process HTTP Routes
		var attachedRoutes int32
		// getHTTPRoutesForListener processes parent references on routes
		for _, route := range c.getHTTPRoutesForListener(gw, listener) {
			klog.V(2).Infof("Processing http route %s/%s for gw %s/%s", route.Namespace, route.Name, gw.Namespace, gw.Name)
			// Check hostnames between listener and route
			if !hostnamesIntersect(listener.Hostname, route.Spec.Hostnames) {
				klog.V(4).Infof("HTTPRoute %s/%s hostnames do not intersect with Gateway %s/%s Listener %s hostname, skipping", route.Namespace, route.Name, gw.Namespace, gw.Name, listener.Name)
				continue // Skip this route for this listener if hostnames don't intersect
			}

			routeEnvoyResources, err := translateHTTPRouteToEnvoyResources(c.serviceLister, route)
			if err != nil {
				klog.Errorf("Error translating HTTPRoute %s/%s: %v", route.Namespace, route.Name, err)
				continue // Skip this route if translation fails
			}

			// Merge the translated resources into the main map
			mergeEnvoyResources(resources, routeEnvoyResources)
		}

		for _, route := range c.getGRPCRoutesForListener(gw, listener) {
			klog.V(2).Infof("Processing grpc route %s/%s for gw %s/%s", route.Namespace, route.Name, gw.Namespace, gw.Name)
			resources[resourcev3.RouteType] = append(resources[resourcev3.RouteType], &routev3.Route{})
			resources[resourcev3.ClusterType] = append(resources[resourcev3.ClusterType], &clusterv3.Cluster{})
		}

		lisStatus[i] = gatewayv1.ListenerStatus{
			Name:           listener.Name,
			SupportedKinds: []gatewayv1.RouteGroupKind{{Kind: "HTTPRoute"}},
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
		newGw.Status.Conditions, _ = UpdateConditionIfChanged(newGw.Status.Conditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionFalse,
			Reason:             string(gatewayv1.GatewayReasonProgrammed),
			Message:            err.Error(),
			ObservedGeneration: gw.Generation,
			LastTransitionTime: metav1.Now(),
		})
	} else {
		newGw.Status.Conditions, _ = UpdateConditionIfChanged(newGw.Status.Conditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionTrue,
			Reason:             string(gatewayv1.GatewayReasonProgrammed),
			ObservedGeneration: gw.Generation,
			LastTransitionTime: metav1.Now(),
		})
	}

	newGw.Status.Listeners = lisStatus

	_, err = c.gwClient.GatewayV1().Gateways(newGw.Namespace).UpdateStatus(context.Background(), newGw, metav1.UpdateOptions{})
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

// mergeEnvoyResources merges resources generated for a specific route (source)
// into the main resource map (target).
func mergeEnvoyResources(target map[resourcev3.Type][]envoyproxytypes.Resource, source map[resourcev3.Type][]interface{}) {
	for resType, resList := range source {
		if _, ok := target[resType]; !ok {
			target[resType] = []envoyproxytypes.Resource{}
		}
		// Convert []interface{} to []envoyproxytypes.Resource
		for _, res := range resList {
			if envoyRes, ok := res.(envoyproxytypes.Resource); ok {
				target[resType] = append(target[resType], envoyRes)
			} else {
				klog.Warningf("Translated resource is not of type envoyproxytypes.Resource: %T", res)
			}
		}
	}
}

// hostnameMatches checks if a route hostname matches a listener hostname according to Gateway API rules.
func hostnameMatches(listenerHostname, routeHostname gatewayv1.Hostname) bool {
	lh := string(listenerHostname)
	rh := string(routeHostname)

	// Exact match
	if lh == rh {
		return true
	}

	// Wildcard listener
	if strings.HasPrefix(lh, "*.") {
		listenerDomain := lh[1:] // .example.com
		if strings.HasPrefix(rh, "*.") {
			// Both are wildcards, check if domains suffix match each other
			routeDomain := rh[1:]
			return strings.HasSuffix(listenerDomain, routeDomain) || strings.HasSuffix(routeDomain, listenerDomain)
		}
		// Route is not wildcard, check if it's a suffix and not the domain itself (e.g., *.com does not match com)
		return strings.HasSuffix(rh, listenerDomain) && rh != listenerDomain[1:]
	}

	// Wildcard route
	if strings.HasPrefix(rh, "*.") {
		// Listener is not wildcard, check if it's a suffix and not the domain itself
		routeDomain := rh[1:] // .example.com
		return strings.HasSuffix(lh, routeDomain) && lh != routeDomain[1:]
	}

	// No wildcards involved, and not an exact match
	return false
}

// hostnamesIntersect checks if any route hostname intersects with the listener hostname.
func hostnamesIntersect(listenerHostname *gatewayv1.Hostname, routeHostnames []gatewayv1.Hostname) bool {
	// If the route specifies no hostnames, it implicitly matches any listener hostname.
	if len(routeHostnames) == 0 {
		return true
	}

	// If the listener specifies no hostname, it implicitly matches any route hostname.
	if listenerHostname == nil || *listenerHostname == "" {
		return true
	}

	lh := *listenerHostname
	for _, rh := range routeHostnames {
		if hostnameMatches(lh, rh) {
			return true // Found an intersection
		}
	}

	// No intersection found
	return false
}

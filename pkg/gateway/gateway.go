package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	envoyproxytypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func (c *Controller) syncGateway(ctx context.Context, key string) error {
	startTime := time.Now()
	defer func() {
		klog.V(2).Infof("Finished syncing gateway %q (%v)", key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	gw, err := c.gatewayLister.Gateways(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		klog.V(2).Infof("Gateway %s has been deleted, cleaning up resources", key)
		return c.deleteGatewayResources(ctx, name, namespace)
	}
	if err != nil {
		return fmt.Errorf("failed to get gateway %s: %w", key, err)
	}

	if gw.Spec.GatewayClassName != GWClassName {
		klog.V(2).Infof("Gateway %s is not for this controller, ignoring", key)
		return nil
	}

	containerName := gatewayName(c.clusterName, namespace, name)
	klog.Infof("Syncing Gateway %s, container %s", key, containerName)

	err = c.ensureGatewayContainer(ctx, gw)
	if err != nil {
		return fmt.Errorf("failed to ensure gateway container %s: %w", containerName, err)
	}

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

	// Get the desired state
	envoyResources, listenerStatuses, httpRouteStatuses, grpcRouteStatuses := c.buildEnvoyResourcesForGateway(newGw)

	// Apply the desired state to the data plane (Envoy).
	newGw.Status.Listeners = listenerStatuses
	err = c.UpdateXDSServer(ctx, containerName, envoyResources)

	// Calculate and set the Gateway's own status conditions based on the build results.
	setGatewayConditions(newGw, listenerStatuses, err)

	if !reflect.DeepEqual(gw.Status, newGw.Status) {
		_, err := c.gwClient.GatewayV1().Gateways(newGw.Namespace).UpdateStatus(ctx, newGw, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("Failed to update gateway status: %v", err)
			return err
		}
	}
	return c.updateRouteStatuses(ctx, httpRouteStatuses, grpcRouteStatuses)
}

// Main State Calculation Function
func (c *Controller) buildEnvoyResourcesForGateway(gateway *gatewayv1.Gateway) (
	map[resourcev3.Type][]envoyproxytypes.Resource,
	[]gatewayv1.ListenerStatus,
	map[types.NamespacedName]gatewayv1.RouteParentStatus, // HTTPRoutes
	map[types.NamespacedName]gatewayv1.RouteParentStatus, // GRPCRoutes
) {
	envoyRoutes := []envoyproxytypes.Resource{}
	envoyClusters := make(map[string]envoyproxytypes.Resource)
	allListenerStatuses := make(map[gatewayv1.SectionName]gatewayv1.ListenerStatus)
	httpRouteStatuses := make(map[types.NamespacedName]gatewayv1.RouteParentStatus)
	grpcRouteStatuses := make(map[types.NamespacedName]gatewayv1.RouteParentStatus)

	// Aggregate Listeners by Port
	listenersByPort := make(map[gatewayv1.PortNumber][]gatewayv1.Listener)
	for _, listener := range gateway.Spec.Listeners {
		listenersByPort[listener.Port] = append(listenersByPort[listener.Port], listener)
	}

	// validate listeners that may reuse the same port
	conflictedListenerConditions := c.validateListeners(gateway)

	finalEnvoyListeners := []envoyproxytypes.Resource{}
	// Process Listeners by Port
	for port, listeners := range listenersByPort {
		// This slice will hold the filter chains.
		var filterChains []*listenerv3.FilterChain
		// Prepare to collect ALL virtual hosts for this port into a single list.
		virtualHostsForPort := make(map[string]*routev3.VirtualHost)
		routeName := fmt.Sprintf("route-%d", port)

		// All these listeners have the same port
		for _, listener := range listeners {
			var attachedRoutes int32
			listenerStatus := gatewayv1.ListenerStatus{
				Name:           gatewayv1.SectionName(listener.Name),
				SupportedKinds: []gatewayv1.RouteGroupKind{},
				Conditions:     []metav1.Condition{},
				AttachedRoutes: 0,
			}
			supportedKinds, allKindsValid := getSupportedKinds(listener)
			listenerStatus.SupportedKinds = supportedKinds

			if !allKindsValid {
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionFalse,
					Reason:             string(gatewayv1.ListenerReasonInvalidRouteKinds),
					Message:            "Invalid route kinds specified in allowedRoutes",
					ObservedGeneration: gateway.Generation,
				})
				allListenerStatuses[listener.Name] = listenerStatus
				continue // Stop processing this invalid listener
			}

			if conflictCondition, isConflicted := conflictedListenerConditions[listener.Name]; isConflicted {
				// This listener is conflicted. Set its status and skip it.
				meta.SetStatusCondition(&listenerStatus.Conditions, conflictCondition)
				allListenerStatuses[listener.Name] = listenerStatus
				continue // DO NOT generate Envoy config for this listener
			}

			// This map is temporary for just this listener's virtual hosts,
			// which are determined by the hostnames on the attached routes.
			virtualHostsForListener := make(map[string]*routev3.VirtualHost)

			switch listener.Protocol {
			case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
				// Process HTTPRoutes
				httpRoutes := c.getHTTPRoutesForListener(gateway, listener)
				for _, httpRoute := range httpRoutes {
					key := types.NamespacedName{Namespace: httpRoute.Namespace, Name: httpRoute.Name}
					if _, ok := httpRouteStatuses[key]; ok {
						continue
					}
					// Validate that the route's parentRef is a valid reference to this specific listener.
					if !isValidParentRef(gateway, listener, httpRoute) {
						// This route references the Gateway but has an invalid SectionName for THIS listener.
						// Set the "Accepted: False" status and stop processing it further for this listener.
						parentStatus := gatewayv1.RouteParentStatus{
							ParentRef:      gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name), Namespace: ptr.To(gatewayv1.Namespace(gateway.Namespace))},
							ControllerName: controllerName,
							Conditions: []metav1.Condition{{
								Type:               string(gatewayv1.RouteConditionAccepted),
								Status:             metav1.ConditionFalse,
								Reason:             string(gatewayv1.RouteReasonNoMatchingParent),
								Message:            "The referenced SectionName does not match this listener's name.",
								ObservedGeneration: httpRoute.Generation,
								LastTransitionTime: metav1.Now(),
							}},
						}
						httpRouteStatuses[key] = parentStatus
						continue // Stop processing this invalid route.
					}

					if !isRouteAllowed(gateway, listener, httpRoute, c.namespaceLister) {
						// This route references the Gateway but has an invalid SectionName for THIS listener.
						// Set the "Accepted: False" status and stop processing it further for this listener.
						parentStatus := gatewayv1.RouteParentStatus{
							ParentRef:      gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name), Namespace: ptr.To(gatewayv1.Namespace(gateway.Namespace))},
							ControllerName: controllerName,
							Conditions: []metav1.Condition{
								{
									Type:               string(gatewayv1.RouteConditionResolvedRefs),
									Status:             metav1.ConditionTrue,
									Reason:             string(gatewayv1.RouteReasonResolvedRefs),
									Message:            "The referenced gateway is resolved",
									ObservedGeneration: httpRoute.Generation,
									LastTransitionTime: metav1.Now(),
								},
								{
									Type:               string(gatewayv1.RouteConditionAccepted),
									Status:             metav1.ConditionFalse,
									Reason:             string(gatewayv1.RouteReasonNotAllowedByListeners),
									Message:            "The referenced gateway is not allowed",
									ObservedGeneration: httpRoute.Generation,
									LastTransitionTime: metav1.Now(),
								}},
						}
						httpRouteStatuses[key] = parentStatus
						continue // Stop processing this invalid route.
					}
					routes, validBackendRefs, finalCondition := translateHTTPRouteToEnvoyRoutes(httpRoute, c.serviceLister)

					// Create the necessary Envoy Cluster resources from the valid backends.
					for _, backendRef := range validBackendRefs {
						cluster, err := c.translateBackendRefToCluster(httpRoute.Namespace, backendRef)
						if err == nil && cluster != nil {
							if _, exists := envoyClusters[cluster.Name]; !exists {
								envoyClusters[cluster.Name] = cluster
							}
						}
					}

					// Aggregate Envoy routes into VirtualHosts.
					if routes != nil {
						hostnames := getRouteHostnames(httpRoute.Spec.Hostnames, listener)
						for _, hostname := range hostnames {
							vh, ok := virtualHostsForListener[hostname]
							if !ok {
								vh = &routev3.VirtualHost{
									Name:    fmt.Sprintf("%s-%s-%d-%s", gateway.Name, listener.Protocol, port, hostname),
									Domains: []string{hostname},
								}
								virtualHostsForListener[hostname] = vh
							}
							vh.Routes = append(vh.Routes, routes...)
						}
						attachedRoutes++
					}

					if _, ok := httpRouteStatuses[key]; !ok {
						// Store the calculated status for the route.
						parentStatus := gatewayv1.RouteParentStatus{
							ParentRef:      gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name), Namespace: ptr.To(gatewayv1.Namespace(gateway.Namespace))},
							ControllerName: controllerName,
							Conditions: []metav1.Condition{
								finalCondition,
								{
									Type:               string(gatewayv1.RouteConditionAccepted),
									Status:             metav1.ConditionTrue,
									Reason:             string(gatewayv1.RouteReasonAccepted),
									Message:            "Route is accepted",
									ObservedGeneration: httpRoute.Generation,
									LastTransitionTime: metav1.Now(),
								},
							},
						}
						httpRouteStatuses[key] = parentStatus
					}
				}

				// Process GRPCRoutes
				grpcRoutes := c.getGRPCRoutesForListener(gateway, listener)
				for _, grpcRoute := range grpcRoutes {
					key := types.NamespacedName{Namespace: grpcRoute.Namespace, Name: grpcRoute.Name}
					routes, validBackendRefs, finalCondition := translateGRPCRouteToEnvoyRoutes(grpcRoute, c.serviceLister)

					// Create clusters for valid GRPCRoute backends.
					for _, backendRef := range validBackendRefs {
						cluster, err := c.translateBackendRefToCluster(grpcRoute.Namespace, backendRef)
						if err == nil && cluster != nil {
							if _, exists := envoyClusters[cluster.Name]; !exists {
								envoyClusters[cluster.Name] = cluster
							}
						}
					}

					if routes != nil {
						hostnames := getRouteHostnames(grpcRoute.Spec.Hostnames, listener)
						for _, hostname := range hostnames {
							vh, ok := virtualHostsForListener[hostname]
							if !ok {
								vh = &routev3.VirtualHost{
									Name:    fmt.Sprintf("%s-%s-%d-%s", gateway.Name, listener.Protocol, port, hostname),
									Domains: []string{hostname},
								}
								virtualHostsForListener[hostname] = vh
							}
							vh.Routes = append(vh.Routes, routes...)
						}
						attachedRoutes++
					}

					parentStatus := gatewayv1.RouteParentStatus{
						ParentRef:      gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gateway.Name), Namespace: ptr.To(gatewayv1.Namespace(gateway.Namespace))},
						ControllerName: controllerName,
						Conditions:     []metav1.Condition{finalCondition},
					}
					grpcRouteStatuses[key] = parentStatus
				}

			default:
				klog.Warningf("Unsupported listener protocol for route processing: %s", listener.Protocol)
			}

			vhSlice := make([]*routev3.VirtualHost, 0, len(virtualHostsForListener))
			for _, vh := range virtualHostsForListener {
				vhSlice = append(vhSlice, vh)
				virtualHostsForPort[vh.Name] = vh
			}

			filterChain, err := c.translateListenerToFilterChain(gateway, listener, vhSlice, routeName)
			if err != nil {
				// If translation fails, a reference is unresolved. Set both conditions to False.
				klog.Errorf("Error translating listener %s to filter chain: %v", listener.Name, err)
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionFalse,
					Reason:             string(gatewayv1.ListenerReasonInvalidCertificateRef),
					Message:            fmt.Sprintf("Failed to resolve references: %v", err),
					ObservedGeneration: gateway.Generation,
				})
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionFalse,
					Reason:             string(gatewayv1.ListenerReasonInvalid),
					Message:            fmt.Sprintf("Failed to program listener: %v", err),
					ObservedGeneration: gateway.Generation,
				})
			} else {
				// Only if ALL checks pass, set the conditions to True.
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
					Message:            "All references resolved",
					ObservedGeneration: gateway.Generation,
				})
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener is programmed",
					ObservedGeneration: gateway.Generation,
				})

				filterChains = append(filterChains, filterChain)
			}

			listenerStatus.AttachedRoutes = attachedRoutes
			meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionAccepted),
				Status:             metav1.ConditionTrue,
				Reason:             string(gatewayv1.ListenerReasonAccepted),
				Message:            "Listener is valid",
				ObservedGeneration: gateway.Generation,
			})
			allListenerStatuses[listener.Name] = listenerStatus
		}

		// This happens AFTER all routes for this port have been collected.
		// Envoy processes the list of routes within a VirtualHost sequentially.
		// The Gateway API specification requires that controllers order routes from most specific to least specific.
		for _, vh := range virtualHostsForPort {
			sortRoutes(vh.Routes)
		}

		allVirtualHosts := make([]*routev3.VirtualHost, 0, len(virtualHostsForPort))
		for _, vh := range virtualHostsForPort {
			allVirtualHosts = append(allVirtualHosts, vh)
		}

		// now aggregate all the listeners on the same port
		routeConfig := &routev3.RouteConfiguration{
			Name:         routeName,
			VirtualHosts: allVirtualHosts,
		}
		envoyRoutes = append(envoyRoutes, routeConfig)

		if len(filterChains) > 0 {
			envoyListener := &listenerv3.Listener{
				Name:            fmt.Sprintf("listener-%d", port),
				Address:         createEnvoyAddress(uint32(port)),
				FilterChains:    filterChains,
				ListenerFilters: createListenerFilters(),
			}
			// If this is plain HTTP, we must now create exactly ONE default filter chain.
			// Use first listener as a template
			// For HTTPS, we create one filter chain per listener because they have unique
			// SNI matches and TLS settings.
			if listeners[0].Protocol == gatewayv1.HTTPProtocolType {
				filterChain, _ := c.translateListenerToFilterChain(gateway, listeners[0], allVirtualHosts, routeName)
				envoyListener.FilterChains = []*listenerv3.FilterChain{filterChain}
			}
			finalEnvoyListeners = append(finalEnvoyListeners, envoyListener)
		}
	}

	clustersSlice := make([]envoyproxytypes.Resource, 0, len(envoyClusters))
	for _, cluster := range envoyClusters {
		clustersSlice = append(clustersSlice, cluster)
	}

	orderedStatuses := make([]gatewayv1.ListenerStatus, len(gateway.Spec.Listeners))
	for i, listener := range gateway.Spec.Listeners {
		orderedStatuses[i] = allListenerStatuses[listener.Name]
	}

	return map[resourcev3.Type][]envoyproxytypes.Resource{
			resourcev3.ListenerType: finalEnvoyListeners,
			resourcev3.RouteType:    envoyRoutes,
			resourcev3.ClusterType:  clustersSlice,
		}, orderedStatuses,
		httpRouteStatuses,
		grpcRouteStatuses
}

func getSupportedKinds(listener gatewayv1.Listener) ([]gatewayv1.RouteGroupKind, bool) {
	supportedKinds := []gatewayv1.RouteGroupKind{}
	allKindsValid := true
	groupName := gatewayv1.Group(gatewayv1.GroupName)

	if listener.AllowedRoutes != nil && len(listener.AllowedRoutes.Kinds) > 0 {
		for _, kind := range listener.AllowedRoutes.Kinds {
			if (kind.Group == nil || *kind.Group == groupName) && (kind.Kind == "HTTPRoute" || kind.Kind == "GRPCRoute") {
				supportedKinds = append(supportedKinds, gatewayv1.RouteGroupKind{
					Group: &groupName,
					Kind:  kind.Kind,
				})
			} else {
				allKindsValid = false
			}
		}
	} else if listener.Protocol == gatewayv1.HTTPProtocolType || listener.Protocol == gatewayv1.HTTPSProtocolType {
		supportedKinds = append(supportedKinds,
			gatewayv1.RouteGroupKind{
				Group: &groupName,
				Kind:  "HTTPRoute",
			},
			gatewayv1.RouteGroupKind{
				Group: &groupName,
				Kind:  "GRPCRoute",
			})
	}

	return supportedKinds, allKindsValid
}
func (c *Controller) updateRouteStatuses(
	ctx context.Context,
	httpRouteStatuses map[types.NamespacedName]gatewayv1.RouteParentStatus,
	grpcRouteStatuses map[types.NamespacedName]gatewayv1.RouteParentStatus,
) error {
	var errGroup []error

	// --- Process HTTPRoutes ---
	for key, desiredParentStatus := range httpRouteStatuses {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// 1. GET the latest version of the route from the cache.
			originalRoute, err := c.httprouteLister.HTTPRoutes(key.Namespace).Get(key.Name)
			if apierrors.IsNotFound(err) {
				// Route has been deleted, nothing to do.
				return nil
			} else if err != nil {
				return err
			}

			// 2. Create a mutable copy to work with.
			routeToUpdate := originalRoute.DeepCopy()

			// 3. Find the existing parent status for our Gateway, or append the new one.
			var found bool
			for i := range routeToUpdate.Status.Parents {
				if routeToUpdate.Status.Parents[i].ParentRef.Name == desiredParentStatus.ParentRef.Name &&
					*routeToUpdate.Status.Parents[i].ParentRef.Namespace == *desiredParentStatus.ParentRef.Namespace {
					// Found it, so replace it with our new desired status.
					routeToUpdate.Status.Parents[i] = desiredParentStatus
					found = true
					break
				}
			}
			if !found {
				routeToUpdate.Status.Parents = append(routeToUpdate.Status.Parents, desiredParentStatus)
			}

			// 4. Only make an API call if the status has actually changed.
			if !reflect.DeepEqual(originalRoute.Status, routeToUpdate.Status) {
				_, updateErr := c.gwClient.GatewayV1().HTTPRoutes(routeToUpdate.Namespace).UpdateStatus(ctx, routeToUpdate, metav1.UpdateOptions{})
				return updateErr
			}

			// Status is already up-to-date.
			return nil
		})

		if err != nil {
			errGroup = append(errGroup, fmt.Errorf("failed to update status for HTTPRoute %s: %w", key, err))
		}
	}

	// --- Process GRPCRoutes (repeat the same logic) ---
	for key, desiredParentStatus := range grpcRouteStatuses {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// 1. GET the latest version of the route from the cache.
			originalRoute, err := c.grpcrouteLister.GRPCRoutes(key.Namespace).Get(key.Name)
			if apierrors.IsNotFound(err) {
				// Route has been deleted, nothing to do.
				return nil
			}
			if err != nil {
				return err
			}

			// 2. Create a mutable copy to work with.
			routeToUpdate := originalRoute.DeepCopy()

			// The route is attached, so it is always considered "Accepted".
			// The ResolvedRefs condition was already calculated in the build phase.
			acceptedCond := metav1.Condition{
				Type:               string(gatewayv1.RouteConditionAccepted),
				Status:             metav1.ConditionTrue,
				Reason:             string(gatewayv1.RouteReasonAccepted),
				Message:            "Route is accepted",
				ObservedGeneration: routeToUpdate.Generation,
				LastTransitionTime: metav1.Now(),
			}
			meta.SetStatusCondition(&desiredParentStatus.Conditions, acceptedCond)

			// 3. Find the existing parent status for our Gateway, or append the new one.
			var found bool
			for i := range routeToUpdate.Status.Parents {
				if routeToUpdate.Status.Parents[i].ParentRef.Name == desiredParentStatus.ParentRef.Name &&
					*routeToUpdate.Status.Parents[i].ParentRef.Namespace == *desiredParentStatus.ParentRef.Namespace {
					// Found it, so replace it with our new desired status.
					routeToUpdate.Status.Parents[i] = desiredParentStatus
					found = true
					break
				}
			}
			if !found {
				routeToUpdate.Status.Parents = append(routeToUpdate.Status.Parents, desiredParentStatus)
			}

			// 4. Only make an API call if the status has actually changed.
			if !reflect.DeepEqual(originalRoute.Status, routeToUpdate.Status) {
				_, updateErr := c.gwClient.GatewayV1().GRPCRoutes(routeToUpdate.Namespace).UpdateStatus(ctx, routeToUpdate, metav1.UpdateOptions{})
				return updateErr
			}

			// Status is already up-to-date.
			return nil
		})

		if err != nil {
			errGroup = append(errGroup, fmt.Errorf("failed to update status for HTTPRoute %s: %w", key, err))
		}
	}

	return errors.Join(errGroup...)
}

func (c *Controller) getHTTPRoutesForListener(gw *gatewayv1.Gateway, listener gatewayv1.Listener) []*gatewayv1.HTTPRoute {
	var matchingRoutes []*gatewayv1.HTTPRoute
	httpRoutes, err := c.httprouteLister.List(labels.Everything())
	if err != nil {
		klog.Infof("failed to list HTTPRoutes: %v", err)
		return matchingRoutes
	}

	for _, route := range httpRoutes {
		if isRouteReferenced(gw, listener, route) {
			matchingRoutes = append(matchingRoutes, route)
		}
	}
	return matchingRoutes
}

func (c *Controller) getGRPCRoutesForListener(gw *gatewayv1.Gateway, listener gatewayv1.Listener) []*gatewayv1.GRPCRoute {
	var matchingRoutes []*gatewayv1.GRPCRoute
	grpcRoutes, err := c.grpcrouteLister.List(labels.Everything())
	if err != nil {
		klog.Infof("failed to list GRPCRoutes: %v", err)
		return matchingRoutes
	}
	for _, route := range grpcRoutes {
		if isRouteReferenced(gw, listener, route) && isRouteAllowed(gw, listener, route, c.namespaceLister) {
			matchingRoutes = append(matchingRoutes, route)
		}
	}
	return matchingRoutes
}

func (c *Controller) translateBackendRefToCluster(defaultNamespace string, backendRef gatewayv1.BackendRef) (*clusterv3.Cluster, error) {
	ns := defaultNamespace
	if backendRef.Namespace != nil {
		ns = string(*backendRef.Namespace)
	}
	service, err := c.serviceLister.Services(ns).Get(string(backendRef.Name))
	if err != nil {
		return nil, fmt.Errorf("could not find service %s/%s: %w", ns, backendRef.Name, err)
	}

	clusterName, err := backendRefToClusterName(defaultNamespace, backendRef)
	if err != nil {
		return nil, err
	}

	// Create the base cluster configuration.
	cluster := &clusterv3.Cluster{
		Name:           clusterName,
		ConnectTimeout: durationpb.New(5 * time.Second),
	}

	if service.Spec.ClusterIP == corev1.ClusterIPNone {
		// Use STRICT_DNS discovery and the service's FQDN.
		cluster.ClusterDiscoveryType = &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STRICT_DNS}
		// Construct the FQDN for the service.
		fqdn := fmt.Sprintf("%s.%s.svc.cluster.local", service.Name, service.Namespace)
		// Get the port of the endpoints.
		targetPort := 0
		for _, port := range service.Spec.Ports {
			if port.Port == int32(*backendRef.Port) {
				targetPort = int(port.TargetPort.IntVal)
				break
			}
		}
		if targetPort == 0 {
			return nil, fmt.Errorf("could not find port %d in service %s/%s", *backendRef.Port, service.Namespace, service.Name)
		}
		cluster.LoadAssignment = createClusterLoadAssignment(clusterName, fqdn, uint32(targetPort))
	} else {
		// Use STATIC discovery with the service's ClusterIP.
		cluster.ClusterDiscoveryType = &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC}
		cluster.LoadAssignment = createClusterLoadAssignment(clusterName, service.Spec.ClusterIP, uint32(*backendRef.Port))
	}

	return cluster, nil
}

func (c *Controller) deleteGatewayResources(ctx context.Context, name, namespace string) error {
	klog.Infof("Deleting resources for Gateway: %s/%s", namespace, name)
	containerName := gatewayName(c.clusterName, namespace, name)

	c.xdsVersion.Add(1)
	version := fmt.Sprintf("%d", c.xdsVersion.Load())

	snapshot, err := cachev3.NewSnapshot(version, map[resourcev3.Type][]envoyproxytypes.Resource{
		resourcev3.ListenerType: {},
		resourcev3.RouteType:    {},
		resourcev3.ClusterType:  {},
		resourcev3.EndpointType: {},
	})
	if err != nil {
		return fmt.Errorf("failed to create empty snapshot for deleted gateway %s: %w", name, err)
	}
	if err := c.xdscache.SetSnapshot(ctx, containerName, snapshot); err != nil {
		return fmt.Errorf("failed to set empty snapshot for deleted gateway %s: %w", name, err)
	}

	if err := container.Delete(containerName); err != nil {
		return fmt.Errorf("failed to delete container for gateway %s: %v", name, err)
	}

	klog.Infof("Successfully cleared resources for deleted Gateway: %s", name)
	return nil
}

func createClusterLoadAssignment(clusterName, serviceHost string, servicePort uint32) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{
				LbEndpoints: []*endpointv3.LbEndpoint{
					{
						HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
							Endpoint: &endpointv3.Endpoint{
								Address: &corev3.Address{
									Address: &corev3.Address_SocketAddress{
										SocketAddress: &corev3.SocketAddress{
											Address: serviceHost,
											PortSpecifier: &corev3.SocketAddress_PortValue{
												PortValue: servicePort,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// getRouteHostnames determines the effective hostnames for a route.
func getRouteHostnames(routeHostnames []gatewayv1.Hostname, listener gatewayv1.Listener) []string {
	if len(routeHostnames) > 0 {
		hostnames := make([]string, len(routeHostnames))
		for i, h := range routeHostnames {
			hostnames[i] = string(h)
		}
		return hostnames
	}
	if listener.Hostname != nil && *listener.Hostname != "" {
		return []string{string(*listener.Hostname)}
	}
	return []string{"*"}
}

// setGatewayConditions calculates and sets the final status conditions for the Gateway
// based on the results of the reconciliation loop.
func setGatewayConditions(newGw *gatewayv1.Gateway, listenerStatuses []gatewayv1.ListenerStatus, err error) {
	programmedCondition := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		ObservedGeneration: newGw.Generation,
	}
	if err != nil {
		// If the Envoy update fails, the Gateway is not programmed.
		programmedCondition.Status = metav1.ConditionFalse
		programmedCondition.Reason = "ReconciliationError"
		programmedCondition.Message = fmt.Sprintf("Failed to program envoy config: %s", err.Error())
	} else {
		// If the Envoy update succeeds, check if all individual listeners were programmed.
		listenersProgrammed := 0
		for _, listenerStatus := range listenerStatuses {
			if meta.IsStatusConditionTrue(listenerStatus.Conditions, string(gatewayv1.ListenerConditionProgrammed)) {
				listenersProgrammed++
			}
		}

		if listenersProgrammed == len(listenerStatuses) {
			// The Gateway is only fully programmed if all listeners are programmed.
			programmedCondition.Status = metav1.ConditionTrue
			programmedCondition.Reason = string(gatewayv1.GatewayReasonProgrammed)
			programmedCondition.Message = "Envoy configuration updated successfully"
		} else {
			// If any listener failed, the Gateway as a whole is not fully programmed.
			programmedCondition.Status = metav1.ConditionFalse
			programmedCondition.Reason = "ListenersNotProgrammed"
			programmedCondition.Message = fmt.Sprintf("%d out of %d listeners failed to be programmed", listenersProgrammed, len(listenerStatuses))
		}
	}
	meta.SetStatusCondition(&newGw.Status.Conditions, programmedCondition)

	meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gatewayv1.GatewayReasonAccepted),
		Message:            "Gateway is accepted",
		ObservedGeneration: newGw.Generation,
	})
}

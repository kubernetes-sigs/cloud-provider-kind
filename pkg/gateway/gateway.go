package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	envoyproxytypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/types/known/durationpb"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var (
	CloudProviderSupportedKinds = sets.New[gatewayv1.Kind](
		"HTTPRoute",
		// "GRPCRoute",
	)
)

// isOurGateway returns true when the Gateway's GatewayClass is managed by this
// controller (i.e. its spec.controllerName matches ours). Using the controller
// name rather than the hard-coded GWClassName constant means the controller
// also handles Gateways that reference conformance-test-created classes (which
// carry our controller name but a different class name).
func (c *Controller) isOurGateway(gw *gatewayv1.Gateway) bool {
	gwc, err := c.gatewayClassLister.Get(string(gw.Spec.GatewayClassName))
	if err != nil {
		return false
	}
	return gwc.Spec.ControllerName == controllerName
}

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

	if !c.isOurGateway(gw) {
		klog.V(2).Infof("Gateway %s is not for this controller, ignoring", key)
		return nil
	}

	newGw := gw.DeepCopy()

	// Get the desired state
	envoyResources, listenerStatuses, httpRouteStatuses, grpcRouteStatuses := c.buildEnvoyResourcesForGateway(newGw)
	newGw.Status.Listeners = listenerStatuses
	setAcceptedCondition(newGw)

	var xdsErr error
	if meta.IsStatusConditionTrue(newGw.Status.Conditions, string(gatewayv1.GatewayConditionAccepted)) {
		containerName := gatewayName(c.clusterName, namespace, name)
		klog.Infof("Syncing Gateway %s, container %s", key, containerName)

		err = c.ensureGatewayContainer(ctx, gw)
		if err != nil {
			return fmt.Errorf("failed to ensure gateway container %s: %w", containerName, err)
		}

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

		// Apply the desired state to the data plane (Envoy).
		xdsErr = c.UpdateXDSServer(ctx, containerName, envoyResources)

		// forward traffic from the host on Mac and Windows
		if c.tunnelManager != nil {
			err := c.tunnelManager.SetupTunnels(containerName)
			if err != nil {
				klog.Errorf("failed to set up tunnels for gateway %s: %v", key, err)
			}
		}
	}

	setProgrammedCondition(newGw, xdsErr)

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
	map[types.NamespacedName][]gatewayv1.RouteParentStatus, // HTTPRoutes
	map[types.NamespacedName][]gatewayv1.RouteParentStatus, // GRPCRoutes
) {

	httpRouteStatuses := make(map[types.NamespacedName][]gatewayv1.RouteParentStatus)
	grpcRouteStatuses := make(map[types.NamespacedName][]gatewayv1.RouteParentStatus)
	routesByListener := make(map[gatewayv1.SectionName][]*gatewayv1.HTTPRoute)

	// Validate all HTTPRoutes against this Gateway
	allHTTPRoutesForGateway := c.getHTTPRoutesForGateway(gateway)
	for _, httpRoute := range allHTTPRoutesForGateway {
		key := types.NamespacedName{Name: httpRoute.Name, Namespace: httpRoute.Namespace}
		parentStatuses, acceptingListeners := c.validateHTTPRoute(gateway, httpRoute)

		// Store the definitive status for the route.
		if len(parentStatuses) > 0 {
			httpRouteStatuses[key] = parentStatuses
		}
		// If the route was accepted, associate it with the listeners that accepted it.
		if len(acceptingListeners) > 0 {
			// Associate the accepted route with the listeners that will handle it.
			// Use a set to prevent adding a route multiple times to the same listener.
			processedListeners := make(map[gatewayv1.SectionName]bool)
			for _, listener := range acceptingListeners {
				if _, ok := processedListeners[listener.Name]; !ok {
					routesByListener[listener.Name] = append(routesByListener[listener.Name], httpRoute)
					processedListeners[listener.Name] = true
				}
			}
		}
	}

	// Build Envoy config using only the pre-validated and accepted routes
	envoyRoutes := []envoyproxytypes.Resource{}
	envoyClusters := make(map[string]envoyproxytypes.Resource)
	allListenerStatuses := make(map[gatewayv1.SectionName]gatewayv1.ListenerStatus)
	// Aggregate Listeners by Port
	listenersByPort := make(map[gatewayv1.PortNumber][]gatewayv1.Listener)
	for _, listener := range gateway.Spec.Listeners {
		listenersByPort[listener.Port] = append(listenersByPort[listener.Port], listener)
	}

	// validate listeners that may reuse the same port
	listenerValidationConditions := c.validateListeners(gateway)

	finalEnvoyListeners := []envoyproxytypes.Resource{}
	// Process Listeners by Port
	for port, listeners := range listenersByPort {
		// Prepare to collect ALL virtual hosts for this port into a single list.
		virtualHostsForPort := make(map[string]*routev3.VirtualHost)
		routeName := fmt.Sprintf("route-%d", port)
		// All listeners on a port share one route configuration, so an HTTP
		// filter needed by any of their routes has to be installed on every
		// filter chain of the port. Filter chains are therefore only built once
		// every route on the port has been translated.
		extraHTTPFilters := make(map[string]*hcm.HttpFilter)
		var listenersToProgram []gatewayv1.Listener

		// All these listeners have the same port
		for _, listener := range listeners {
			var attachedRoutes int32
			listenerStatus := gatewayv1.ListenerStatus{
				Name:           gatewayv1.SectionName(listener.Name),
				SupportedKinds: []gatewayv1.RouteGroupKind{},
				Conditions:     []metav1.Condition{},
				AttachedRoutes: 0,
			}
			// Find old status to preserve condition timestamps
			for _, oldStatus := range gateway.Status.Listeners {
				if oldStatus.Name == listener.Name {
					listenerStatus.Conditions = oldStatus.Conditions // Copy old conditions
					break
				}
			}

			// Apply pre-calculated validation conditions
			if validationConds, ok := listenerValidationConditions[listener.Name]; ok {
				for _, cond := range validationConds {
					meta.SetStatusCondition(&listenerStatus.Conditions, cond)
				}
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

			isConflicted := meta.IsStatusConditionTrue(listenerStatus.Conditions, string(gatewayv1.ListenerConditionConflicted))
			// If the listener is conflicted set its status and skip Envoy config generation.
			if isConflicted {
				allListenerStatuses[listener.Name] = listenerStatus
				continue
			}

			// If there are not references issues then set it to tru
			if !meta.IsStatusConditionFalse(listenerStatus.Conditions, string(gatewayv1.ListenerConditionResolvedRefs)) {
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
					Message:            "All references resolved",
					ObservedGeneration: gateway.Generation,
				})
			}

			switch listener.Protocol {
			case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
				// Process HTTPRoutes
				// Get the routes that were pre-validated for this specific listener.
				for _, httpRoute := range routesByListener[listener.Name] {
					translation := translateHTTPRouteToEnvoyRoutes(httpRoute, c.serviceLister, c.referenceGrantLister)

					key := types.NamespacedName{Name: httpRoute.Name, Namespace: httpRoute.Namespace}
					currentParentStatuses := httpRouteStatuses[key]
					for i := range currentParentStatuses {
						if translation.notAccepted != nil {
							meta.SetStatusCondition(&currentParentStatuses[i].Conditions, *translation.notAccepted)
						}
						if meta.IsStatusConditionTrue(currentParentStatuses[i].Conditions, string(gatewayv1.RouteConditionAccepted)) {
							if translation.resolvedRefsFailure != nil {
								meta.SetStatusCondition(&currentParentStatuses[i].Conditions, *translation.resolvedRefsFailure)
							} else {
								meta.SetStatusCondition(&currentParentStatuses[i].Conditions, createResolvedCondition(httpRoute.Generation))
							}
							if translation.partiallyInvalid != nil {
								meta.SetStatusCondition(&currentParentStatuses[i].Conditions, *translation.partiallyInvalid)
							}
						}
					}
					httpRouteStatuses[key] = currentParentStatuses

					// Create the necessary Envoy Cluster resources from the valid backends.
					for _, backendRef := range translation.backendRefs {
						cluster, err := translateBackendRefToCluster(c.serviceLister, httpRoute.Namespace, backendRef)
						if err == nil && cluster != nil {
							if _, exists := envoyClusters[cluster.Name]; !exists {
								envoyClusters[cluster.Name] = cluster
							}
						}
					}

					// Resources the translated routes reference through their
					// typed_per_filter_config, such as GEP-1494 authorization servers.
					for name, resources := range translation.externalAuth {
						extraHTTPFilters[name] = resources.httpFilter
						if _, exists := envoyClusters[resources.cluster.Name]; !exists {
							envoyClusters[resources.cluster.Name] = resources.cluster
						}
					}

					// Aggregate Envoy routes into VirtualHosts.
					if translation.routes != nil {
						attachedRoutes++
						// Get the domain for this listener's VirtualHost.
						vhostDomains := getIntersectingHostnames(listener, httpRoute.Spec.Hostnames)
						for _, domain := range vhostDomains {
							vh, ok := virtualHostsForPort[domain]
							if !ok {
								vh = &routev3.VirtualHost{
									Name:    fmt.Sprintf("%s-vh-%d-%s", gateway.Name, port, domain),
									Domains: []string{domain},
								}
								virtualHostsForPort[domain] = vh
							}
							vh.Routes = append(vh.Routes, translation.routes...)
							klog.V(4).Infof("created VirtualHost %s for listener %s with domain %s", vh.Name, listener.
								Name, domain)
							if klog.V(4).Enabled() {
								for _, route := range translation.routes {
									klog.Infof("adding route %s to VirtualHost %s", route.Name, vh.Name)
								}
							}
						}
					}
				}

				// TODO: Process GRPCRoutes

			default:
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionFalse,
					Reason:             string(gatewayv1.ListenerReasonUnsupportedProtocol),
					Message:            fmt.Sprintf("Protocol %q is not supported; supported protocols are HTTP and HTTPS.", listener.Protocol),
					ObservedGeneration: gateway.Generation,
				})
				allListenerStatuses[listener.Name] = listenerStatus
				continue
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
			listenersToProgram = append(listenersToProgram, listener)
		}

		allVirtualHosts := make([]*routev3.VirtualHost, 0, len(virtualHostsForPort))
		for _, vh := range virtualHostsForPort {
			sortRoutes(vh.Routes)
			allVirtualHosts = append(allVirtualHosts, vh)
		}

		// now aggregate all the listeners on the same port
		routeConfig := &routev3.RouteConfiguration{
			Name:                     routeName,
			VirtualHosts:             allVirtualHosts,
			IgnorePortInHostMatching: true, // tricky to figure out thanks to howardjohn
		}
		envoyRoutes = append(envoyRoutes, routeConfig)

		httpFilters := sortedHTTPFilters(extraHTTPFilters)
		var filterChains []*listenerv3.FilterChain
		for _, listener := range listenersToProgram {
			listenerStatus := allListenerStatuses[listener.Name]
			filterChain, err := c.translateListenerToFilterChain(gateway, listener, allVirtualHosts, routeName, httpFilters)
			if err != nil {
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionFalse,
					Reason:             string(gatewayv1.ListenerReasonInvalid),
					Message:            fmt.Sprintf("Failed to program listener: %v", err),
					ObservedGeneration: gateway.Generation,
				})
			} else {
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener is programmed",
					ObservedGeneration: gateway.Generation,
				})

				filterChains = append(filterChains, filterChain)
			}
			allListenerStatuses[listener.Name] = listenerStatus
		}

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
				filterChain, _ := c.translateListenerToFilterChain(gateway, listeners[0], allVirtualHosts, routeName, httpFilters)
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
			if (kind.Group == nil || *kind.Group == groupName) && CloudProviderSupportedKinds.Has(kind.Kind) {
				supportedKinds = append(supportedKinds, gatewayv1.RouteGroupKind{
					Group: &groupName,
					Kind:  kind.Kind,
				})
			} else {
				allKindsValid = false
			}
		}
	} else if listener.Protocol == gatewayv1.HTTPProtocolType || listener.Protocol == gatewayv1.HTTPSProtocolType {
		for _, kind := range CloudProviderSupportedKinds.UnsortedList() {
			supportedKinds = append(supportedKinds,
				gatewayv1.RouteGroupKind{
					Group: &groupName,
					Kind:  kind,
				},
			)
		}
	}

	return supportedKinds, allKindsValid
}

var semanticIgnoreLastTransitionTime = conversion.EqualitiesOrDie(
	func(a, b metav1.Condition) bool {
		a.LastTransitionTime = metav1.Time{}
		b.LastTransitionTime = metav1.Time{}
		return a == b
	},
)

func (c *Controller) updateRouteStatuses(
	ctx context.Context,
	httpRouteStatuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
	grpcRouteStatuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
) error {
	var errGroup []error

	// --- Process HTTPRoutes ---
	for key, desiredParentStatuses := range httpRouteStatuses {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// GET the latest version of the route from the cache.
			originalRoute, err := c.httprouteLister.HTTPRoutes(key.Namespace).Get(key.Name)
			if apierrors.IsNotFound(err) {
				// Route has been deleted, nothing to do.
				return nil
			} else if err != nil {
				return err
			}

			// Create a mutable copy to work with.
			routeToUpdate := originalRoute.DeepCopy()
			routeToUpdate.Status.Parents = desiredParentStatuses

			// Only make an API call if the status has actually changed.
			if !semanticIgnoreLastTransitionTime.DeepEqual(originalRoute.Status, routeToUpdate.Status) {
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

	// TODO: Process GRPCRoutes (repeat the same logic)

	return errors.Join(errGroup...)
}

// getHTTPRoutesForGateway returns all HTTPRoutes that have a ParentRef pointing to the specified Gateway.
func (c *Controller) getHTTPRoutesForGateway(gw *gatewayv1.Gateway) []*gatewayv1.HTTPRoute {
	var matchingRoutes []*gatewayv1.HTTPRoute
	allRoutes, err := c.httprouteLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list HTTPRoutes: %v", err)
		return matchingRoutes
	}

	for _, route := range allRoutes {
		for _, parentRef := range route.Spec.ParentRefs {
			// Check if the ParentRef targets the Gateway, defaulting to the route's namespace.
			refNamespace := route.Namespace
			if parentRef.Namespace != nil {
				refNamespace = string(*parentRef.Namespace)
			}
			if parentRef.Name == gatewayv1.ObjectName(gw.Name) && refNamespace == gw.Namespace {
				matchingRoutes = append(matchingRoutes, route)
				break // Found a matching ref for this gateway, no need to check others.
			}
		}
	}
	return matchingRoutes
}

// validateHTTPRoute is the definitive validation function. It iterates through all
// parentRefs of an HTTPRoute and generates a complete RouteParentStatus for each one
// that targets the specified Gateway. It also returns a slice of all listeners
// that ended up accepting the route.
func (c *Controller) validateHTTPRoute(
	gateway *gatewayv1.Gateway,
	httpRoute *gatewayv1.HTTPRoute,
) ([]gatewayv1.RouteParentStatus, []gatewayv1.Listener) {

	var parentStatuses []gatewayv1.RouteParentStatus
	// Use a map to collect a unique set of listeners that accepted the route.
	acceptedListenerSet := make(map[gatewayv1.SectionName]gatewayv1.Listener)

	// --- Determine the ResolvedRefs status for the entire Route first. ---
	// This is a property of the route itself, independent of any parent.
	resolvedRefsCondition := metav1.Condition{
		Type:               string(gatewayv1.RouteConditionResolvedRefs),
		ObservedGeneration: httpRoute.Generation,
	}
	if c.areBackendsValid(httpRoute) {
		resolvedRefsCondition.Status = metav1.ConditionTrue
		resolvedRefsCondition.Reason = string(gatewayv1.RouteReasonResolvedRefs)
		resolvedRefsCondition.Message = "All backend references have been resolved."
	} else {
		resolvedRefsCondition.Status = metav1.ConditionFalse
		resolvedRefsCondition.Reason = string(gatewayv1.RouteReasonBackendNotFound)
		resolvedRefsCondition.Message = "One or more backend references could not be found."
	}

	// --- Iterate over EACH ParentRef in the HTTPRoute ---
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		// We only care about refs that target our current Gateway.
		refNamespace := httpRoute.Namespace
		if parentRef.Namespace != nil {
			refNamespace = string(*parentRef.Namespace)
		}
		if parentRef.Name != gatewayv1.ObjectName(gateway.Name) || refNamespace != gateway.Namespace {
			continue // This ref is for another Gateway.
		}

		// This ref targets our Gateway. We MUST generate a status for it.
		var listenersForThisRef []gatewayv1.Listener
		rejectionReason := gatewayv1.RouteReasonNoMatchingParent

		// --- Find all listeners on the Gateway that match this specific parentRef ---
		for _, listener := range gateway.Spec.Listeners {
			sectionNameMatches := (parentRef.SectionName == nil) || (*parentRef.SectionName == listener.Name)
			portMatches := (parentRef.Port == nil) || (*parentRef.Port == listener.Port)

			if sectionNameMatches && portMatches {
				// The listener matches the ref. Now check if the listener's policy (e.g., hostname) allows it.
				if !isAllowedByListener(gateway, listener, httpRoute, c.namespaceLister) {
					rejectionReason = gatewayv1.RouteReasonNotAllowedByListeners
					continue
				}
				if !isAllowedByHostname(listener, httpRoute) {
					rejectionReason = gatewayv1.RouteReasonNoMatchingListenerHostname
					continue
				}
				listenersForThisRef = append(listenersForThisRef, listener)
			}
		}

		// --- Build the final status for this ParentRef ---
		status := gatewayv1.RouteParentStatus{
			ParentRef:      parentRef,
			ControllerName: controllerName,
			Conditions:     []metav1.Condition{},
		}

		// Create the 'Accepted' condition based on the listener validation.
		acceptedCondition := metav1.Condition{
			Type:               string(gatewayv1.RouteConditionAccepted),
			ObservedGeneration: httpRoute.Generation,
		}

		if len(listenersForThisRef) == 0 {
			acceptedCondition.Status = metav1.ConditionFalse
			acceptedCondition.Reason = string(rejectionReason)
			acceptedCondition.Message = "No listener matched the parentRef."
			if rejectionReason == gatewayv1.RouteReasonNotAllowedByListeners {
				acceptedCondition.Message = "Route is not allowed by a listener's policy."
			} else {
				acceptedCondition.Message = "The route's hostnames do not match any listener hostnames."
			}
		} else {
			acceptedCondition.Status = metav1.ConditionTrue
			acceptedCondition.Reason = string(gatewayv1.RouteReasonAccepted)
			acceptedCondition.Message = "Route is accepted."
			for _, l := range listenersForThisRef {
				acceptedListenerSet[l.Name] = l
			}
		}

		// --- 4. Combine the two independent conditions into the final status. ---
		meta.SetStatusCondition(&status.Conditions, acceptedCondition)
		meta.SetStatusCondition(&status.Conditions, resolvedRefsCondition)

		parentStatuses = append(parentStatuses, status)
	}

	var allAcceptingListeners []gatewayv1.Listener
	for _, l := range acceptedListenerSet {
		allAcceptingListeners = append(allAcceptingListeners, l)
	}

	return parentStatuses, allAcceptingListeners
}

// areBackendsValid is a helper extracted from the original validate function.
func (c *Controller) areBackendsValid(httpRoute *gatewayv1.HTTPRoute) bool {
	for _, rule := range httpRoute.Spec.Rules {
		if ruleHasRedirectFilter(rule) {
			continue
		}
		for _, backendRef := range rule.BackendRefs {
			ns := httpRoute.Namespace
			if backendRef.Namespace != nil {
				ns = string(*backendRef.Namespace)
			}
			if _, err := c.serviceLister.Services(ns).Get(string(backendRef.Name)); err != nil {
				return false
			}
		}
	}
	return true
}

// Helper to check for redirect filters
func ruleHasRedirectFilter(rule gatewayv1.HTTPRouteRule) bool {
	for _, filter := range rule.Filters {
		if filter.Type == gatewayv1.HTTPRouteFilterRequestRedirect {
			return true
		}
	}
	return false
}

func translateBackendRefToCluster(serviceLister corev1listers.ServiceLister, defaultNamespace string, backendRef gatewayv1.BackendRef) (*clusterv3.Cluster, error) {
	ns := defaultNamespace
	if backendRef.Namespace != nil {
		ns = string(*backendRef.Namespace)
	}
	service, err := serviceLister.Services(ns).Get(string(backendRef.Name))
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
		CommonLbConfig: &clusterv3.Cluster_CommonLbConfig{
			HealthyPanicThreshold: &typev3.Percent{
				Value: 0,
			},
		},
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

	if c.tunnelManager != nil {
		err := c.tunnelManager.RemoveTunnels(containerName)
		if err != nil {
			klog.Errorf("failed to remove tunnels for deleted gateway %s: %v", name, err)
		}
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

// setAcceptedCondition sets the Accepted condition on the Gateway.
func setAcceptedCondition(newGw *gatewayv1.Gateway) {
	if newGw.Spec.Infrastructure != nil && newGw.Spec.Infrastructure.ParametersRef != nil {
		meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionFalse,
			Reason:             string(gatewayv1.GatewayReasonInvalidParameters),
			Message:            "infrastructure.parametersRef is not supported",
			ObservedGeneration: newGw.Generation,
		})
		return
	}

	for _, address := range newGw.Spec.Addresses {
		if address.Type != nil && *address.Type != gatewayv1.IPAddressType && *address.Type != gatewayv1.HostnameAddressType {
			meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
				Type:               string(gatewayv1.GatewayConditionAccepted),
				Status:             metav1.ConditionFalse,
				Reason:             string(gatewayv1.GatewayReasonUnsupportedAddress),
				Message:            fmt.Sprintf("Address type %s is not supported", *address.Type),
				ObservedGeneration: newGw.Generation,
			})
			return
		}
	}

	var invalidListeners int
	for _, ls := range newGw.Status.Listeners {
		if meta.IsStatusConditionFalse(ls.Conditions, string(gatewayv1.ListenerConditionAccepted)) {
			invalidListeners++
		}
	}
	switch {
	case invalidListeners == 0:
		meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			Reason:             string(gatewayv1.GatewayReasonAccepted),
			Message:            "Gateway is accepted",
			ObservedGeneration: newGw.Generation,
		})
	case invalidListeners < len(newGw.Status.Listeners):
		meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			Reason:             string(gatewayv1.GatewayReasonListenersNotValid),
			Message:            "One or more listeners are not valid",
			ObservedGeneration: newGw.Generation,
		})
	default:
		meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionFalse,
			Reason:             string(gatewayv1.GatewayReasonListenersNotValid),
			Message:            "All listeners are invalid",
			ObservedGeneration: newGw.Generation,
		})
	}
}

// setProgrammedCondition sets the Programmed condition for the Gateway.
// Accepted is expected to already be set on newGw by the time this is called.
func setProgrammedCondition(newGw *gatewayv1.Gateway, xdsErr error) {
	switch {
	case meta.IsStatusConditionFalse(newGw.Status.Conditions, string(gatewayv1.GatewayConditionAccepted)):
		// A Gateway can never be Programmed=True if Accepted=False.
		meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionFalse,
			Reason:             string(gatewayv1.GatewayReasonInvalid),
			Message:            "Gateway is not accepted",
			ObservedGeneration: newGw.Generation,
		})
	case xdsErr != nil:
		// If the Envoy update fails, the Gateway is not programmed.
		meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionFalse,
			Reason:             "ReconciliationError",
			Message:            fmt.Sprintf("Failed to program envoy config: %s", xdsErr.Error()),
			ObservedGeneration: newGw.Generation,
		})
	default:
		// If the Envoy update succeeds:

		// Check if all addresses were assigned.
		totalAddresses := len(newGw.Spec.Addresses)
		addressAssigned := 0
		for _, address := range newGw.Spec.Addresses {
			if slices.ContainsFunc(newGw.Status.Addresses, func(statusAddress gatewayv1.GatewayStatusAddress) bool {
				if (statusAddress.Type != nil) != (address.Type != nil) {
					return false
				}
				if statusAddress.Type != nil && address.Type != nil {
					return *statusAddress.Type == *address.Type && statusAddress.Value == address.Value
				}
				return statusAddress.Value == address.Value
			}) {
				addressAssigned++
			}
		}
		if addressAssigned != totalAddresses {
			meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
				Type:               string(gatewayv1.GatewayConditionProgrammed),
				Status:             metav1.ConditionFalse,
				Reason:             string(gatewayv1.GatewayReasonAddressNotUsable),
				Message:            fmt.Sprintf("%d out of %d addresses failed to be assigned", totalAddresses-addressAssigned, totalAddresses),
				ObservedGeneration: newGw.Generation,
			})
			return
		}

		// Check if all individual listeners were programmed.
		totalListeners := len(newGw.Status.Listeners)
		listenersProgrammed := 0
		for _, ls := range newGw.Status.Listeners {
			if meta.IsStatusConditionTrue(ls.Conditions, string(gatewayv1.ListenerConditionProgrammed)) {
				listenersProgrammed++
			}
		}
		if listenersProgrammed == totalListeners {
			// The Gateway is only fully programmed if all listeners are programmed.
			meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
				Type:               string(gatewayv1.GatewayConditionProgrammed),
				Status:             metav1.ConditionTrue,
				Reason:             string(gatewayv1.GatewayReasonProgrammed),
				Message:            "Envoy configuration updated successfully",
				ObservedGeneration: newGw.Generation,
			})
		} else {
			// If any listener failed, the Gateway as a whole is not fully programmed.
			meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
				Type:               string(gatewayv1.GatewayConditionProgrammed),
				Status:             metav1.ConditionFalse,
				Reason:             "ListenersNotProgrammed",
				Message:            fmt.Sprintf("%d out of %d listeners failed to be programmed", totalListeners-listenersProgrammed, totalListeners),
				ObservedGeneration: newGw.Generation,
			})
		}
	}
}

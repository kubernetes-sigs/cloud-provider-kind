package gateway

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func hostnameConflictMessage(preferredKind, namespace, name string) string {
	return fmt.Sprintf("hostname intersects %s %s/%s on the same listener; that route was preferred", preferredKind, namespace, name)
}

// applyHTTPGRPCHostnameUniqueness enforces GEP-1016 hostname uniqueness between
// HTTPRoute and GRPCRoute on the same listener. When hostnames overlap, the
// older route is kept and the other route is rejected with Accepted=False.
func applyHTTPGRPCHostnameUniqueness(
	gateway *gatewayv1.Gateway,
	httpByListener map[gatewayv1.SectionName][]*gatewayv1.HTTPRoute,
	grpcByListener map[gatewayv1.SectionName][]*gatewayv1.GRPCRoute,
	httpStatuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
	grpcStatuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
) {
	httpMessages := map[types.NamespacedName]string{}
	grpcMessages := map[types.NamespacedName]string{}

	for _, listener := range gateway.Spec.Listeners {
		httpRoutes := httpByListener[listener.Name]
		grpcRoutes := grpcByListener[listener.Name]
		if len(httpRoutes) == 0 || len(grpcRoutes) == 0 {
			continue
		}

		httpReject := make([]bool, len(httpRoutes))
		grpcReject := make([]bool, len(grpcRoutes))
		httpPreferred := make([]string, len(httpRoutes))
		grpcPreferred := make([]string, len(grpcRoutes))
		for i, httpRoute := range httpRoutes {
			for j, grpcRoute := range grpcRoutes {
				if !routeHostnamesOverlap(listener, httpRoute.Spec.Hostnames, grpcRoute.Spec.Hostnames) {
					continue
				}
				if routePrecedenceWins(httpRoute.ObjectMeta, grpcRoute.ObjectMeta) {
					grpcReject[j] = true
					if grpcPreferred[j] == "" {
						grpcPreferred[j] = hostnameConflictMessage("HTTPRoute", httpRoute.Namespace, httpRoute.Name)
					}
				} else {
					httpReject[i] = true
					if httpPreferred[i] == "" {
						httpPreferred[i] = hostnameConflictMessage("GRPCRoute", grpcRoute.Namespace, grpcRoute.Name)
					}
				}
			}
		}

		for i, route := range httpRoutes {
			if !httpReject[i] || httpPreferred[i] == "" {
				continue
			}
			key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
			if httpMessages[key] == "" {
				httpMessages[key] = httpPreferred[i]
			}
		}
		for j, route := range grpcRoutes {
			if !grpcReject[j] || grpcPreferred[j] == "" {
				continue
			}
			key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
			if grpcMessages[key] == "" {
				grpcMessages[key] = grpcPreferred[j]
			}
		}

		httpByListener[listener.Name] = filterHTTPRoutes(httpRoutes, httpReject)
		grpcByListener[listener.Name] = filterGRPCRoutes(grpcRoutes, grpcReject)
	}

	markConflictingHTTPParents(gateway, httpByListener, httpStatuses, httpMessages)
	markConflictingGRPCParents(gateway, grpcByListener, grpcStatuses, grpcMessages)
}

func filterHTTPRoutes(routes []*gatewayv1.HTTPRoute, reject []bool) []*gatewayv1.HTTPRoute {
	var kept []*gatewayv1.HTTPRoute
	for i, route := range routes {
		if !reject[i] {
			kept = append(kept, route)
		}
	}
	return kept
}

func filterGRPCRoutes(routes []*gatewayv1.GRPCRoute, reject []bool) []*gatewayv1.GRPCRoute {
	var kept []*gatewayv1.GRPCRoute
	for i, route := range routes {
		if !reject[i] {
			kept = append(kept, route)
		}
	}
	return kept
}

func markConflictingHTTPParents(
	gateway *gatewayv1.Gateway,
	remainingByListener map[gatewayv1.SectionName][]*gatewayv1.HTTPRoute,
	statuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
	messages map[types.NamespacedName]string,
) {
	remaining := map[types.NamespacedName]map[gatewayv1.SectionName]struct{}{}
	for listenerName, routes := range remainingByListener {
		for _, route := range routes {
			key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
			if remaining[key] == nil {
				remaining[key] = map[gatewayv1.SectionName]struct{}{}
			}
			remaining[key][listenerName] = struct{}{}
		}
	}
	rejectParentsWithoutListener(gateway, remaining, statuses, messages)
}

func markConflictingGRPCParents(
	gateway *gatewayv1.Gateway,
	remainingByListener map[gatewayv1.SectionName][]*gatewayv1.GRPCRoute,
	statuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
	messages map[types.NamespacedName]string,
) {
	remaining := map[types.NamespacedName]map[gatewayv1.SectionName]struct{}{}
	for listenerName, routes := range remainingByListener {
		for _, route := range routes {
			key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
			if remaining[key] == nil {
				remaining[key] = map[gatewayv1.SectionName]struct{}{}
			}
			remaining[key][listenerName] = struct{}{}
		}
	}
	rejectParentsWithoutListener(gateway, remaining, statuses, messages)
}

func rejectParentsWithoutListener(
	gateway *gatewayv1.Gateway,
	remaining map[types.NamespacedName]map[gatewayv1.SectionName]struct{},
	statuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
	messages map[types.NamespacedName]string,
) {
	for key, parentStatuses := range statuses {
		for i := range parentStatuses {
			if !meta.IsStatusConditionTrue(parentStatuses[i].Conditions, string(gatewayv1.RouteConditionAccepted)) {
				continue
			}
			if parentStillHasListener(gateway, parentStatuses[i].ParentRef, remaining[key]) {
				continue
			}
			message := messages[key]
			if message == "" {
				message = "hostname intersects a route of the other type on the same listener; the other route was preferred"
			}
			meta.SetStatusCondition(&parentStatuses[i].Conditions, metav1.Condition{
				Type:    string(gatewayv1.RouteConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayv1.RouteReasonUnsupportedValue),
				Message: message,
			})
		}
		statuses[key] = parentStatuses
	}
}

func parentStillHasListener(gateway *gatewayv1.Gateway, parentRef gatewayv1.ParentReference, remainingListeners map[gatewayv1.SectionName]struct{}) bool {
	if remainingListeners == nil {
		return false
	}
	if !parentRefTargetsGateway(parentRef, gateway.Namespace, gateway) {
		return false
	}
	for _, listener := range gateway.Spec.Listeners {
		if !parentRefMatchesListener(parentRef, listener) {
			continue
		}
		if _, ok := remainingListeners[listener.Name]; ok {
			return true
		}
	}
	return false
}

func hostnamesOverlap(a, b []string) bool {
	for _, ha := range a {
		for _, hb := range b {
			if hostnamesIntersect(ha, hb) {
				return true
			}
		}
	}
	return false
}

func hostnamesIntersect(a, b string) bool {
	if a == "*" || b == "*" || a == b {
		return true
	}
	return isHostnameSubset(a, b) || isHostnameSubset(b, a)
}

func routeHostnamesOverlap(listener gatewayv1.Listener, aHostnames, bHostnames []gatewayv1.Hostname) bool {
	return hostnamesOverlap(
		getIntersectingHostnames(listener, aHostnames),
		getIntersectingHostnames(listener, bHostnames),
	)
}

func routePrecedenceWins(a, b metav1.ObjectMeta) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	aKey := a.Namespace + "/" + a.Name
	bKey := b.Namespace + "/" + b.Name
	return aKey < bKey
}

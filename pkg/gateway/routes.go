package gateway

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// isRouteReferenced checks if the given route object has a ParentRef
// that explicitly targets the specified gateway and listener.
func isRouteReferenced(gateway *gatewayv1.Gateway, listener gatewayv1.Listener, route metav1.Object) bool {
	var parentRefs []gatewayv1.ParentReference

	// Extract ParentRefs based on the concrete route type
	switch r := route.(type) {
	case *gatewayv1.HTTPRoute:
		parentRefs = r.Spec.ParentRefs
	case *gatewayv1.GRPCRoute:
		parentRefs = r.Spec.ParentRefs
	default:
		klog.Warningf("isRouteReferenced: Unsupported route type %T for route %s/%s", route, route.GetNamespace(), route.GetName())
		return false
	}

	if len(parentRefs) == 0 {
		klog.V(5).Infof("isRouteReferenced: Route %s/%s has no ParentRefs", route.GetNamespace(), route.GetName())
		return false
	}

	routeNamespace := route.GetNamespace()
	gatewayNamespace := gateway.GetNamespace()
	gatewayName := gatewayv1.ObjectName(gateway.GetName())

	for _, ref := range parentRefs {
		refGroup := gatewayv1.Group(gatewayv1.GroupName)
		if ref.Group != nil {
			refGroup = *ref.Group
		}
		refKind := gatewayv1.Kind("Gateway")
		if ref.Kind != nil {
			refKind = *ref.Kind
		}
		if refGroup != gatewayv1.GroupName || refKind != "Gateway" {
			continue
		}

		refNamespace := routeNamespace
		if ref.Namespace != nil {
			refNamespace = string(*ref.Namespace)
		}
		if refNamespace != gatewayNamespace {
			continue
		}

		if ref.Name != gatewayName {
			continue
		}

		return true
	}

	return false
}

// isAllowedByListener checks if a given route is allowed to attach to a listener
// based on the listener's `allowedRoutes` specification for namespaces and kinds.
func isAllowedByListener(gateway *gatewayv1.Gateway, listener gatewayv1.Listener, route metav1.Object, namespaceLister corev1listers.NamespaceLister) bool {
	allowed := listener.AllowedRoutes
	if allowed == nil {
		// If AllowedRoutes is not set, only routes in the same namespace are allowed.
		return route.GetNamespace() == gateway.GetNamespace()
	}

	routeNamespace := route.GetNamespace()
	gatewayNamespace := gateway.GetNamespace()

	// Check if the route's namespace is allowed.
	namespaceAllowed := false
	effectiveFrom := gatewayv1.NamespacesFromSame
	if allowed.Namespaces != nil && allowed.Namespaces.From != nil {
		effectiveFrom = *allowed.Namespaces.From
	}

	switch effectiveFrom {
	case gatewayv1.NamespacesFromAll:
		namespaceAllowed = true
	case gatewayv1.NamespacesFromSame:
		namespaceAllowed = (routeNamespace == gatewayNamespace)
	case gatewayv1.NamespacesFromSelector:
		if allowed.Namespaces.Selector == nil {
			klog.Errorf("Invalid AllowedRoutes: Namespaces.From is 'Selector' but Namespaces.Selector is nil for Gateway %s/%s, Listener %s", gatewayNamespace, gateway.GetName(), listener.Name)
			return false
		}
		if namespaceLister == nil {
			klog.Warningf("Namespace selection using 'Selector' requires a Namespace Lister, but none was provided. Denying route %s/%s.", routeNamespace, route.GetName())
			return false
		}
		selector, err := metav1.LabelSelectorAsSelector(allowed.Namespaces.Selector)
		if err != nil {
			klog.Errorf("Failed to parse label selector for Gateway %s/%s, Listener %s: %v", gatewayNamespace, gateway.GetName(), listener.Name, err)
			return false
		}
		routeNsObj, err := namespaceLister.Get(routeNamespace)
		if err != nil {
			klog.Warningf("Failed to get namespace %s for route %s/%s: %v", routeNamespace, routeNamespace, route.GetName(), err)
			return false
		}
		namespaceAllowed = selector.Matches(labels.Set(routeNsObj.GetLabels()))
	default:
		klog.Errorf("Unknown 'From' value %q in AllowedRoutes.Namespaces for Gateway %s/%s, Listener %s", effectiveFrom, gatewayNamespace, gateway.GetName(), listener.Name)
		return false
	}

	if !namespaceAllowed {
		return false
	}

	// If namespaces are allowed, check if the route's kind is allowed.
	if len(allowed.Kinds) == 0 {
		// No kinds specified, so all kinds are allowed.
		return true
	}

	var routeGroup, routeKind string
	switch route.(type) {
	case *gatewayv1.HTTPRoute:
		routeGroup = gatewayv1.GroupName
		routeKind = "HTTPRoute"
	case *gatewayv1.GRPCRoute:
		routeGroup = gatewayv1.GroupName
		routeKind = "GRPCRoute"
	default:
		klog.Warningf("Cannot determine GroupKind for route object type %T for route %s/%s", route, routeNamespace, route.GetName())
		return false
	}

	for _, allowedKind := range allowed.Kinds {
		allowedGroup := gatewayv1.Group(gatewayv1.GroupName)
		if allowedKind.Group != nil && *allowedKind.Group != "" {
			allowedGroup = *allowedKind.Group
		}
		if routeKind == string(allowedKind.Kind) && routeGroup == string(allowedGroup) {
			return true
		}
	}

	// The route's kind is not in the allowed list.
	return false
}

// isAllowedByHostname checks if a route is allowed to attach to a listener
// based on hostname matching rules.
func isAllowedByHostname(listener gatewayv1.Listener, route metav1.Object) bool {
	// If the listener specifies no hostname, it allows all route hostnames.
	if listener.Hostname == nil || *listener.Hostname == "" {
		return true
	}
	listenerHostname := string(*listener.Hostname)

	var routeHostnames []gatewayv1.Hostname
	switch r := route.(type) {
	case *gatewayv1.HTTPRoute:
		routeHostnames = r.Spec.Hostnames
	case *gatewayv1.GRPCRoute:
		routeHostnames = r.Spec.Hostnames
	default:
		// Not a type with hostnames, so no hostname check needed.
		return true
	}

	// If the route specifies no hostnames, it inherits from the listener, which is always valid.
	if len(routeHostnames) == 0 {
		return true
	}

	// If the route specifies hostnames, at least one must be permitted by the listener.
	for _, routeHostname := range routeHostnames {
		if isHostnameSubset(string(routeHostname), listenerHostname) {
			// Found a valid hostname match. The route is allowed by this listener.
			return true
		}
	}

	// If we reach here, the route specified hostnames, but NONE of them were valid
	// for this listener. The route must not be attached.
	return false
}

// getIntersectingHostnames calculates the precise set of hostnames for an Envoy VirtualHost,
// resolving each match to the most restrictive hostname as per the Gateway API specification.
// It returns a slice of the resulting hostnames, or an empty slice if there is no valid intersection.
func getIntersectingHostnames(listener gatewayv1.Listener, routeHostnames []gatewayv1.Hostname) []string {
	// Case 1: The listener has no hostname specified. It acts as a universal wildcard,
	// allowing any and all hostnames from the route.
	if listener.Hostname == nil || *listener.Hostname == "" {
		if len(routeHostnames) == 0 {
			return []string{"*"} // Universal match
		}
		// The result is simply the route's own hostnames.
		var names []string
		for _, h := range routeHostnames {
			names = append(names, string(h))
		}
		return names
	}
	listenerHostname := string(*listener.Hostname)

	// Case 2: The route has no hostnames. It implicitly inherits the listener's specific hostname.
	if len(routeHostnames) == 0 {
		return []string{listenerHostname}
	}

	// Case 3: Both have hostnames. We must find the intersection and then determine
	// the most restrictive hostname for each match.
	intersection := sets.New[string]()
	for _, h := range routeHostnames {
		routeHostname := string(h)
		if isHostnameSubset(routeHostname, listenerHostname) {
			// A valid intersection was found. Now, determine the most specific
			// hostname to use for the configuration.
			if strings.HasPrefix(routeHostname, "*") && !strings.HasPrefix(listenerHostname, "*") {
				// If the route is a wildcard and the listener is specific, the listener's
				// specific hostname is the most restrictive result.
				intersection.Insert(listenerHostname)
			} else {
				// In all other valid cases (exact match, specific route on a wildcard listener),
				// the route's hostname is the most restrictive result.
				intersection.Insert(routeHostname)
			}
		}
	}

	// Return the unique set of resulting hostnames.
	return intersection.UnsortedList()
}

// isHostnameSubset checks if a route hostname is a valid subset of a listener hostname,
// implementing the precise matching rules from the Gateway API specification.
func isHostnameSubset(routeHostname, listenerHostname string) bool {
	// Rule 1: An exact match is always a valid intersection.
	if routeHostname == listenerHostname {
		return true
	}

	// Rule 2: Listener has a wildcard (e.g., "*.example.com").
	if strings.HasPrefix(listenerHostname, "*.") {
		// Use the part of the string including the dot as the suffix.
		listenerSuffix := listenerHostname[1:] // e.g., ".example.com"

		// Case 2a: Route also has a wildcard (e.g., "*.foo.example.com").
		// The route's suffix must be identical to or a sub-suffix of the listener's.
		if strings.HasPrefix(routeHostname, "*.") {
			routeSuffix := routeHostname[1:] // e.g., ".foo.example.com"
			return strings.HasSuffix(routeSuffix, listenerSuffix)
		}

		// Case 2b: Route is specific (e.g., "foo.example.com").
		// The route must end with the listener's suffix. This correctly handles
		// the "parent domain" case because "example.com" does not have the
		// suffix ".example.com".
		return strings.HasSuffix(routeHostname, listenerSuffix)
	}

	// Rule 3: Route has a wildcard (e.g., "*.example.com").
	if strings.HasPrefix(routeHostname, "*.") {
		routeSuffix := routeHostname[1:] // e.g., ".example.com"
		routeDomain := routeHostname[2:] // e.g., "example.com"

		// The listener must be more specific (not a wildcard).
		if !strings.HasPrefix(listenerHostname, "*.") {
			// The listener hostname must be the parent domain or a subdomain.
			// e.g., "example.com" or "foo.example.com" are subsets of "*.example.com".
			return listenerHostname == routeDomain || strings.HasSuffix(listenerHostname, routeSuffix)
		}
	}

	return false
}

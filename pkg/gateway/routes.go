package gateway

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

// isRouteAllowed checks if a given route is allowed to attach to a listener
// based on the listener's `allowedRoutes` specification.
func isRouteAllowed(gateway *gatewayv1.Gateway, listener gatewayv1.Listener, route metav1.Object, namespaceLister corev1listers.NamespaceLister) bool {
	allowed := listener.AllowedRoutes
	if allowed == nil {
		return route.GetNamespace() == gateway.GetNamespace()
	}

	routeNamespace := route.GetNamespace()
	gatewayNamespace := gateway.GetNamespace()

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

	if len(allowed.Kinds) == 0 {
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

	// If the listener specifies no hostname, it allows all route hostnames.
	if listener.Hostname == nil || *listener.Hostname == "" {
		return true // Continue with the result of the namespace check.
	}
	listenerHostname := string(*listener.Hostname)

	var routeHostnames []gatewayv1.Hostname
	switch r := route.(type) {
	case *gatewayv1.HTTPRoute:
		routeHostnames = r.Spec.Hostnames
	case *gatewayv1.GRPCRoute:
		routeHostnames = r.Spec.Hostnames
	default:
		return true // Not a type with hostnames, so no hostname check needed.
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

// isHostnameSubset checks if a route hostname is a valid subset of a listener hostname,
// following the precise Gateway API matching rules.
func isHostnameSubset(routeHostname, listenerHostname string) bool {
	// 1. An exact match is always a valid subset.
	if routeHostname == listenerHostname {
		return true
	}

	// 2. If the listener has a wildcard, the route's hostname must be a subdomain.
	if strings.HasPrefix(listenerHostname, "*.") {
		domain := strings.TrimPrefix(listenerHostname, "*.")
		// The route must be a proper subdomain. It cannot be the parent domain itself.
		// e.g., "foo.example.com" is a subset of "*.example.com".
		// e.g., "example.com" is NOT a subset of "*.example.com".
		if strings.HasSuffix(routeHostname, "."+domain) {
			return true
		}
	}

	// A route with a wildcard can only be a subset of an identical listener wildcard.
	// e.g., route "*.example.com" is NOT a subset of listener "foo.example.com".
	// This case is already handled by the exact match check in rule #1.

	return false
}

// isValidParentRef checks if a given Route has a ParentRef that is a valid,
// specific reference to the provided Gateway and Listener.
//
// It returns:
// - (true, &parentRef) if a specific ParentRef is a valid match for the listener.
// - (false, &parentRef) if a ParentRef matched the Gateway but failed to match the listener's port or sectionName.
// - (false, nil) if no ParentRef in the route targets this Gateway at all.
func isValidParentRef(gateway *gatewayv1.Gateway, listener gatewayv1.Listener, route metav1.Object) bool {
	var parentRefs []gatewayv1.ParentReference

	// 1. Extract ParentRefs from the specific Route object.
	switch r := route.(type) {
	case *gatewayv1.HTTPRoute:
		parentRefs = r.Spec.ParentRefs
	case *gatewayv1.GRPCRoute:
		parentRefs = r.Spec.ParentRefs
	default:
		// This case should ideally not be hit if called from the main controller loop.
		klog.Warningf("isValidParentRef called with unsupported route type: %T", route)
		return false
	}

	if len(parentRefs) == 0 {
		return false
	}

	for _, parentRef := range parentRefs {
		// Default to the route's own namespace if the ParentRef doesn't specify one.
		refNamespace := route.GetNamespace()
		if parentRef.Namespace != nil {
			refNamespace = string(*parentRef.Namespace)
		}
		if parentRef.Name != gatewayv1.ObjectName(gateway.Name) || refNamespace != gateway.Namespace {
			continue // This ParentRef is for a different Gateway, so we ignore it.
		}

		//    A nil field in the ParentRef acts as a wildcard for that property.
		sectionNameMatches := (parentRef.SectionName == nil) || (*parentRef.SectionName == listener.Name)
		portMatches := (parentRef.Port == nil) || (*parentRef.Port == listener.Port)

		// For the reference to be valid, ALL specified fields must match.
		if sectionNameMatches && portMatches {
			return true
		}
	}

	return false
}

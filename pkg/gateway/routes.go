package gateway

import (
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
	// Add cases for other supported Route types (TLSRoute, TCPRoute, UDPRoute) if needed
	// case *gatewayv1alpha2.TLSRoute:
	// 	parentRefs = r.Spec.ParentRefs
	// ... etc
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
		// 1. Check Group and Kind match "gateway.networking.k8s.io/Gateway" (allowing defaults)
		refGroup := gatewayv1.Group(gatewayv1.GroupName) // Default group
		if ref.Group != nil {
			refGroup = *ref.Group
		}
		refKind := gatewayv1.Kind("Gateway") // Default kind
		if ref.Kind != nil {
			refKind = *ref.Kind
		}
		if refGroup != gatewayv1.GroupName || refKind != "Gateway" {
			klog.V(5).Infof("isRouteReferenced: ParentRef in route %s/%s has non-Gateway Group/Kind (%s/%s), skipping", routeNamespace, route.GetName(), refGroup, refKind)
			continue // Skip refs that don't point to a Gateway
		}

		// 2. Check Namespace matches gateway's namespace (allowing default)
		refNamespace := routeNamespace // Default namespace is the route's namespace
		if ref.Namespace != nil {
			refNamespace = string(*ref.Namespace)
		}
		if refNamespace != gatewayNamespace {
			klog.V(5).Infof("isRouteReferenced: ParentRef in route %s/%s points to different namespace (%s) than gateway (%s), skipping", routeNamespace, route.GetName(), refNamespace, gatewayNamespace)
			continue // Skip refs pointing to a different namespace
		}

		// 3. Check Name matches gateway's name
		if ref.Name != gatewayName {
			klog.V(5).Infof("isRouteReferenced: ParentRef in route %s/%s points to different gateway name (%s) than target (%s), skipping", routeNamespace, route.GetName(), ref.Name, gatewayName)
			continue // Skip refs pointing to a different gateway name
		}

		// 4. Check SectionName (Listener Name) if specified in input
		if ref.SectionName != nil && *ref.SectionName != listener.Name {
			klog.V(5).Infof("isRouteReferenced: ParentRef in route %s/%s does not match target listener name (%s vs %s), skipping", routeNamespace, route.GetName(), *ref.SectionName, listener.Name)
			continue // Skip refs that don't match the specific listener name when one is required
		}
		// If listenerName is "" (empty), we don't filter based on SectionName here.
		// A nil SectionName in the ref means it targets the Gateway as a whole,
		// which is acceptable if we aren't looking for a specific listener.

		// 5. Check Port if specified in input
		if ref.Port != nil && *ref.Port != listener.Port {
			klog.V(5).Infof("isRouteReferenced: ParentRef in route %s/%s does not match target listener port (%v vs %d), skipping", routeNamespace, route.GetName(), ref.Port, listener.Port)
			continue // Skip refs that don't match the specific listener port when one is required
		}

		// If we passed all checks for this ParentRef, the route references the target
		klog.V(4).Infof("isRouteReferenced: Route %s/%s found matching ParentRef for Gateway %s/%s (Listener: %q, Port: %d)", routeNamespace, route.GetName(), gatewayNamespace, gatewayName, listener.Name, listener.Port)
		return true
	}

	// No matching ParentRef found after checking all of them
	klog.V(4).Infof("isRouteReferenced: Route %s/%s has no ParentRef matching Gateway %s/%s (Listener: %q, Port: %d)", routeNamespace, route.GetName(), gatewayNamespace, gatewayName, listener.Name, listener.Port)
	return false
}

// isRouteAllowed checks if a given route is allowed to attach to a listener
// based *only* on the listener's `allowedRoutes` specification (namespace and kind).
func isRouteAllowed(gateway *gatewayv1.Gateway, listener gatewayv1.Listener, route metav1.Object, namespaceLister corev1listers.NamespaceLister) bool {
	// 1. Handle nil AllowedRoutes - implicitly allows routes based on this field.
	//    The default defined in the CRD spec is {namespaces:{from: Same}},
	//    so a nil value here means the CRD default wasn't applied or it's an older object.
	//    For safety and clarity, let's treat nil as "allow all from same namespace" matching the CRD default intent.
	allowed := listener.AllowedRoutes
	if allowed == nil {
		// Apply default behavior: only allow from the same namespace, any compatible kind.
		return route.GetNamespace() == gateway.GetNamespace()
	}

	routeNamespace := route.GetNamespace()
	gatewayNamespace := gateway.GetNamespace()

	// 2. Check Namespace restrictions
	namespaceAllowed := false
	effectiveFrom := gatewayv1.NamespacesFromSame // Default if field is nil
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
			return false // Invalid config
		}
		if namespaceLister == nil {
			// Log a warning or error if the lister is required but not provided.
			// Deny access as we cannot evaluate the selector.
			klog.Warningf("Namespace selection using 'Selector' requires a Namespace Lister, but none was provided for Gateway %s/%s, Listener %s. Denying route %s/%s.", gatewayNamespace, gateway.GetName(), listener.Name, routeNamespace, route.GetName())
			return false
		}

		selector, err := metav1.LabelSelectorAsSelector(allowed.Namespaces.Selector)
		if err != nil {
			klog.Errorf("Failed to parse label selector for Gateway %s/%s, Listener %s: %v", gatewayNamespace, gateway.GetName(), listener.Name, err)
			return false // Invalid selector
		}

		routeNsObj, err := namespaceLister.Get(routeNamespace)
		if err != nil {
			// Log error (e.g., namespace not found) and deny.
			// A route from a non-existent namespace cannot satisfy the selector.
			klog.Warningf("Failed to get namespace %s for route %s/%s when checking selector for Gateway %s/%s, Listener %s: %v", routeNamespace, routeNamespace, route.GetName(), gatewayNamespace, gateway.GetName(), listener.Name, err)
			return false
		}

		// Check if the namespace labels match the selector
		namespaceAllowed = selector.Matches(labels.Set(routeNsObj.GetLabels()))
		if !namespaceAllowed {
			klog.V(5).Infof("Route %s/%s denied by namespace selector for Gateway %s/%s, Listener %s", routeNamespace, route.GetName(), gatewayNamespace, gateway.GetName(), listener.Name)
		} else {
			klog.V(5).Infof("Route %s/%s allowed by namespace selector for Gateway %s/%s, Listener %s", routeNamespace, route.GetName(), gatewayNamespace, gateway.GetName(), listener.Name)
		}
	default:
		// Should be caught by validation, but handle defensively.
		klog.Errorf("Unknown 'From' value %q in AllowedRoutes.Namespaces for Gateway %s/%s, Listener %s", effectiveFrom, gatewayNamespace, gateway.GetName(), listener.Name)
		return false
	}

	if !namespaceAllowed {
		klog.V(4).Infof("Route %s/%s denied by namespace restrictions (From: %s) for Gateway %s/%s, Listener %s", routeNamespace, route.GetName(), effectiveFrom, gatewayNamespace, gateway.GetName(), listener.Name)
		return false
	}

	// 3. Check Kind restrictions
	if len(allowed.Kinds) == 0 {
		// No specific kinds listed means only the default kinds for the listener protocol are allowed.
		// This function *only* checks explicit restrictions. If the list is empty, this check passes.
		// The caller should verify protocol compatibility separately.
		return true
	}

	// Determine the GroupKind of the route object
	var routeGroup, routeKind string
	switch route.(type) {
	case *gatewayv1.HTTPRoute:
		routeGroup = gatewayv1.GroupName
		routeKind = "HTTPRoute"
	case *gatewayv1.GRPCRoute:
		routeGroup = gatewayv1.GroupName
		routeKind = "GRPCRoute"
	// Add cases for other supported Route types (TLSRoute, TCPRoute, UDPRoute) if needed
	// case *gatewayv1alpha2.TLSRoute:
	// 	routeGroup = gatewayv1.GroupName
	// 	routeKind = "TLSRoute"
	// ... etc
	default:
		// This function only knows about Gateway API route types by default.
		klog.Warningf("Cannot determine GroupKind for route object type %T for route %s/%s", route, routeNamespace, route.GetName())
		return false
	}

	for _, allowedKind := range allowed.Kinds {
		// Default Group is gateway.networking.k8s.io
		allowedGroup := gatewayv1.Group(gatewayv1.GroupName)
		if allowedKind.Group != nil && *allowedKind.Group != "" {
			allowedGroup = *allowedKind.Group
		}

		if routeKind == string(allowedKind.Kind) && routeGroup == string(allowedGroup) {
			// Found a matching kind in the allowed list
			return true
		}
	}

	// If we reached here, the route's kind was not found in the allowed list.
	klog.V(4).Infof("Route %s/%s (Kind: %s/%s) denied by Kind restrictions for Gateway %s/%s, Listener %s", routeNamespace, route.GetName(), routeGroup, routeKind, gatewayNamespace, gateway.GetName(), listener.Name)
	return false
}

// getFirstKey returns the first key from a map. Useful for simple cases.
func getFirstKey[K comparable, V any](m map[K]V) K {
	for k := range m {
		return k
	}
	var zero K
	return zero
}

// toInterfaceSlice converts a slice of a specific type to a slice of interface{}.
func toInterfaceSlice[T any](s []*T) []interface{} {
	result := make([]interface{}, len(s))
	for i, v := range s {
		result[i] = v
	}
	return result
}

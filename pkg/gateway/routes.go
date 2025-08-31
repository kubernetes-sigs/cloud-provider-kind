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

		if ref.SectionName != nil && *ref.SectionName != listener.Name {
			continue
		}

		if ref.Port != nil && *ref.Port != listener.Port {
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

	return false
}

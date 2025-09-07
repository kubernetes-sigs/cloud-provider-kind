package gateway

import (
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	gatewayv1beta1listers "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1beta1"
)

// isCrossNamespaceRefAllowed checks if a cross-namespace reference from a 'from' object
// to a 'to' object is permitted by a ReferenceGrant in the 'to' object's namespace.
func isCrossNamespaceRefAllowed(
	from gatewayv1beta1.ReferenceGrantFrom, // Describes the referencing object (e.g., an HTTPRoute)
	to gatewayv1beta1.ReferenceGrantTo, // Describes the referenced object (e.g., a Service)
	toNamespace string, // The namespace of the referenced object
	referenceGrantLister gatewayv1beta1listers.ReferenceGrantLister,
) bool {
	// List all ReferenceGrants in the target namespace.
	grants, err := referenceGrantLister.ReferenceGrants(toNamespace).List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list ReferenceGrants in namespace %s: %v", toNamespace, err)
		return false
	}

	for _, grant := range grants {
		// Check if the grant's "From" section matches our referencing object.
		fromAllowed := false
		for _, grantFrom := range grant.Spec.From {
			if grantFrom.Group == from.Group &&
				grantFrom.Kind == from.Kind &&
				grantFrom.Namespace == from.Namespace {
				fromAllowed = true
				break
			}
		}

		if !fromAllowed {
			continue // This grant doesn't apply to our 'from' object.
		}

		// Check if the grant's "To" section matches our referenced object.
		toAllowed := false
		for _, grantTo := range grant.Spec.To {
			if grantTo.Group == to.Group && grantTo.Kind == to.Kind {
				// If the grant specifies a resource name, it must match.
				if grantTo.Name == nil || *grantTo.Name == "" {
					toAllowed = true // Grant applies to all resources of this kind.
					break
				}
				if to.Name != nil && *grantTo.Name == *to.Name {
					toAllowed = true // Grant applies to this specific resource name.
					break
				}
			}
		}

		if toAllowed {
			// We found a grant that explicitly allows this cross-namespace reference.
			return true
		}
	}

	// No grant was found that allows this reference.
	return false
}

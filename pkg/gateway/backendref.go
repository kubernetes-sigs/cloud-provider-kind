package gateway

import (
	"fmt"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// backendRefToClusterName generates a unique Envoy cluster name from a Gateway API BackendRef.
// It returns a structured ControllerError with the appropriate Reason on failure.
func backendRefToClusterName(defaultNamespace string, backendRef gatewayv1.BackendRef) (string, error) {
	// 1. Validate that the Kind is a supported type (Service).
	if backendRef.Kind != nil && *backendRef.Kind != "Service" {
		return "", &ControllerError{
			Reason:  string(gatewayv1.RouteReasonInvalidKind),
			Message: fmt.Sprintf("unsupported backend kind: %s", *backendRef.Kind),
		}
	}

	// 2. Validate that the Port is specified.
	if backendRef.Port == nil {
		return "", &ControllerError{
			// Note: There isn't a specific reason for a missing port,
			// so InvalidParameters is a reasonable choice.
			Reason:  string(gatewayv1.RouteReasonUnsupportedProtocol),
			Message: "backend port must be specified",
		}
	}

	namespace := defaultNamespace
	if backendRef.Namespace != nil {
		namespace = string(*backendRef.Namespace)
	}

	port := int32(*backendRef.Port)
	group := "core"
	if backendRef.Group != nil && *backendRef.Group != "" {
		group = string(*backendRef.Group)
	}
	kind := "Service"
	if backendRef.Kind != nil && *backendRef.Kind != "" {
		kind = string(*backendRef.Kind)
	}

	// Format: <namespace>_<name>_<group>_<kind>_<port>
	clusterName := fmt.Sprintf("%s_%s_%s_%s_%d", namespace, backendRef.Name, group, kind, port)

	return clusterName, nil
}

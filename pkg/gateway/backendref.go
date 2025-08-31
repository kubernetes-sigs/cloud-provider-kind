package gateway

import (
	"fmt"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// backendRefToClusterName generates a unique Envoy cluster name from a Gateway API BackendRef.
func backendRefToClusterName(defaultNamespace string, backendRef gatewayv1.BackendRef) (string, error) {
	if backendRef.Kind != nil && *backendRef.Kind != "Service" {
		return "", fmt.Errorf("unsupported backend kind: %s", *backendRef.Kind)
	}
	if backendRef.Port == nil {
		return "", fmt.Errorf("backend port must be specified")
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
	return fmt.Sprintf("%s_%s_%s_%s_%d", namespace, backendRef.Name, group, kind, port), nil
}

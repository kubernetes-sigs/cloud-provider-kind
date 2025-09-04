package gateway

import (
	"errors"
	"testing"

	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestBackendRefToClusterName(t *testing.T) {
	tests := []struct {
		name             string
		defaultNamespace string
		backendRef       gatewayv1.BackendRef
		wantName         string
		wantErr          bool
		wantReason       string
	}{
		{
			name:             "valid backendRef with all fields",
			defaultNamespace: "default",
			backendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name:      "my-service",
					Namespace: ptr.To(gatewayv1.Namespace("my-ns")),
					Group:     ptr.To(gatewayv1.Group("my-group")),
					Kind:      ptr.To(gatewayv1.Kind("Service")),
					Port:      ptr.To(gatewayv1.PortNumber(8080)),
				},
			},
			wantName:   "my-ns_my-service_my-group_Service_8080",
			wantErr:    false,
			wantReason: "",
		},
		{
			name:             "valid backendRef with default fields",
			defaultNamespace: "default",
			backendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "my-service",
					Port: ptr.To(gatewayv1.PortNumber(80)),
				},
			},
			wantName:   "default_my-service_core_Service_80",
			wantErr:    false,
			wantReason: "",
		},
		{
			name:             "unsupported kind",
			defaultNamespace: "default",
			backendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "my-service",
					Kind: ptr.To(gatewayv1.Kind("UnsupportedKind")),
					Port: ptr.To(gatewayv1.PortNumber(80)),
				},
			},
			wantName:   "",
			wantErr:    true,
			wantReason: string(gatewayv1.RouteReasonInvalidKind),
		},
		{
			name:             "missing port",
			defaultNamespace: "default",
			backendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: "my-service",
					Port: nil,
				},
			},
			wantName:   "",
			wantErr:    true,
			wantReason: string(gatewayv1.RouteReasonUnsupportedProtocol),
		},
		{
			name:             "specified namespace",
			defaultNamespace: "default",
			backendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name:      "my-service",
					Namespace: ptr.To(gatewayv1.Namespace("other-ns")),
					Port:      ptr.To(gatewayv1.PortNumber(80)),
				},
			},
			wantName:   "other-ns_my-service_core_Service_80",
			wantErr:    false,
			wantReason: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, err := backendRefToClusterName(tt.defaultNamespace, tt.backendRef)

			if (err != nil) != tt.wantErr {
				t.Errorf("backendRefToClusterName() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				var controllerErr *ControllerError
				if errors.As(err, &controllerErr) {
					if controllerErr.Reason != tt.wantReason {
						t.Errorf("backendRefToClusterName() error reason = %s, wantReason %s", controllerErr.Reason, tt.wantReason)
					}
				} else {
					t.Errorf("backendRefToClusterName() expected a ControllerError, but got %T", err)
				}
			}

			if gotName != tt.wantName {
				t.Errorf("backendRefToClusterName() gotName = %v, want %v", gotName, tt.wantName)
			}
		})
	}
}

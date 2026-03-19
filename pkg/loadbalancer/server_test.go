package loadbalancer

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
)

func TestIsIPv6Service(t *testing.T) {
	tests := []struct {
		name     string
		service  *v1.Service
		expected bool
	}{
		{
			name:     "nil service",
			service:  nil,
			expected: false,
		},
		{
			name: "IPv4-only service",
			service: &v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
				},
			},
			expected: false,
		},
		{
			name: "IPv6-only service",
			service: &v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv6Protocol},
				},
			},
			expected: true,
		},
		{
			name: "dual-stack service",
			service: &v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			expected: true,
		},
		{
			name:     "no IPFamilies set",
			service:  &v1.Service{},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := isIPv6Service(test.service)
			if actual != test.expected {
				t.Errorf("expected %v, got %v", test.expected, actual)
			}
		})
	}
}

func TestListenAddressForService(t *testing.T) {
	tests := []struct {
		name     string
		service  *v1.Service
		expected string
	}{
		{
			name:     "no IPFamilies set",
			service:  &v1.Service{},
			expected: "",
		},
		{
			name: "IPv4-only service",
			service: &v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
				},
			},
			expected: "0.0.0.0",
		},
		{
			name: "IPv6-only service",
			service: &v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv6Protocol},
				},
			},
			expected: "::",
		},
		{
			name: "dual-stack service",
			service: &v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			expected: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := listenAddressForService(test.service)
			if actual != test.expected {
				t.Errorf("expected %q, got %q", test.expected, actual)
			}
		})
	}
}

func TestLoadBalancerName(t *testing.T) {
	tests := []struct {
		name        string
		cluster     string
		service     *v1.Service
		expected    string
		expectedLen int
	}{
		{
			name:        "simple",
			cluster:     "test-cluster",
			service:     &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "test-namespace", Name: "test-service"}},
			expected:    constants.ContainerPrefix + "-11ab7482a104",
			expectedLen: 20,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := loadBalancerName(test.cluster, test.service)
			if actual != test.expected {
				t.Errorf("expected %q, got %q", test.expected, actual)
			}
			if len(actual) != test.expectedLen {
				t.Errorf("expected length %d, got %d", test.expectedLen, len(actual))
			}
		})
	}
}

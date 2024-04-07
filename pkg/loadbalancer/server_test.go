package loadbalancer

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
)

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
			expected:    constants.ContainerPrefix + "-CGVXJAVBASN2Z3RXOABMYVHNP7WNHR3ATSDVOTEN",
			expectedLen: 48,
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

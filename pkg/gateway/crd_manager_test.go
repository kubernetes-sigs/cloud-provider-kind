package gateway

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/cloud-provider-kind/pkg/config"
)

func TestYAMLFilesApply(t *testing.T) {
	tests := []struct {
		name string
		channel config.GatewayReleaseChannel
	}{
		{
		name: "Stable release channel",
		channel: config.Standard,
		},

		{
			name: "Experimental release channel",
			channel: config.Experimental,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := CRDManager {
				dynamicClient: fake.NewSimpleDynamicClient(runtime.NewScheme()),

			}

			if err := manager.InstallCRDs(t.Context(), tt.channel); err != nil {
				t.Fatalf("couldn't install CRDs: %v", err)

			}
		})

	}
}


package controller

import (
	"testing"

	"k8s.io/client-go/rest"
)

func Test_selectRestConfig(t *testing.T) {
	internal := &rest.Config{Host: "https://internal:6443"}
	external := &rest.Config{Host: "https://external:6443"}

	tests := []struct {
		name           string
		internalConfig *rest.Config
		externalConfig *rest.Config
		host           string
		want           *rest.Config
		wantErr        bool
	}{
		{
			name:           "internal kubeconfig unavailable, external endpoint answered",
			internalConfig: nil,
			externalConfig: external,
			host:           external.Host,
			want:           external,
		},
		{
			name:           "external kubeconfig unavailable, internal endpoint answered",
			internalConfig: internal,
			externalConfig: nil,
			host:           internal.Host,
			want:           internal,
		},
		{
			name:           "both available, host matches internal",
			internalConfig: internal,
			externalConfig: external,
			host:           internal.Host,
			want:           internal,
		},
		{
			name:           "both available, host matches external",
			internalConfig: internal,
			externalConfig: external,
			host:           external.Host,
			want:           external,
		},
		{
			name:           "host matches neither available config",
			internalConfig: internal,
			externalConfig: external,
			host:           "https://other:6443",
			wantErr:        true,
		},
		{
			name:           "only external available but host does not match",
			internalConfig: nil,
			externalConfig: external,
			host:           internal.Host,
			wantErr:        true,
		},
		{
			name:           "both configs unavailable",
			internalConfig: nil,
			externalConfig: nil,
			host:           internal.Host,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectRestConfig(tt.internalConfig, tt.externalConfig, tt.host)
			if (err != nil) != tt.wantErr {
				t.Fatalf("selectRestConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("selectRestConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

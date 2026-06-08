package gateway

import "testing"

func TestGenerateEnvoyConfigTable(t *testing.T) {
	tests := []struct {
		name           string
		configData     *configData
		expectedConfig string
		wantErr        bool
	}{
		{
			name: "Default Configuration",
			configData: &configData{
				Cluster:             "test-cluster",
				ID:                  "test-id",
				AdminPort:           9000,
				ControlPlaneAddress: "192.168.1.10",
				ControlPlanePort:    8080,
			},
			expectedConfig: `node:
  cluster: test-cluster
  id: test-id

dynamic_resources:
  ads_config:
    api_type: GRPC
    grpc_services:
    - envoy_grpc:
        cluster_name: xds_cluster
  cds_config:
    ads: {}
  lds_config:
    ads: {}

static_resources:
  clusters:
  - type: STRICT_DNS
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options: {}
    name: xds_cluster
    load_assignment:
      cluster_name: xds_cluster
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: 192.168.1.10
                port_value: 8080
        - endpoint:
            address:
              socket_address:
                address: host.docker.internal
                port_value: 8080
        - endpoint:
            address:
              socket_address:
                address: host.lima.internal
                port_value: 8080

admin:
  access_log_path: /dev/stdout
  address:
    socket_address:
      address: 0.0.0.0
      port_value: 9000
`,
			wantErr: false,
		},
		{
			name: "Different Ports and Addresses",
			configData: &configData{
				Cluster:             "another-cluster",
				ID:                  "instance-01",
				AdminPort:           12345,
				ControlPlaneAddress: "10.0.1.5",
				ControlPlanePort:    50051,
			},
			expectedConfig: `node:
  cluster: another-cluster
  id: instance-01

dynamic_resources:
  ads_config:
    api_type: GRPC
    grpc_services:
    - envoy_grpc:
        cluster_name: xds_cluster
  cds_config:
    ads: {}
  lds_config:
    ads: {}

static_resources:
  clusters:
  - type: STRICT_DNS
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options: {}
    name: xds_cluster
    load_assignment:
      cluster_name: xds_cluster
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: 10.0.1.5
                port_value: 50051
        - endpoint:
            address:
              socket_address:
                address: host.docker.internal
                port_value: 50051
        - endpoint:
            address:
              socket_address:
                address: host.lima.internal
                port_value: 50051

admin:
  access_log_path: /dev/stdout
  address:
    socket_address:
      address: 0.0.0.0
      port_value: 12345
`,
			wantErr: false,
		},
		{
			name: "Dual Stack Admin",
			configData: &configData{
				Cluster:             "test-cluster",
				ID:                  "test-id",
				AdminPort:           9000,
				AdminAddress:        "::",
				AdminIPv4Compat:     true,
				ControlPlaneAddress: "192.168.1.10",
				ControlPlanePort:    8080,
			},
			expectedConfig: `node:
  cluster: test-cluster
  id: test-id

dynamic_resources:
  ads_config:
    api_type: GRPC
    grpc_services:
    - envoy_grpc:
        cluster_name: xds_cluster
  cds_config:
    ads: {}
  lds_config:
    ads: {}

static_resources:
  clusters:
  - type: STRICT_DNS
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options: {}
    name: xds_cluster
    load_assignment:
      cluster_name: xds_cluster
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: 192.168.1.10
                port_value: 8080
        - endpoint:
            address:
              socket_address:
                address: host.docker.internal
                port_value: 8080
        - endpoint:
            address:
              socket_address:
                address: host.lima.internal
                port_value: 8080

admin:
  access_log_path: /dev/stdout
  address:
    socket_address:
      address: "::"
      port_value: 9000
      ipv4_compat: true
`,
			wantErr: false,
		},
		{
			name: "Empty Cluster and ID",
			configData: &configData{
				Cluster:             "",
				ID:                  "",
				AdminPort:           80,
				ControlPlaneAddress: "localhost",
				ControlPlanePort:    8080,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := generateEnvoyConfig(tt.configData)
			if (err != nil) != tt.wantErr {
				t.Errorf("generateEnvoyConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if config != tt.expectedConfig {
				t.Errorf("generateEnvoyConfig() got = \n%v\nwant = \n%v", config, tt.expectedConfig)
			}
		})
	}
}

func TestAdminPortPublishArg(t *testing.T) {
	tests := []struct {
		name        string
		ipv6Enabled bool
		expected    string
	}{
		{
			name:        "ipv4 only",
			ipv6Enabled: false,
			expected:    "--publish=127.0.0.1::10000/tcp",
		},
		{
			name:        "ipv6 enabled",
			ipv6Enabled: true,
			expected:    "--publish=10000/tcp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := adminPortPublishArg(tt.ipv6Enabled, envoyAdminPort)
			if actual != tt.expected {
				t.Fatalf("expected %q, got %q", tt.expected, actual)
			}
		})
	}
}

func TestAdminConfigForGateway(t *testing.T) {
	tests := []struct {
		name           string
		listenAddress  string
		expectedAddr   string
		expectedCompat bool
	}{
		{
			name:           "ipv4 only",
			listenAddress:  "0.0.0.0",
			expectedAddr:   "0.0.0.0",
			expectedCompat: false,
		},
		{
			name:           "ipv6 only",
			listenAddress:  "::",
			expectedAddr:   "::",
			expectedCompat: true,
		},
		{
			name:           "dual stack or unspecified",
			listenAddress:  "",
			expectedAddr:   "::",
			expectedCompat: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, compat := adminConfigForGateway(tt.listenAddress)
			if addr != tt.expectedAddr || compat != tt.expectedCompat {
				t.Fatalf("expected (%q, %t), got (%q, %t)", tt.expectedAddr, tt.expectedCompat, addr, compat)
			}
		})
	}
}

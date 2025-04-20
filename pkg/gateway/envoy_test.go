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

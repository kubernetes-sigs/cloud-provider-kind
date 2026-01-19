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
				Cluster:     "test-cluster",
				ID:          "test-id",
				AdminPort:   9000,
				EnvoySocket: "/var/run/test-envoy-1.sock",
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
  - type: STATIC
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
              pipe:
                path: /var/run/test-envoy-1.sock

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
				Cluster:     "another-cluster",
				ID:          "instance-01",
				AdminPort:   12345,
				EnvoySocket: "/var/run/test-envoy-2.sock",
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
  - type: STATIC
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
              pipe:
                path: /var/run/test-envoy-2.sock

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
				Cluster:     "",
				ID:          "",
				AdminPort:   80,
				EnvoySocket: "/var/run/test-envoy-3",
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

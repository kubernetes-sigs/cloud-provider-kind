package gateway

import (
	"bytes"
	"fmt"
	"text/template"
)

// keep in sync with dynamicControlPlaneConfig
const (
	proxyConfigPath = "/home/envoy/envoy.yaml"
	envoyAdminPort  = 10000

	// well known dns to reach host from containers
	// https://github.com/containerd/nerdctl/issues/747
	dockerInternal = "host.docker.internal"
	limaInternal   = "host.lima.internal"
)

// https://www.envoyproxy.io/docs/envoy/latest/start/quick-start/configuration-dynamic-control-plane
const dynamicControlPlaneConfig = `node:
  cluster: {{ .Cluster }}
  id: {{ .Id }}

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
                address: {{ .ControlPlaneAddress }}
                port_value: {{ .ControlPlanePort }}
        - endpoint:
            address:
              socket_address:
                address: host.docker.internal
                port_value: {{ .ControlPlanePort }}
        - endpoint:
            address:
              socket_address:
                address: host.lima.internal
                port_value: {{ .ControlPlanePort }}

admin:
  access_log_path: /dev/stdout
  address:
    socket_address:
      address: 0.0.0.0
      port_value: {{ .AdminPort }}
`

type configData struct {
	Cluster             string
	Id                  string
	AdminPort           int
	ControlPlaneAddress string
	ControlPlanePort    int
}

// generateEnvoyConfig returns an envoy config generated from config data
func generateEnvoyConfig(data *configData) (config string, err error) {
	if data.Cluster == "" ||
		data.Id == "" ||
		data.AdminPort == 0 ||
		data.ControlPlaneAddress == "" ||
		data.ControlPlanePort == 0 {
		return "", fmt.Errorf("missing parameters")
	}

	t, err := template.New("gateway-config").Parse(dynamicControlPlaneConfig)
	if err != nil {
		return "", fmt.Errorf("failed to parse config template: %w", err)
	}
	// execute the template
	var buff bytes.Buffer
	err = t.Execute(&buff, data)
	if err != nil {
		return "", fmt.Errorf("error executing config template: %w", err)
	}
	return buff.String(), nil
}

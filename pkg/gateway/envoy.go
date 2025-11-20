package gateway

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"text/template"

	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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
  id: {{ .ID }}

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
	ID                  string
	AdminPort           int
	ControlPlaneAddress string
	ControlPlanePort    int
}

// generateEnvoyConfig returns an envoy config generated from config data
func generateEnvoyConfig(data *configData) (string, error) {
	if data.Cluster == "" ||
		data.ID == "" ||
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

// gatewayName name is a unique name for the gateway container
func gatewayName(clusterName, namespace, name string) string {
	h := sha256.New()
	_, err := io.WriteString(h, gatewaySimpleName(clusterName, namespace, name))
	if err != nil {
		panic(err)
	}
	hash := h.Sum(nil)
	return fmt.Sprintf("%s-gw-%x", constants.ContainerPrefix, hash[:6])
}

func gatewaySimpleName(clusterName, namespace, name string) string {
	return clusterName + "/" + namespace + "/" + name
}

// createGateway create a docker container with a gateway
func createGateway(clusterName string, nameserver string, localAddress string, localPort int, gateway *gatewayv1.Gateway, enableTunnel bool) error {
	name := gatewayName(clusterName, gateway.Namespace, gateway.Name)
	simpleName := gatewaySimpleName(clusterName, gateway.Namespace, gateway.Name)
	envoyConfigData := &configData{
		ID:                  name,
		Cluster:             simpleName,
		AdminPort:           envoyAdminPort,
		ControlPlaneAddress: localAddress,
		ControlPlanePort:    localPort,
	}
	dynamicFilesystemConfig, err := generateEnvoyConfig(envoyConfigData)
	if err != nil {
		return err
	}
	networkName := constants.FixedNetworkName
	if n := os.Getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK"); n != "" {
		networkName = n
	}

	args := []string{
		"--detach",
		"--tty",
		"--user=0",
		"--label", fmt.Sprintf("%s=%s", constants.NodeCCMLabelKey, clusterName),
		"--label", fmt.Sprintf("%s=%s", constants.GatewayNameLabelKey, simpleName),
		"--net", networkName,
		"--dns", nameserver,
		"--init=false",
		"--hostname", name,
		"--privileged",
		"--restart=on-failure",
		"--sysctl=net.ipv4.ip_forward=1",
		"--sysctl=net.ipv4.conf.all.rp_filter=0",
		"--sysctl=net.ipv4.ip_unprivileged_port_start=1",
	}

	// support to specify addresses
	// only the first of each IP family will be used
	var ipv4, ipv6 string
	for _, address := range gateway.Spec.Addresses {
		if address.Type != nil && *address.Type != gatewayv1.IPAddressType {
			continue
		}
		ip, err := netip.ParseAddr(address.Value)
		if err != nil {
			continue
		}
		if ip.Is4() {
			if ipv4 == "" {
				ipv4 = address.Value
				args = append(args, "--ip", ip.String())
			}
		} else if ip.Is6() {
			if ipv6 == "" {
				ipv6 = address.Value
				args = append(args, "--ip6", ip.String())
			}
		}
	}

	args = append(args, []string{
		"--sysctl=net.ipv6.conf.all.disable_ipv6=0",
		"--sysctl=net.ipv6.conf.all.forwarding=1",
	}...)

	if enableTunnel {
		for _, listener := range gateway.Spec.Listeners {
			if listener.Protocol == gatewayv1.UDPProtocolType {
				args = append(args, fmt.Sprintf("--publish=%d/%s", listener.Port, "udp"))
			} else {
				args = append(args, fmt.Sprintf("--publish=%d/%s", listener.Port, "tcp"))
			}
		}
	}
	args = append(args, fmt.Sprintf("--publish=%d/tcp", envoyAdminPort))
	args = append(args, "--publish-all")

	// Construct the multi-step command
	var startupCmd strings.Builder
	startupCmd.WriteString(fmt.Sprintf("echo -en '%s' > %s && ", dynamicFilesystemConfig, proxyConfigPath))
	startupCmd.WriteString(fmt.Sprintf("while true; do envoy -c %s && break; sleep 1; done", proxyConfigPath))

	args = append(args, config.DefaultConfig.ProxyImage)
	cmd := []string{"bash", "-c", startupCmd.String()}
	args = append(args, cmd...)

	klog.V(2).Infof("creating gateway with parameters: %v", args)
	err = container.Create(name, args)
	if err != nil {
		return fmt.Errorf("failed to create containers %s %v: %w", name, args, err)
	}

	return nil
}

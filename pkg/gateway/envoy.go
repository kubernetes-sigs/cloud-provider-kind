package gateway

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"text/template"

	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	"sigs.k8s.io/cloud-provider-kind/pkg/images"
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
func generateEnvoyConfig(data *configData) (config string, err error) {
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
func createGateway(clusterName string, localAddress string, localPort int, gateway *gatewayv1.Gateway, enableTunnel bool) error {
	name := gatewayName(clusterName, gateway.Namespace, gateway.Name)
	simpleName := gatewaySimpleName(clusterName, gateway.Namespace, gateway.Name)
	envoyConfig := &configData{
		ID:                  name,
		Cluster:             simpleName,
		AdminPort:           envoyAdminPort,
		ControlPlaneAddress: localAddress,
		ControlPlanePort:    localPort,
	}
	dynamicFilesystemConfig, err := generateEnvoyConfig(envoyConfig)
	if err != nil {
		return err
	}
	networkName := constants.FixedNetworkName
	if n := os.Getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK"); n != "" {
		networkName = n
	}

	args := []string{
		"--detach", // run the container detached
		"--tty",    // allocate a tty for entrypoint logs
		// label the node with the cluster ID
		"--label", fmt.Sprintf("%s=%s", constants.NodeCCMLabelKey, clusterName),
		// label the node with the load balancer name
		"--label", fmt.Sprintf("%s=%s", constants.GatewayNameLabelKey, simpleName),
		// user a user defined docker network so we get embedded DNS
		"--net", networkName,
		"--init=false",
		"--hostname", name, // make hostname match container name
		// label the node with the role ID
		// running containers in a container requires privileged
		// NOTE: we could try to replicate this with --cap-add, and use less
		// privileges, but this flag also changes some mounts that are necessary
		// including some ones docker would otherwise do by default.
		// for now this is what we want. in the future we may revisit this.
		"--privileged",
		"--restart=on-failure",                           // to deal with the crash casued by https://github.com/envoyproxy/envoy/issues/34195
		"--sysctl=net.ipv4.ip_forward=1",                 // allow ip forwarding
		"--sysctl=net.ipv4.conf.all.rp_filter=0",         // disable rp filter
		"--sysctl=net.ipv4.ip_unprivileged_port_start=1", // Allow lower port numbers for podman (see https://github.com/containers/podman/blob/main/rootless.md for more info)
	}

	// allow IPv6 always
	args = append(args, []string{
		"--sysctl=net.ipv6.conf.all.disable_ipv6=0", // enable IPv6
		"--sysctl=net.ipv6.conf.all.forwarding=1",   // allow ipv6 forwarding})
	}...)

	if enableTunnel {
		// Forward the Service Ports to the host so they are accessible on Mac and Windows
		for _, listener := range gateway.Spec.Listeners {
			if listener.Protocol == gatewayv1.UDPProtocolType {
				args = append(args, fmt.Sprintf("--publish=%d/%s", listener.Port, "udp"))
			} else {
				args = append(args, fmt.Sprintf("--publish=%d/%s", listener.Port, "tcp"))
			}
		}
	}
	// publish the admin endpoint
	args = append(args, fmt.Sprintf("--publish=%d/tcp", envoyAdminPort))
	// Publish all ports in the host in random ports
	args = append(args, "--publish-all")

	args = append(args, images.Images["proxy"])
	// we need to override the default envoy configuration
	// https://www.envoyproxy.io/docs/envoy/latest/start/quick-start/configuration-dynamic-filesystem
	// envoy crashes in some circumstances, causing the container to restart, the problem is that the container
	// may come with a different IP and we don't update the status, we may do it, but applications does not use
	// to handle that the assigned LoadBalancerIP changes.
	// https://github.com/envoyproxy/envoy/issues/34195
	cmd := []string{"bash", "-c",
		fmt.Sprintf(`echo -en '%s' > %s && while true; do envoy -c %s && break; sleep 1; done`,
			dynamicFilesystemConfig, proxyConfigPath, proxyConfigPath)}
	args = append(args, cmd...)
	klog.V(2).Infof("creating gateway with parameters: %v", args)
	err = container.Create(name, args)
	if err != nil {
		return fmt.Errorf("failed to create continers %s %v: %w", name, args, err)
	}

	return nil
}

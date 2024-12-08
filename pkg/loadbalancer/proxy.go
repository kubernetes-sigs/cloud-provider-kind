package loadbalancer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"

	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"
)

// keep in sync with dynamicFilesystemConfig
const (
	proxyConfigPath    = "/home/envoy/envoy.yaml"
	proxyConfigPathCDS = "/home/envoy/cds.yaml"
	proxyConfigPathLDS = "/home/envoy/lds.yaml"
	envoyAdminPort     = 10000
)

// start Envoy with dynamic configuration by using files that implement the xDS protocol.
// https://www.envoyproxy.io/docs/envoy/latest/start/quick-start/configuration-dynamic-filesystem
const dynamicFilesystemConfig = `node:
  cluster: cloud-provider-kind
  id: cloud-provider-kind-id

dynamic_resources:
  cds_config:
    resource_api_version: V3
    path: /home/envoy/cds.yaml
  lds_config:
    resource_api_version: V3
    path: /home/envoy/lds.yaml

admin:
  access_log_path: /dev/stdout
  address:
    socket_address:
      address: 0.0.0.0
      port_value: 10000
`

// proxyConfigData is supplied to the loadbalancer config template
type proxyConfigData struct {
	HealthCheckPort int                    // is the same for all ServicePorts
	ServicePorts    map[string]servicePort // key is the IP family and Port and Protocol to support MultiPort services
	SessionAffinity string
	SourceRanges    []sourceRange
}

type sourceRange struct {
	Prefix string
	Length int
}

type servicePort struct {
	// frontend
	Listener endpoint
	// backend
	Cluster []endpoint
}

type endpoint struct {
	Address  string
	Port     int
	Protocol string
}

// proxyLDSConfigTemplate is the loadbalancer config template for listeners
const proxyLDSConfigTemplate = `
resources:
{{- range $index, $servicePort := .ServicePorts }}
- "@type": type.googleapis.com/envoy.config.listener.v3.Listener
  name: listener_{{$index}}
  address:
    socket_address:
      address: {{ $servicePort.Listener.Address }}
      port_value: {{ $servicePort.Listener.Port }}
      protocol: {{ $servicePort.Listener.Protocol }}
  {{- if eq $servicePort.Listener.Protocol "UDP"}}
  udp_listener_config:
    downstream_socket_config:
      max_rx_datagram_size: 9000
  listener_filters:
  - name: envoy.filters.udp_listener.udp_proxy
    typed_config:
      '@type': type.googleapis.com/envoy.extensions.filters.udp.udp_proxy.v3.UdpProxyConfig
      access_log:
      - name: envoy.file_access_log
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
      stat_prefix: udp_proxy
      matcher:
        on_no_match:
          action:
            name: route
            typed_config:
              '@type': type.googleapis.com/envoy.extensions.filters.udp.udp_proxy.v3.Route
              cluster: cluster_{{$index}}
      {{- if eq $.SessionAffinity "ClientIP"}}
      hash_policies:
        source_ip: true
      {{- end}}
      upstream_socket_config:
        max_rx_datagram_size: 9000
  {{- else }}
  filter_chains:
  - filters:
    - name: envoy.filters.network.tcp_proxy
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
        access_log:
        - name: envoy.file_access_log
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
        stat_prefix: tcp_proxy
        cluster: cluster_{{$index}}
        {{- if eq $.SessionAffinity "ClientIP"}}
        hash_policy:
          source_ip: {}
        {{- end}}
  {{- if len $.SourceRanges }}
    filter_chain_match:
      source_prefix_ranges:
      {{- range $sr := $.SourceRanges }}
      - address_prefix: "{{ $sr.Prefix }}"
        prefix_len: {{ $sr.Length }}
      {{- end }}
  {{- end }}
  {{- end}}
{{- end }}
`

// proxyCDSConfigTemplate is the loadbalancer config template for clusters
// https://www.envoyproxy.io/docs/envoy/latest/api-v3/config/core/v3/health_check.proto#envoy-v3-api-msg-config-core-v3-healthcheck-httphealthcheck
const proxyCDSConfigTemplate = `
resources:
{{- range $index, $servicePort := .ServicePorts }}
- "@type": type.googleapis.com/envoy.config.cluster.v3.Cluster
  name: cluster_{{$index}}
  connect_timeout: 5s
  type: STATIC
  {{- if eq $.SessionAffinity "ClientIP"}}
  lb_policy: RING_HASH
  {{- else}}
  lb_policy: RANDOM
  {{- end}}
  health_checks:
  - timeout: 5s
    interval: 3s
    unhealthy_threshold: 2
    healthy_threshold: 1
    no_traffic_interval: 5s
    always_log_health_check_failures: true
    always_log_health_check_success: true
    event_log_path: /dev/stdout
    http_health_check:
      path: /healthz
  load_assignment:
    cluster_name: cluster_{{$index}}
    endpoints:
    {{- range $address := $servicePort.Cluster }}
      - lb_endpoints:
        - endpoint:
            health_check_config:
              port_value: {{ $.HealthCheckPort }}
            address:
              socket_address:
                address: {{ $address.Address }}
                port_value: {{ $address.Port }}
                protocol: {{ $address.Protocol }}
    {{- end}}
{{- end }}
`

// proxyConfig returns a kubeadm config generated from config data, in particular
// the kubernetes version
func proxyConfig(configTemplate string, data *proxyConfigData) (config string, err error) {
	t, err := template.New("loadbalancer-config").Parse(configTemplate)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse config template")
	}
	// execute the template
	var buff bytes.Buffer
	err = t.Execute(&buff, data)
	if err != nil {
		return "", errors.Wrap(err, "error executing config template")
	}
	return buff.String(), nil
}

func generateConfig(service *v1.Service, nodes []*v1.Node) *proxyConfigData {
	if service == nil {
		return nil
	}
	hcPort := 10256 // kube-proxy default port
	if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		hcPort = int(service.Spec.HealthCheckNodePort)
	}

	lbConfig := &proxyConfigData{
		HealthCheckPort: hcPort,
		SessionAffinity: string(service.Spec.SessionAffinity),
	}

	servicePortConfig := map[string]servicePort{}
	for _, ipFamily := range service.Spec.IPFamilies {
		for _, port := range service.Spec.Ports {
			if port.Protocol != v1.ProtocolTCP && port.Protocol != v1.ProtocolUDP {
				klog.Infof("service port protocol %s not supported", port.Protocol)
				continue
			}
			key := fmt.Sprintf("%s_%d_%s", ipFamily, port.Port, port.Protocol)
			bind := `0.0.0.0`
			if ipFamily == v1.IPv6Protocol {
				bind = `"::"`
			}

			backends := []endpoint{}
			for _, n := range nodes {
				for _, addr := range n.Status.Addresses {
					// only internal IPs supported
					if addr.Type != v1.NodeInternalIP {
						klog.V(2).Infof("address type %s, only %s supported", addr.Type, v1.NodeInternalIP)
						continue
					}
					// only addresses that match the Service IP family
					if (netutils.IsIPv4String(addr.Address) && ipFamily != v1.IPv4Protocol) ||
						(netutils.IsIPv6String(addr.Address) && ipFamily != v1.IPv6Protocol) {
						continue
					}
					backends = append(backends, endpoint{Address: addr.Address, Port: int(port.NodePort), Protocol: string(port.Protocol)})
				}
			}

			servicePortConfig[key] = servicePort{
				Listener: endpoint{Address: bind, Port: int(port.Port), Protocol: string(port.Protocol)},
				Cluster:  backends,
			}
		}
	}
	lbConfig.ServicePorts = servicePortConfig

	for _, sr := range service.Spec.LoadBalancerSourceRanges {
		// This is validated (though the validation mistakenly allows whitespace)
		// so we don't bother dealing with parse failures.
		_, cidr, _ := netutils.ParseCIDRSloppy(strings.TrimSpace(sr))
		if cidr != nil {
			len, _ := cidr.Mask.Size()
			lbConfig.SourceRanges = append(lbConfig.SourceRanges,
				sourceRange{
					Prefix: cidr.IP.String(),
					Length: len,
				},
			)
		}
	}

	klog.V(2).Infof("envoy config info: %+v", lbConfig)
	return lbConfig
}

// TODO: move to xDS via GRPC instead of having to deal with files
func proxyUpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	if service == nil {
		return nil
	}
	var stdout, stderr bytes.Buffer
	name := loadBalancerName(clusterName, service)
	config := generateConfig(service, nodes)
	// create loadbalancer config data
	ldsConfig, err := proxyConfig(proxyLDSConfigTemplate, config)
	if err != nil {
		return errors.Wrap(err, "failed to generate loadbalancer config data")
	}

	klog.V(2).Infof("updating loadbalancer with config %s", ldsConfig)
	err = container.Exec(name, []string{"cp", "/dev/stdin", proxyConfigPathLDS + ".tmp"}, strings.NewReader(ldsConfig), &stdout, &stderr)
	if err != nil {
		return err
	}

	cdsConfig, err := proxyConfig(proxyCDSConfigTemplate, config)
	if err != nil {
		return errors.Wrap(err, "failed to generate loadbalancer config data")
	}

	klog.V(2).Infof("updating loadbalancer with config %s", cdsConfig)
	err = container.Exec(name, []string{"cp", "/dev/stdin", proxyConfigPathCDS + ".tmp"}, strings.NewReader(cdsConfig), &stdout, &stderr)
	if err != nil {
		return err
	}
	// envoy has an initialization process until starts to forward traffic
	// https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/init#arch-overview-initialization
	// also wait for the healthchecks and "no_traffic_interval"
	cmd := fmt.Sprintf(`chmod a+rw /home/envoy/* && mv %s %s && mv %s %s`, proxyConfigPathCDS+".tmp", proxyConfigPathCDS, proxyConfigPathLDS+".tmp", proxyConfigPathLDS)
	err = container.Exec(name, []string{"bash", "-c", cmd}, nil, &stdout, &stderr)
	if err != nil {
		return fmt.Errorf("error updating configuration Stdout: %s Stderr: %s : %w", stdout.String(), stderr.String(), err)
	}
	return waitLoadBalancerReady(ctx, name, 30*time.Second)
}

func waitLoadBalancerReady(ctx context.Context, name string, timeout time.Duration) error {
	portmaps, err := container.PortMaps(name)
	if err != nil {
		return err
	}

	var authority string
	if config.DefaultConfig.ControlPlaneConnectivity == config.Direct {
		ipv4, _, err := container.IPs(name)
		if err != nil {
			return err
		}
		authority = net.JoinHostPort(ipv4, strconv.Itoa(envoyAdminPort))
	} else {
		port, ok := portmaps[strconv.Itoa(envoyAdminPort)]
		if !ok {
			return fmt.Errorf("envoy admin port %d not found, got %v", envoyAdminPort, portmaps)
		}
		authority = net.JoinHostPort("127.0.0.1", port)
	}

	httpClient := http.DefaultClient
	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, timeout, true, func(ctx context.Context) (done bool, err error) {
		// iptables port forwarding on localhost only works for IPv4
		resp, err := httpClient.Get(fmt.Sprintf("http://%s/ready", authority))
		if err != nil {
			klog.V(2).Infof("unexpected error trying to get load balancer %s readiness :%v", name, err)
			return false, nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			klog.V(2).Infof("unexpected status code from load balancer %s expected LIVE got %d", name, resp.StatusCode)
			return false, nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			klog.V(2).Infof("unexpected error trying to get load balancer %s readiness :%v", name, err)
			return false, nil
		}

		response := strings.TrimSpace(string(body))
		if response != "LIVE" {
			klog.V(2).Infof("unexpected ready response from load balancer %s expected LIVE got %s", name, response)
			return false, nil
		}
		return true, nil
	})
	return err
}

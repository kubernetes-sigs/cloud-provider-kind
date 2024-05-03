package loadbalancer

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"

	"sigs.k8s.io/cloud-provider-kind/pkg/container"
)

// proxyImage defines the loadbalancer image:tag
const proxyImage = "envoyproxy/envoy:v1.30.1"

// proxyConfigPath defines the path to the config file in the image
const proxyConfigPath = "/etc/envoy/envoy.yaml"

// proxyConfigData is supplied to the loadbalancer config template
type proxyConfigData struct {
	HealthCheckPort int             // is the same for all ServicePorts
	ServicePorts    map[string]data // key is the IP family and Port to support MultiPort services
}

type data struct {
	// frontend
	Listener endpoint
	// backend
	Cluster []endpoint // key: node name  value: IP:Port
}

type endpoint struct {
	Address string
	Port    int
}

// proxyDefaultConfigTemplate is the loadbalancer config template
const proxyDefaultConfigTemplate = `
admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: 9901 }

static_resources:
  listeners:
  {{- range $index, $data := .ServicePorts }}
  - name: listener_{{$index}}
    address:
      socket_address: { address: {{ $data.Listener.Address }}, port_value: {{ $data.Listener.Port }} }
    filter_chains:
      - filters:
        - name: envoy.filters.network.tcp_proxy
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
            stat_prefix: destination
            cluster: cluster_{{$index}}
  {{- end }}

  clusters:
  {{- range $index, $data := .ServicePorts }}
  - name: cluster_{{$index}}
    connect_timeout: 0.25s
    type: STATIC
    lb_policy: ROUND_ROBIN
    load_assignment:
      cluster_name: cluster_{{$index}}
      endpoints:
	  {{- range $address := $data.Cluster }}
        - lb_endpoints:
          - endpoint:
              health_check_config:
                port_value: {{ $.HealthCheckPort  }}
              address:
                socket_address:
                  address: {{ $address.Address }}
                  port_value: {{ $address.Port }}
      {{- end}}
  {{- end }}
`

// proxyConfig returns a kubeadm config generated from config data, in particular
// the kubernetes version
func proxyConfig(data *proxyConfigData) (config string, err error) {
	t, err := template.New("loadbalancer-config").Parse(proxyDefaultConfigTemplate)
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
	}

	servicePortConfig := map[string]data{}
	for _, ipFamily := range service.Spec.IPFamilies {
		// TODO: support UDP
		for _, port := range service.Spec.Ports {
			if port.Protocol != v1.ProtocolTCP {
				klog.Infof("service port protocol %s not supported", port.Protocol)
				continue
			}
			key := fmt.Sprintf("%s_%d", string(ipFamily), port.Port)
			bind := `0.0.0.0`
			if ipFamily == v1.IPv6Protocol {
				bind = `::`
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
					backends = append(backends, endpoint{addr.Address, int(port.NodePort)})
				}
			}

			servicePortConfig[key] = data{
				Listener: endpoint{bind, int(port.Port)},
				Cluster:  backends,
			}
		}
	}
	lbConfig.ServicePorts = servicePortConfig
	klog.V(2).Infof("haproxy config info: %+v", lbConfig)
	return lbConfig
}

func proxyUpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	if service == nil {
		return nil
	}
	config := generateConfig(service, nodes)
	// create loadbalancer config data
	loadbalancerConfig, err := proxyConfig(config)
	if err != nil {
		return errors.Wrap(err, "failed to generate loadbalancer config data")
	}

	klog.V(2).Infof("updating loadbalancer with config %s", loadbalancerConfig)
	var stdout, stderr bytes.Buffer
	name := loadBalancerName(clusterName, service)
	err = container.Exec(name, []string{"cp", "/dev/stdin", proxyConfigPath}, strings.NewReader(loadbalancerConfig), &stdout, &stderr)
	if err != nil {
		return err
	}

	klog.V(2).Infof("restarting loadbalancer")
	err = container.Signal(name, "HUP")
	if err != nil {
		return err
	}

	return nil
}

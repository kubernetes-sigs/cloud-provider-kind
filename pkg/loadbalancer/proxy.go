package loadbalancer

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"

	"sigs.k8s.io/cloud-provider-kind/pkg/container"
)

// proxyImage defines the loadbalancer image:tag
const proxyImage = "docker.io/kindest/haproxy:v20230606-42a2262b"

// proxyConfigPath defines the path to the config file in the image
const proxyConfigPath = "/usr/local/etc/haproxy/haproxy.cfg"

// proxyConfigData is supplied to the loadbalancer config template
type proxyConfigData struct {
	HealthCheckPort int             // is the same for all ServicePorts
	ServicePorts    map[string]data // key is the IP family and Port to support MultiPort services
}

type data struct {
	// frontend
	BindAddress string // *:Port for IPv4 :::Port for IPv6
	// backend
	Backends map[string]string // key: node name  value: IP:Port
}

// proxyDefaultConfigTemplate is the loadbalancer config template
const proxyDefaultConfigTemplate = `
global
  log /dev/log local0
  log /dev/log local1 notice
  daemon

resolvers docker
  nameserver dns 127.0.0.11:53

defaults
  log global
  mode tcp
  option dontlognull
  # TODO: tune these
  timeout connect 5000
  timeout client 50000
  timeout server 50000
  # allow to boot despite dns don't resolve backends
  default-server init-addr none

{{ range $index, $data := .ServicePorts }}
frontend {{$index}}-frontend
  bind {{ $data.BindAddress }}
  default_backend {{$index}}-backend

backend {{$index}}-backend
  option httpchk GET /healthz
  {{- range $server, $address := $data.Backends }}
  server {{ $server }} {{ $address }} check port {{ $.HealthCheckPort  }} inter 5s fall 3 rise 1
  {{- end}}
{{ end }}
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
			bind := `*`
			if ipFamily == v1.IPv6Protocol {
				bind = `::`
			}
			servicePortConfig[key] = data{
				BindAddress: fmt.Sprintf("%s:%d", bind, port.Port),
				Backends:    map[string]string{},
			}

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
					servicePortConfig[key].Backends[n.Name] = net.JoinHostPort(addr.Address, strconv.Itoa(int(port.NodePort)))
				}
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

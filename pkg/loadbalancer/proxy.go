package loadbalancer

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"
)

// proxyImage defines the loadbalancer image:tag
const proxyImage = "kindest/haproxy:v20230330-2f738c2"

// proxyConfigPath defines the path to the config file in the image
const proxyConfigPath = "/usr/local/etc/haproxy/haproxy.cfg"

// proxyConfigData is supplied to the loadbalancer config template
type proxyConfigData struct {
	ServicePorts    []string
	HealthCheckPort int
	BackendServers  map[string]string
	IPv6            bool
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

frontend service
{{ range $index, $port := .ServicePorts }}  bind *:{{ $port }}{{end}}
  default_backend nodes

backend nodes
  option httpchk GET /healthz
  {{- $hcport := .HealthCheckPort -}}
  {{- range $server, $address := .BackendServers }}
  server {{ $server }} {{ $address }} check port {{ $hcport }} inter 5s fall 3 rise 1
  {{- end}}
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

func proxyUpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	name := loadBalancerName(clusterName, service)
	if service == nil {
		return nil
	}
	config := &proxyConfigData{
		HealthCheckPort: 10256, // kube-proxy default port
		BackendServers:  map[string]string{},
		ServicePorts:    []string{},
	}
	if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		config.HealthCheckPort = int(service.Spec.HealthCheckNodePort)
	}

	backendsV4 := map[string]string{}
	backendsV6 := map[string]string{}
	for _, n := range nodes {
		for _, addr := range n.Status.Addresses {
			if addr.Type == v1.NodeInternalIP {
				if netutils.IsIPv4String(addr.Address) {
					backendsV4[n.Name] = addr.Address
				} else if netutils.IsIPv6String(addr.Address) {
					backendsV6[n.Name] = addr.Address
				}
			}
		}
	}

	// TODO: support UDP and IPv6
	for _, port := range service.Spec.Ports {
		if port.Protocol != v1.ProtocolTCP {
			continue
		}
		config.ServicePorts = append(config.ServicePorts, strconv.Itoa(int(port.Port)))
		for server, address := range backendsV4 {
			config.BackendServers[server] = net.JoinHostPort(address, strconv.Itoa(int(port.NodePort)))
		}
	}

	// create loadbalancer config data
	loadbalancerConfig, err := proxyConfig(config)
	if err != nil {
		return errors.Wrap(err, "failed to generate loadbalancer config data")
	}

	klog.V(2).Infof("updating loadbalancer with config %s", loadbalancerConfig)
	var stdout, stderr bytes.Buffer
	err = execContainer(name, []string{"cp", "/dev/stdin", proxyConfigPath}, strings.NewReader(loadbalancerConfig), &stdout, &stderr)
	if err != nil {
		return err
	}

	klog.V(2).Infof("restarting loadbalancer")
	err = containerSignal(name, "HUP")
	if err != nil {
		return err
	}

	return nil
}

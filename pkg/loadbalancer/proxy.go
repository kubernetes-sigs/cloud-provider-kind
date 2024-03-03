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

	"sigs.k8s.io/cloud-provider-kind/pkg/container"
)

// proxyImage defines the loadbalancer image:tag
const proxyImage = "docker.io/kindest/haproxy:v20230606-42a2262b"

// proxyConfigPath defines the path to the config file in the image
const proxyConfigPath = "/usr/local/etc/haproxy/haproxy.cfg"

// proxyConfigData is supplied to the loadbalancer config template
type proxyConfigData struct {
	ServicePorts    []string
	HealthCheckPort int
	BackendServers  map[string][]backendServer
	IPv6            bool
	IPv4            bool
}

type backendServer struct {
	Name    string
	Address string
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

{{- if .IPv4}}
frontend ipv4-frontend
{{- $bind := "*:" }}
{{- range $index, $port := .ServicePorts }} 
  bind {{$bind}}{{ $port }}  
{{- end}} 
  default_backend ipv4-backend
{{end}}

{{- if .IPv6}}
frontend ipv6-frontend
{{- $bind := ":::"}}
{{- range $index, $port := .ServicePorts }} 
  bind {{$bind}}{{ $port }}  
{{- end}} 
  default_backend ipv6-backend
{{end}}

{{- if .IPv4}}
backend ipv4-backend
  option httpchk GET /healthz
  {{- $hcport := .HealthCheckPort -}}
  {{- range $i, $server := index .BackendServers "IPv4" }}
  server {{ $server.Name }} {{ $server.Address }} check port {{ $hcport }} inter 5s fall 3 rise 1
  {{- end}}
{{end}}

{{- if .IPv6}}
backend ipv6-backend
  option httpchk GET /healthz
  {{- $hcport := .HealthCheckPort -}}
  {{- range $i, $server := index .BackendServers "IPv6" }}
  server {{ $server.Name }} {{ $server.Address }} check port {{ $hcport }} inter 5s fall 3 rise 1
  {{- end}}
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
		BackendServers:  map[string][]backendServer{},
		ServicePorts:    []string{},
	}

	config.IPv6 = len(service.Spec.IPFamilies) == 2 || service.Spec.IPFamilies[0] == v1.IPv6Protocol
	config.IPv4 = len(service.Spec.IPFamilies) == 2 || service.Spec.IPFamilies[0] == v1.IPv4Protocol
	if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeLocal {
		config.HealthCheckPort = int(service.Spec.HealthCheckNodePort)
	}

	backends := map[string][]backendServer{}
	for _, n := range nodes {
		for _, addr := range n.Status.Addresses {
			if addr.Type == v1.NodeInternalIP {
				if netutils.IsIPv4String(addr.Address) {
					backends[string(v1.IPv4Protocol)] = append(backends[string(v1.IPv4Protocol)], backendServer{Name: n.Name, Address: addr.Address})
				} else if netutils.IsIPv6String(addr.Address) {
					backends[string(v1.IPv6Protocol)] = append(backends[string(v1.IPv6Protocol)], backendServer{Name: n.Name, Address: addr.Address})
				}
			}
		}
	}

	// TODO: support UDP
	for _, port := range service.Spec.Ports {
		if port.Protocol != v1.ProtocolTCP {
			continue
		}
		config.ServicePorts = append(config.ServicePorts, strconv.Itoa(int(port.Port)))

		for _, be := range backends {
			for i := range be {
				be[i].Address = net.JoinHostPort(be[i].Address, strconv.Itoa(int(port.NodePort)))
			}
		}
	}
	config.BackendServers = backends
	klog.V(2).Infof("backend servers info: %v", backends)

	// create loadbalancer config data
	loadbalancerConfig, err := proxyConfig(config)
	if err != nil {
		return errors.Wrap(err, "failed to generate loadbalancer config data")
	}

	klog.V(2).Infof("updating loadbalancer with config %s", loadbalancerConfig)
	var stdout, stderr bytes.Buffer
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

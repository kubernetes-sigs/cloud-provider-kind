package loadbalancer

import (
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func makeNode(name string, ip string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.NodeSpec{},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: ip},
			},
		},
	}
}

func makeService(name string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{
				{Port: 80},
			},
		},
	}
}

func Test_generateConfig(t *testing.T) {
	tests := []struct {
		name    string
		service *v1.Service
		nodes   []*v1.Node
		want    *proxyConfigData
	}{
		{
			name: "empty",
		},
		{
			name: "simple service",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test",
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
					IPFamilies:            []v1.IPFamily{v1.IPv4Protocol},
					Ports: []v1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: 8080},
							NodePort:   30000,
							Protocol:   v1.ProtocolTCP,
						},
					},
					HealthCheckNodePort: 32000,
				},
			},
			nodes: []*v1.Node{
				makeNode("a", "10.0.0.1"),
				makeNode("b", "10.0.0.2"),
			},
			want: &proxyConfigData{
				HealthCheckPort: 32000,
				ServicePorts: map[string]data{
					"IPv4_80": data{BindAddress: "*:80", Backends: map[string]string{"a": "10.0.0.1:30000", "b": "10.0.0.2:30000"}},
				},
			},
		},
		{
			name: "multiport service",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test",
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
					IPFamilies:            []v1.IPFamily{v1.IPv4Protocol},
					Ports: []v1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: 8080},
							NodePort:   30000,
							Protocol:   v1.ProtocolTCP,
						},
						{
							Port:       443,
							TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: 8080},
							NodePort:   31000,
							Protocol:   v1.ProtocolTCP,
						},
					},
					HealthCheckNodePort: 32000,
				},
			},
			nodes: []*v1.Node{
				makeNode("a", "10.0.0.1"),
				makeNode("b", "10.0.0.2"),
			},
			want: &proxyConfigData{
				HealthCheckPort: 32000,
				ServicePorts: map[string]data{
					"IPv4_80":  data{BindAddress: "*:80", Backends: map[string]string{"a": "10.0.0.1:30000", "b": "10.0.0.2:30000"}},
					"IPv4_443": data{BindAddress: "*:443", Backends: map[string]string{"a": "10.0.0.1:31000", "b": "10.0.0.2:31000"}},
				},
			},
		},
		{
			name: "multiport service ipv6",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test",
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
					IPFamilies:            []v1.IPFamily{v1.IPv6Protocol},
					Ports: []v1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: 8080},
							NodePort:   30000,
							Protocol:   v1.ProtocolTCP,
						},
						{
							Port:       443,
							TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: 8080},
							NodePort:   31000,
							Protocol:   v1.ProtocolTCP,
						},
					},
					HealthCheckNodePort: 32000,
				},
			},
			nodes: []*v1.Node{
				makeNode("a", "2001:db2::3"),
				makeNode("b", "2001:db2::4"),
			},
			want: &proxyConfigData{
				HealthCheckPort: 32000,
				ServicePorts: map[string]data{
					"IPv6_80":  data{BindAddress: `:::80`, Backends: map[string]string{"a": "[2001:db2::3]:30000", "b": "[2001:db2::4]:30000"}},
					"IPv6_443": data{BindAddress: `:::443`, Backends: map[string]string{"a": "[2001:db2::3]:31000", "b": "[2001:db2::4]:31000"}},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := generateConfig(tt.service, tt.nodes); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("generateConfig() = %+v,\n want %+v", got, tt.want)
			}
		})
	}
}

func Test_proxyConfig(t *testing.T) {
	// Only to check the templating figure out how to assert better on the output
	t.Skip()
	tests := []struct {
		name       string
		data       *proxyConfigData
		wantConfig string
	}{
		{
			name: "ipv4",
			data: &proxyConfigData{
				HealthCheckPort: 32764,
				ServicePorts: map[string]data{
					"IPv4_80": data{BindAddress: "*:80", Backends: map[string]string{
						"kind-worker":  "192.168.8.2:30497",
						"kind-worker2": "192.168.8.3:30497",
					}},
					"IPv4_443": data{BindAddress: "*:443", Backends: map[string]string{
						"kind-worker":  "192.168.8.2:31497",
						"kind-worker2": "192.168.8.3:31497",
					}},
				},
			},
			wantConfig: `
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

frontend IPv4_443-frontend
	bind *:443
	default_backend IPv4_443-backend

backend IPv4_443-backend
	option httpchk GET /healthz
	server kind-worker 192.168.8.2:31497 check port 32764 inter 5s fall 3 rise 1
	server kind-worker2 192.168.8.3:31497 check port 32764 inter 5s fall 3 rise 1

frontend IPv4_80-frontend
	bind *:80
	default_backend IPv4_80-backend

backend IPv4_80-backend
	option httpchk GET /healthz
	server kind-worker 192.168.8.2:30497 check port 32764 inter 5s fall 3 rise 1
	server kind-worker2 192.168.8.3:30497 check port 32764 inter 5s fall 3 rise 1
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotConfig, err := proxyConfig(tt.data)
			if err != nil {
				t.Errorf("proxyConfig() error = %v", err)
				return
			}
			if gotConfig != tt.wantConfig {
				t.Errorf("proxyConfig() = %v , want %v", gotConfig, tt.wantConfig)
				t.Errorf("proxyConfig() = %v", cmp.Diff(gotConfig, tt.wantConfig))
			}
		})
	}
}

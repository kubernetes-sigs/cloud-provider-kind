package loadbalancer

import (
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/lithammer/dedent"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
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
				ServicePorts: map[string]servicePort{
					"IPv4_80_TCP": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"10.0.0.1", 30000, string(v1.ProtocolTCP)}, {"10.0.0.2", 30000, string(v1.ProtocolTCP)}},
					},
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
				ServicePorts: map[string]servicePort{
					"IPv4_80_TCP": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"10.0.0.1", 30000, string(v1.ProtocolTCP)}, {"10.0.0.2", 30000, string(v1.ProtocolTCP)}},
					},
					"IPv4_443_TCP": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 443, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"10.0.0.1", 31000, string(v1.ProtocolTCP)}, {"10.0.0.2", 31000, string(v1.ProtocolTCP)}},
					},
				},
			},
		},
		{
			name: "multiport different protocol service",
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
							Port:       80,
							TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: 8080},
							NodePort:   31000,
							Protocol:   v1.ProtocolUDP,
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
				ServicePorts: map[string]servicePort{
					"IPv4_80_TCP": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"10.0.0.1", 30000, string(v1.ProtocolTCP)}, {"10.0.0.2", 30000, string(v1.ProtocolTCP)}},
					},
					"IPv4_80_UDP": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolUDP)},
						Cluster:  []endpoint{{"10.0.0.1", 31000, string(v1.ProtocolUDP)}, {"10.0.0.2", 31000, string(v1.ProtocolUDP)}},
					},
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
				ServicePorts: map[string]servicePort{
					"IPv6_80_TCP": servicePort{
						Listener: endpoint{Address: `"::"`, Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"2001:db2::3", 30000, string(v1.ProtocolTCP)}, {"2001:db2::4", 30000, string(v1.ProtocolTCP)}},
					},
					"IPv6_443_TCP": servicePort{
						Listener: endpoint{Address: `"::"`, Port: 443, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"2001:db2::3", 31000, string(v1.ProtocolTCP)}, {"2001:db2::4", 31000, string(v1.ProtocolTCP)}},
					},
				},
			},
		},
		{
			name: "session affinity",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test",
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyCluster,
					IPFamilies:            []v1.IPFamily{v1.IPv4Protocol},
					Ports: []v1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: 8080},
							NodePort:   30000,
							Protocol:   v1.ProtocolTCP,
						},
					},
					SessionAffinity: v1.ServiceAffinityClientIP,
					SessionAffinityConfig: &v1.SessionAffinityConfig{
						ClientIP: &v1.ClientIPConfig{
							// FIXME: This is currently ignored
							TimeoutSeconds: ptr.To[int32](60),
						},
					},
				},
			},
			nodes: []*v1.Node{
				makeNode("a", "10.0.0.1"),
			},
			want: &proxyConfigData{
				HealthCheckPort: 10256,
				ServicePorts: map[string]servicePort{
					"IPv4_80_TCP": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"10.0.0.1", 30000, string(v1.ProtocolTCP)}},
					},
				},
				SessionAffinity: "ClientIP",
			},
		},
		{
			name: "source ranges",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test",
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyCluster,
					IPFamilies:            []v1.IPFamily{v1.IPv4Protocol},
					Ports: []v1.ServicePort{
						{
							Port:       80,
							TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: 8080},
							NodePort:   30000,
							Protocol:   v1.ProtocolTCP,
						},
					},
					LoadBalancerSourceRanges: []string{
						"10.0.0.0/8",
						// This is "valid".
						" 192.168.0.0/16  ",
					},
				},
			},
			nodes: []*v1.Node{
				makeNode("a", "10.0.0.1"),
			},
			want: &proxyConfigData{
				HealthCheckPort: 10256,
				ServicePorts: map[string]servicePort{
					"IPv4_80_TCP": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"10.0.0.1", 30000, string(v1.ProtocolTCP)}},
					},
				},
				SourceRanges: []sourceRange{
					{Prefix: "10.0.0.0", Length: 8},
					{Prefix: "192.168.0.0", Length: 16},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := generateConfig(logr.Discard(), tt.service, tt.nodes); !reflect.DeepEqual(got, tt.want) {
				t.Logf("diff %+v", cmp.Diff(got, tt.want))
				t.Errorf("generateConfig() = %+v,\n want %+v", got, tt.want)
			}
		})
	}
}

func Test_proxyConfig(t *testing.T) {
	tests := []struct {
		name       string
		template   string
		data       *proxyConfigData
		wantConfig string
	}{
		{
			name:     "ipv4 CDS",
			template: proxyCDSConfigTemplate,
			data: &proxyConfigData{
				HealthCheckPort: 32764,
				ServicePorts: map[string]servicePort{
					"IPv4_80": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"192.168.8.2", 30497, string(v1.ProtocolTCP)}, {"192.168.8.3", 30497, string(v1.ProtocolTCP)}},
					},
					"IPv4_443": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 443, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"192.168.8.2", 31497, string(v1.ProtocolTCP)}, {"192.168.8.3", 31497, string(v1.ProtocolTCP)}},
					},
				},
			},
			wantConfig: `
				resources:
				- "@type": type.googleapis.com/envoy.config.cluster.v3.Cluster
				  name: cluster_IPv4_443
				  connect_timeout: 5s
				  type: STATIC
				  lb_policy: RANDOM
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
				    cluster_name: cluster_IPv4_443
				    endpoints:
				      - lb_endpoints:
				        - endpoint:
				            health_check_config:
				              port_value: 32764
				            address:
				              socket_address:
				                address: 192.168.8.2
				                port_value: 31497
				                protocol: TCP
				      - lb_endpoints:
				        - endpoint:
				            health_check_config:
				              port_value: 32764
				            address:
				              socket_address:
				                address: 192.168.8.3
				                port_value: 31497
				                protocol: TCP
				- "@type": type.googleapis.com/envoy.config.cluster.v3.Cluster
				  name: cluster_IPv4_80
				  connect_timeout: 5s
				  type: STATIC
				  lb_policy: RANDOM
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
				    cluster_name: cluster_IPv4_80
				    endpoints:
				      - lb_endpoints:
				        - endpoint:
				            health_check_config:
				              port_value: 32764
				            address:
				              socket_address:
				                address: 192.168.8.2
				                port_value: 30497
				                protocol: TCP
				      - lb_endpoints:
				        - endpoint:
				            health_check_config:
				              port_value: 32764
				            address:
				              socket_address:
				                address: 192.168.8.3
				                port_value: 30497
				                protocol: TCP
				`,
		},
		{
			name:     "ipv4 LDS",
			template: proxyLDSConfigTemplate,
			data: &proxyConfigData{
				HealthCheckPort: 32764,
				ServicePorts: map[string]servicePort{
					"IPv4_80": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"192.168.8.2", 30497, string(v1.ProtocolTCP)}, {"192.168.8.3", 30497, string(v1.ProtocolTCP)}},
					},
					"IPv4_443": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 443, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"192.168.8.2", 31497, string(v1.ProtocolTCP)}, {"192.168.8.3", 31497, string(v1.ProtocolTCP)}},
					},
				},
			},
			wantConfig: `
				resources:
				- "@type": type.googleapis.com/envoy.config.listener.v3.Listener
				  name: listener_IPv4_443
				  address:
				    socket_address:
				      address: 0.0.0.0
				      port_value: 443
				      protocol: TCP
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
				        cluster: cluster_IPv4_443
				- "@type": type.googleapis.com/envoy.config.listener.v3.Listener
				  name: listener_IPv4_80
				  address:
				    socket_address:
				      address: 0.0.0.0
				      port_value: 80
				      protocol: TCP
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
				        cluster: cluster_IPv4_80
			`,
		},
		{
			name:     "ipv4 CDS with affinity",
			template: proxyCDSConfigTemplate,
			data: &proxyConfigData{
				HealthCheckPort: 32764,
				ServicePorts: map[string]servicePort{
					"IPv4_80": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"192.168.8.2", 30497, string(v1.ProtocolTCP)}, {"192.168.8.3", 30497, string(v1.ProtocolTCP)}},
					},
				},
				SessionAffinity: "ClientIP",
			},
			wantConfig: `
				resources:
				- "@type": type.googleapis.com/envoy.config.cluster.v3.Cluster
				  name: cluster_IPv4_80
				  connect_timeout: 5s
				  type: STATIC
				  lb_policy: RING_HASH
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
				    cluster_name: cluster_IPv4_80
				    endpoints:
				      - lb_endpoints:
				        - endpoint:
				            health_check_config:
				              port_value: 32764
				            address:
				              socket_address:
				                address: 192.168.8.2
				                port_value: 30497
				                protocol: TCP
				      - lb_endpoints:
				        - endpoint:
				            health_check_config:
				              port_value: 32764
				            address:
				              socket_address:
				                address: 192.168.8.3
				                port_value: 30497
				                protocol: TCP
				`,
		},
		{
			name:     "ipv4 LDS with affinity",
			template: proxyLDSConfigTemplate,
			data: &proxyConfigData{
				HealthCheckPort: 32764,
				ServicePorts: map[string]servicePort{
					"IPv4_80": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"192.168.8.2", 30497, string(v1.ProtocolTCP)}, {"192.168.8.3", 30497, string(v1.ProtocolTCP)}},
					},
				},
				SessionAffinity: "ClientIP",
			},
			wantConfig: `
				resources:
				- "@type": type.googleapis.com/envoy.config.listener.v3.Listener
				  name: listener_IPv4_80
				  address:
				    socket_address:
				      address: 0.0.0.0
				      port_value: 80
				      protocol: TCP
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
				        cluster: cluster_IPv4_80
				        hash_policy:
				          source_ip: {}
			`,
		},
		{
			name:     "ipv4 LDS with source ranges",
			template: proxyLDSConfigTemplate,
			data: &proxyConfigData{
				HealthCheckPort: 32764,
				ServicePorts: map[string]servicePort{
					"IPv4_80": servicePort{
						Listener: endpoint{Address: "0.0.0.0", Port: 80, Protocol: string(v1.ProtocolTCP)},
						Cluster:  []endpoint{{"192.168.8.2", 30497, string(v1.ProtocolTCP)}, {"192.168.8.3", 30497, string(v1.ProtocolTCP)}},
					},
				},
				SourceRanges: []sourceRange{
					{Prefix: "10.0.0.0", Length: 8},
					{Prefix: "192.168.0.0", Length: 16},
				},
			},
			wantConfig: `
				resources:
				- "@type": type.googleapis.com/envoy.config.listener.v3.Listener
				  name: listener_IPv4_80
				  address:
				    socket_address:
				      address: 0.0.0.0
				      port_value: 80
				      protocol: TCP
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
				        cluster: cluster_IPv4_80
				    filter_chain_match:
				      source_prefix_ranges:
				      - address_prefix: "10.0.0.0"
				        prefix_len: 8
				      - address_prefix: "192.168.0.0"
				        prefix_len: 16
			`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotConfig, err := proxyConfig(tt.template, tt.data)
			if err != nil {
				t.Errorf("proxyConfig() error = %v", err)
				return
			}
			wantConfig := dedent.Dedent(tt.wantConfig)
			if gotConfig != wantConfig {
				t.Logf("%s", gotConfig)
				t.Errorf("proxyConfig() not expected\n%v", cmp.Diff(gotConfig, wantConfig))
			}
		})
	}
}

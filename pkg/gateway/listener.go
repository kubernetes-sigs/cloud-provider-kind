package gateway

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// translateListenerToEnvoyListener translates a Gateway API Listener
// into a map of Envoy resources. Currently, it focuses on creating
// an Envoy Listener resource.
func translateListenerToEnvoyListener(listener gatewayv1.Listener) (map[resourcev3.Type][]interface{}, error) {
	resources := make(map[resourcev3.Type][]interface{})

	// Determine the Envoy protocol based on the Gateway API protocol
	var envoyProto corev3.SocketAddress_Protocol
	switch listener.Protocol {
	case gatewayv1.UDPProtocolType:
		envoyProto = corev3.SocketAddress_UDP
	default: // TCP, HTTP, HTTPS, TLS all use TCP at the transport layer
		envoyProto = corev3.SocketAddress_TCP
	}

	envoyListener := &listenerv3.Listener{
		Name: string(listener.Name),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: envoyProto,
					Address:  "::", // Listen on all interfaces (IPv6 and IPv4)
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: uint32(listener.Port),
					},
				},
			},
		},
	}

	// Configure filters based on the Listener protocol
	switch listener.Protocol {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
		// HTTP/HTTPS listener needs an HTTP connection manager filter
		httpConnectionManager := &hcmv3.HttpConnectionManager{
			StatPrefix: string(listener.Name),
			// The RouteConfig will be dynamically discovered via RDS
			RouteSpecifier: &hcmv3.HttpConnectionManager_Rds{
				Rds: &hcmv3.Rds{
					ConfigSource: &corev3.ConfigSource{
						ConfigSourceSpecifier: &corev3.ConfigSource_ApiConfigSource{
							ApiConfigSource: &corev3.ApiConfigSource{
								ApiType: corev3.ApiConfigSource_GRPC,
								GrpcServices: []*corev3.GrpcService{
									{
										TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
											EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{
												ClusterName: "xds_cluster", // This should be the name of your xDS cluster
											},
										},
									},
								},
							},
						},
						InitialFetchTimeout: nil, // Rely on defaults or configure as needed
					},
					RouteConfigName: string(listener.Name), // Use the listener name as the RDS config name
				},
			},
			HttpFilters: []*hcmv3.HttpFilter{
				// The router filter must be the last in the chain
				{
					Name: wellknown.Router,
					ConfigType: &hcmv3.HttpFilter_TypedConfig{
						TypedConfig: &anypb.Any{
							// Empty config for the router filter
							TypeUrl: "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router",
						},
					},
				},
			},
		}
		hcmAny, err := anypb.New(httpConnectionManager)
		if err != nil {
			return nil, err
		}
		filterChain := &listenerv3.FilterChain{
			Filters: []*listenerv3.Filter{{
				Name: wellknown.HTTPConnectionManager,
				ConfigType: &listenerv3.Filter_TypedConfig{
					TypedConfig: hcmAny,
				},
			}},
		}

		// Add SNI matching if a hostname is specified
		if listener.Hostname != nil && *listener.Hostname != "" {
			filterChain.FilterChainMatch = &listenerv3.FilterChainMatch{
				ServerNames: []string{string(*listener.Hostname)},
			}
		}

		if listener.Protocol == gatewayv1.HTTPSProtocolType || listener.TLS != nil {
			// Configure TLS if it's an HTTPS listener or TLS is explicitly configured
			tlsContext := buildDownstreamTLSContext(listener)
			pbst, err := anypb.New(tlsContext)
			if err != nil {
				return nil, err
			}
			filterChain.TransportSocket = &corev3.TransportSocket{
				Name: wellknown.TransportSocketTls,
				ConfigType: &corev3.TransportSocket_TypedConfig{
					TypedConfig: pbst,
				},
			}
		}
		envoyListener.FilterChains = []*listenerv3.FilterChain{filterChain}

	case gatewayv1.TCPProtocolType:
		// TCP listener needs a pass-through filter
		tcpProxy := &tcpproxyv3.TcpProxy{
			StatPrefix: string(listener.Name),
			ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{
				Cluster: string(listener.Name), // The cluster name will be the listener name for now
			},
		}
		pbst, err := anypb.New(tcpProxy)
		if err != nil {
			return nil, err
		}
		envoyListener.FilterChains = []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name: wellknown.TCPProxy,
				ConfigType: &listenerv3.Filter_TypedConfig{
					TypedConfig: pbst,
				},
			}},
		}}

		// For TCP, we also need to create a corresponding cluster
		/*
			resources[resourcev3.ClusterType] = append(resources[resourcev3.ClusterType], &clusterv3.Cluster{
				Name: string(listener.Name),
				// You'll likely need to configure the connect timeout and other cluster parameters
				ConnectTimeout: nil, // Set an appropriate timeout
				ClusterDiscoveryType: &clusterv3.Cluster_Type{
					Type: clusterv3.Cluster_EDS, // Or STATIC if you have fixed endpoints
				},
				EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
					ConfigSource: &clusterv3.ConfigSource{
						ConfigSourceSpecifier: &clusterv3.ConfigSource_ApiConfigSource{
							ApiConfigSource: &clusterv3.ApiConfigSource{
								ApiType: corev3.ApiConfigSource_GRPC,
								GrpcServices: []*corev3.GrpcService{
									{
										TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
											EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{
												ClusterName: "xds_cluster", // This should be the name of your xDS cluster
											},
										},
									},
								},
							},
						},
						InitialFetchTimeout: nil, // Rely on defaults or configure as needed
					},
					ServiceName: string(listener.Name), // Use the listener name as the EDS service name
				},
			})
		*/

	case gatewayv1.UDPProtocolType:
		// For UDP, we primarily set the correct socket address protocol.
		// Specific UDP proxying or handling might require network filters,
		// which would be configured in the FilterChains. If no specific
		// UDP proxy is needed, an empty FilterChains might be sufficient
		// for just opening the UDP port.
		envoyListener.FilterChains = []*listenerv3.FilterChain{}
		// You might need to add network filters here if you require specific
		// UDP processing (e.g., a custom UDP proxy).
	}

	resources[resourcev3.ListenerType] = append(resources[resourcev3.ListenerType], envoyListener)

	return resources, nil
}

func buildDownstreamTLSContext(listener gatewayv1.Listener) *tlsv3.DownstreamTlsContext {
	if listener.TLS == nil {
		return &tlsv3.DownstreamTlsContext{}
	}

	downstreamTLSContext := &tlsv3.DownstreamTlsContext{
		CommonTlsContext: &tlsv3.CommonTlsContext{},
	}

	if listener.TLS.CertificateRefs != nil {
		for _, certRef := range listener.TLS.CertificateRefs {
			// Check Kind: Default is "Secret"
			refKind := gatewayv1.Kind("Secret")
			if certRef.Kind != nil {
				refKind = *certRef.Kind
			}
			// Check Group: Default is "" (core group)
			refGroup := gatewayv1.Group("")
			if certRef.Group != nil {
				refGroup = *certRef.Group
			}
			if refKind == "Secret" && refGroup == "" {
				downstreamTLSContext.CommonTlsContext.TlsCertificates = []*tlsv3.TlsCertificate{{
					CertificateChain: &corev3.DataSource{},
					PrivateKey:       &corev3.DataSource{},
				}}
				break // For now, just take the first valid certificate ref
			}
			// Handle other kinds and groups if needed
		}
	}

	if listener.TLS.Mode != nil && *listener.TLS.Mode == gatewayv1.TLSModeTerminate {
		downstreamTLSContext.RequireClientCertificate = &wrapperspb.BoolValue{
			Value: true,
		}
	}

	return downstreamTLSContext
}

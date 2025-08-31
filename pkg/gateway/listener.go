package gateway

import (
	"fmt"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	udpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// translateListener translates a Gateway API Listener and its associated routes into an Envoy Listener and RouteConfiguration.
func (c *Controller) translateListener(gateway *gatewayv1.Gateway, listener gatewayv1.Listener, virtualHost *routev3.VirtualHost) (*listenerv3.Listener, *routev3.RouteConfiguration) {
	routeConfigName := fmt.Sprintf("%s-%s", gateway.Name, listener.Name)
	routeConfiguration := &routev3.RouteConfiguration{
		Name:         routeConfigName,
		VirtualHosts: []*routev3.VirtualHost{virtualHost},
	}

	envoyListener := &listenerv3.Listener{
		Name:    routeConfigName,
		Address: &corev3.Address{},
	}

	var protocol corev3.SocketAddress_Protocol
	switch listener.Protocol {
	case gatewayv1.UDPProtocolType:
		protocol = corev3.SocketAddress_UDP
	default:
		protocol = corev3.SocketAddress_TCP
	}

	envoyListener.Address = &corev3.Address{
		Address: &corev3.Address_SocketAddress{
			SocketAddress: &corev3.SocketAddress{
				Protocol: protocol,
				Address:  "0.0.0.0",
				PortSpecifier: &corev3.SocketAddress_PortValue{
					PortValue: uint32(listener.Port),
				},
			},
		},
	}

	switch listener.Protocol {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
		routerProto := &routerv3.Router{}
		routerAny, err := anypb.New(routerProto)
		if err != nil {
			klog.Errorf("Failed to marshal router config: %v", err)
			return nil, nil
		}

		httpConnectionManager := &hcmv3.HttpConnectionManager{
			StatPrefix: routeConfigName,
			RouteSpecifier: &hcmv3.HttpConnectionManager_Rds{
				Rds: &hcmv3.Rds{
					ConfigSource: &corev3.ConfigSource{
						ResourceApiVersion: corev3.ApiVersion_V3,
						ConfigSourceSpecifier: &corev3.ConfigSource_Ads{
							Ads: &corev3.AggregatedConfigSource{},
						},
					},
					RouteConfigName: routeConfigName,
				},
			},
			HttpFilters: []*hcmv3.HttpFilter{
				{
					Name: "envoy.filters.http.router",
					ConfigType: &hcmv3.HttpFilter_TypedConfig{
						TypedConfig: routerAny,
					},
				},
			},
		}

		hcmAny, err := anypb.New(httpConnectionManager)
		if err != nil {
			klog.Errorf("Failed to marshal HttpConnectionManager: %v", err)
			return nil, nil
		}

		filterChain := &listenerv3.FilterChain{
			Filters: []*listenerv3.Filter{{
				Name: "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{
					TypedConfig: hcmAny,
				},
			}},
		}

		if listener.Protocol == gatewayv1.HTTPSProtocolType && listener.TLS != nil {
			tlsAny, err := c.buildDownstreamTLSContext(gateway, &listener)
			if err != nil {
				klog.Errorf("Failed to build DownstreamTlsContext: %v", err)
			} else {
				filterChain.TransportSocket = &corev3.TransportSocket{
					Name: "envoy.transport_sockets.tls",
					ConfigType: &corev3.TransportSocket_TypedConfig{
						TypedConfig: tlsAny,
					},
				}
			}
		}
		envoyListener.FilterChains = []*listenerv3.FilterChain{filterChain}

	case gatewayv1.TCPProtocolType, gatewayv1.TLSProtocolType:
		tcpProxy := &tcpproxyv3.TcpProxy{
			StatPrefix: routeConfigName,
			ClusterSpecifier: &tcpproxyv3.TcpProxy_WeightedClusters{
				WeightedClusters: &tcpproxyv3.TcpProxy_WeightedCluster{
					Clusters: []*tcpproxyv3.TcpProxy_WeightedCluster_ClusterWeight{
						// TODO: This needs to be populated from TCPRoute/TLSRoute
					},
				},
			},
		}
		tcpProxyAny, err := anypb.New(tcpProxy)
		if err != nil {
			klog.Errorf("Failed to marshal TcpProxy: %v", err)
			return nil, nil
		}

		filterChain := &listenerv3.FilterChain{
			Filters: []*listenerv3.Filter{{
				Name: "envoy.filters.network.tcp_proxy",
				ConfigType: &listenerv3.Filter_TypedConfig{
					TypedConfig: tcpProxyAny,
				},
			}},
		}

		if listener.Protocol == gatewayv1.TLSProtocolType && listener.TLS != nil {
			tlsAny, err := c.buildDownstreamTLSContext(gateway, &listener)
			if err != nil {
				klog.Errorf("Failed to build DownstreamTlsContext for TLS Listener: %v", err)
			} else {
				filterChain.TransportSocket = &corev3.TransportSocket{
					Name: "envoy.transport_sockets.tls",
					ConfigType: &corev3.TransportSocket_TypedConfig{
						TypedConfig: tlsAny,
					},
				}
			}
		}
		envoyListener.FilterChains = []*listenerv3.FilterChain{filterChain}

	case gatewayv1.UDPProtocolType:
		udpProxy := &udpproxyv3.UdpProxyConfig{
			StatPrefix:     routeConfigName,
			RouteSpecifier: &udpproxyv3.UdpProxyConfig_Matcher{
				// TODO: This needs to be populated from UDPRoute
			},
		}
		udpProxyAny, err := anypb.New(udpProxy)
		if err != nil {
			klog.Errorf("Failed to marshal UdpProxyConfig: %v", err)
			return nil, nil
		}
		envoyListener.ListenerFilters = []*listenerv3.ListenerFilter{
			{
				Name: "envoy.filters.udp_listener.udp_proxy",
				ConfigType: &listenerv3.ListenerFilter_TypedConfig{
					TypedConfig: udpProxyAny,
				},
			},
		}
	}

	return envoyListener, routeConfiguration
}

// buildDownstreamTLSContext builds the DownstreamTlsContext for a listener.
func (c *Controller) buildDownstreamTLSContext(gateway *gatewayv1.Gateway, listener *gatewayv1.Listener) (*anypb.Any, error) {
	if listener.TLS == nil || len(listener.TLS.CertificateRefs) == 0 {
		return nil, fmt.Errorf("TLS is configured, but no certificate refs are provided")
	}

	// TODO: Support multiple certificate refs
	certRef := listener.TLS.CertificateRefs[0]
	if certRef.Kind != nil && *certRef.Kind != "Secret" {
		return nil, fmt.Errorf("unsupported certificate ref kind: %s", *certRef.Kind)
	}
	namespace := gateway.Namespace
	if certRef.Namespace != nil {
		namespace = string(*certRef.Namespace)
	}

	secret, err := c.secretLister.Secrets(namespace).Get(string(certRef.Name))
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", namespace, certRef.Name, err)
	}

	tlsCert, err := toEnvoyTlsCertificate(secret)
	if err != nil {
		return nil, err
	}

	tlsContext := &tlsv3.DownstreamTlsContext{
		CommonTlsContext: &tlsv3.CommonTlsContext{
			TlsCertificates: []*tlsv3.TlsCertificate{tlsCert},
		},
	}
	return anypb.New(tlsContext)
}

// toEnvoyTlsCertificate converts a Kubernetes secret to an Envoy TlsCertificate.
func toEnvoyTlsCertificate(secret *corev1.Secret) (*tlsv3.TlsCertificate, error) {
	privateKey, ok := secret.Data[corev1.TLSPrivateKeyKey]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s does not contain key %s", secret.Namespace, secret.Name, corev1.TLSPrivateKeyKey)
	}
	certChain, ok := secret.Data[corev1.TLSCertKey]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s does not contain key %s", secret.Namespace, secret.Name, corev1.TLSCertKey)
	}

	return &tlsv3.TlsCertificate{
		CertificateChain: &corev3.DataSource{
			Specifier: &corev3.DataSource_InlineBytes{
				InlineBytes: certChain,
			},
		},
		PrivateKey: &corev3.DataSource{
			Specifier: &corev3.DataSource_InlineBytes{
				InlineBytes: privateKey,
			},
		},
	}, nil
}

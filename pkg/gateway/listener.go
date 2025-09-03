package gateway

import (
	"context"
	"encoding/pem"
	"fmt"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	tlsinspector "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	udpproxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"

	"google.golang.org/protobuf/types/known/anypb"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// validateListeners checks for conflicts among all listeners on a Gateway as per the spec.
// It returns a map of conflicted listener conditions and a Gateway-level condition if any conflicts exist.
func (c *Controller) validateListeners(gateway *gatewayv1.Gateway) (
	conflictedListenerConditions map[gatewayv1.SectionName]metav1.Condition,
) {
	conflictedListenerConditions = make(map[gatewayv1.SectionName]metav1.Condition)
	listenersByPort := make(map[gatewayv1.PortNumber][]gatewayv1.Listener)
	for _, listener := range gateway.Spec.Listeners {
		listenersByPort[listener.Port] = append(listenersByPort[listener.Port], listener)
	}

	conflictedListenerNames := []string{}

	for _, listenersOnPort := range listenersByPort {
		// Rule: A TCP listener cannot share a port with HTTP/HTTPS/TLS listeners.
		hasTCP := false
		hasHTTPTLS := false
		for _, listener := range listenersOnPort {
			if listener.Protocol == gatewayv1.TCPProtocolType || listener.Protocol == gatewayv1.UDPProtocolType {
				hasTCP = true
			}
			if listener.Protocol == gatewayv1.HTTPProtocolType || listener.Protocol == gatewayv1.HTTPSProtocolType || listener.Protocol == gatewayv1.TLSProtocolType {
				hasHTTPTLS = true
			}
		}

		if hasTCP && hasHTTPTLS {
			for _, listener := range listenersOnPort {
				conflictedListenerNames = append(conflictedListenerNames, string(listener.Name))
				conflictedListenerConditions[listener.Name] = metav1.Condition{
					Type:    string(gatewayv1.ListenerConditionConflicted),
					Status:  metav1.ConditionTrue,
					Reason:  string(gatewayv1.ListenerReasonProtocolConflict),
					Message: "Protocol conflict: TCP/UDP listeners cannot share a port with HTTP/HTTPS/TLS listeners.",
				}
			}
			continue // Skip further checks for this port
		}

		// Rule: HTTP/HTTPS/TLS listeners on the same port must have unique hostnames.
		seenHostnames := make(map[gatewayv1.Hostname]gatewayv1.SectionName)
		for _, listener := range listenersOnPort {
			// This check only applies to protocols that use hostnames for distinction.
			if listener.Protocol == gatewayv1.HTTPProtocolType || listener.Protocol == gatewayv1.HTTPSProtocolType || listener.Protocol == gatewayv1.TLSProtocolType {
				hostname := gatewayv1.Hostname("")
				if listener.Hostname != nil {
					hostname = *listener.Hostname
				}

				if conflictingListenerName, exists := seenHostnames[hostname]; exists {
					// Found a conflict. Mark both this listener and the one we saw before.
					conflictedListenerNames = append(conflictedListenerNames, string(listener.Name), string(conflictingListenerName))

					conflictedCondition := metav1.Condition{
						Type:    string(gatewayv1.ListenerConditionConflicted),
						Status:  metav1.ConditionTrue,
						Reason:  string(gatewayv1.ListenerReasonHostnameConflict),
						Message: fmt.Sprintf("Hostname '%s' conflicts with another listener on the same port.", hostname),
					}
					conflictedListenerConditions[listener.Name] = conflictedCondition
					conflictedListenerConditions[conflictingListenerName] = conflictedCondition
				} else {
					seenHostnames[hostname] = listener.Name
				}
			}
		}
	}

	return conflictedListenerConditions
}

func (c *Controller) translateListenerToFilterChain(gateway *gatewayv1.Gateway, lis gatewayv1.Listener, virtualHosts []*routev3.VirtualHost, routeName string) (*listener.FilterChain, error) {
	var filterChain *listener.FilterChain

	switch lis.Protocol {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
		routerProto := &routerv3.Router{}
		routerAny, err := anypb.New(routerProto)
		if err != nil {
			klog.Errorf("Failed to marshal router config: %v", err)
			return nil, err
		}

		hcmConfig := &hcm.HttpConnectionManager{
			StatPrefix: string(lis.Name),
			RouteSpecifier: &hcm.HttpConnectionManager_Rds{
				Rds: &hcm.Rds{
					ConfigSource: &corev3.ConfigSource{
						ResourceApiVersion:    corev3.ApiVersion_V3,
						ConfigSourceSpecifier: &corev3.ConfigSource_Ads{Ads: &corev3.AggregatedConfigSource{}},
					},
					RouteConfigName: routeName,
				},
			},
			HttpFilters: []*hcm.HttpFilter{{
				Name: wellknown.Router,
				ConfigType: &hcm.HttpFilter_TypedConfig{
					TypedConfig: routerAny,
				},
			}},
		}
		hcmAny, err := anypb.New(hcmConfig)
		if err != nil {
			return nil, err
		}

		filterChain = &listener.FilterChain{
			Filters: []*listener.Filter{{
				Name: wellknown.HTTPConnectionManager,
				ConfigType: &listener.Filter_TypedConfig{
					TypedConfig: hcmAny,
				},
			}},
		}

	case gatewayv1.TCPProtocolType, gatewayv1.TLSProtocolType:
		// TCP and TLS listeners require a TCP proxy filter.
		// We'll assume for now that routes for these are not supported and it's a direct pass-through.
		tcpProxy := &tcpproxyv3.TcpProxy{
			StatPrefix: string(lis.Name),
			ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{
				Cluster: "some_static_cluster", // This needs to be determined from a TCPRoute/TLSRoute
			},
		}
		tcpProxyAny, err := anypb.New(tcpProxy)
		if err != nil {
			return nil, err
		}
		filterChain = &listener.FilterChain{
			Filters: []*listener.Filter{{
				Name: wellknown.TCPProxy,
				ConfigType: &listener.Filter_TypedConfig{
					TypedConfig: tcpProxyAny,
				},
			}},
		}

	case gatewayv1.UDPProtocolType:
		udpProxy := &udpproxy.UdpProxyConfig{
			StatPrefix: string(lis.Name),
			RouteSpecifier: &udpproxy.UdpProxyConfig_Cluster{
				Cluster: "some_udp_cluster", // This needs to be determined from a UDPRoute
			},
		}
		udpProxyAny, err := anypb.New(udpProxy)
		if err != nil {
			return nil, err
		}
		filterChain = &listener.FilterChain{
			Filters: []*listener.Filter{{
				Name: "envoy.filters.udp_listener.udp_proxy",
				ConfigType: &listener.Filter_TypedConfig{
					TypedConfig: udpProxyAny,
				},
			}},
		}
	}

	// Add SNI matching for applicable protocols
	if lis.Protocol == gatewayv1.HTTPSProtocolType || lis.Protocol == gatewayv1.TLSProtocolType {
		if lis.Hostname != nil && *lis.Hostname != "" {
			filterChain.FilterChainMatch = &listener.FilterChainMatch{
				ServerNames: []string{string(*lis.Hostname)},
			}
		}
		// Configure TLS context
		tlsContext, err := c.buildDownstreamTLSContext(context.Background(), gateway, lis)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS context for listener %s: %w", lis.Name, err)
		}
		if tlsContext != nil {
			filterChain.TransportSocket = &corev3.TransportSocket{
				Name: "envoy.transport_sockets.tls",
				ConfigType: &corev3.TransportSocket_TypedConfig{
					TypedConfig: tlsContext,
				},
			}
		}
	}

	return filterChain, nil
}

func (c *Controller) buildDownstreamTLSContext(ctx context.Context, gateway *gatewayv1.Gateway, lis gatewayv1.Listener) (*anypb.Any, error) {
	if lis.TLS == nil {
		return nil, nil
	}
	if len(lis.TLS.CertificateRefs) == 0 {
		return nil, fmt.Errorf("TLS is configured, but no certificate refs are provided")
	}

	tlsContext := &tlsv3.DownstreamTlsContext{
		CommonTlsContext: &tlsv3.CommonTlsContext{
			TlsCertificates: []*tlsv3.TlsCertificate{},
		},
	}

	for _, certRef := range lis.TLS.CertificateRefs {
		if certRef.Group != nil && *certRef.Group != "" {
			return nil, fmt.Errorf("unsupported certificate ref group: %s", *certRef.Group)
		}
		if certRef.Kind != nil && *certRef.Kind != "Secret" {
			return nil, fmt.Errorf("unsupported certificate ref kind: %s", *certRef.Kind)
		}
		namespace := gateway.Namespace
		if certRef.Namespace != nil {
			namespace = string(*certRef.Namespace)
		}

		secretName := string(certRef.Name)
		secret, err := c.secretLister.Secrets(namespace).Get(secretName)
		if err != nil {
			return nil, fmt.Errorf("failed to get secret %s/%s: %w", namespace, secretName, err)
		}

		tlsCert, err := toEnvoyTlsCertificate(secret)
		if err != nil {
			return nil, fmt.Errorf("failed to convert secret to tls certificate: %v", err)
		}
		tlsContext.CommonTlsContext.TlsCertificates = append(tlsContext.CommonTlsContext.TlsCertificates, tlsCert)
	}

	any, err := anypb.New(tlsContext)
	if err != nil {
		return nil, err
	}
	return any, nil
}

func toEnvoyTlsCertificate(secret *corev1.Secret) (*tlsv3.TlsCertificate, error) {
	privateKey, ok := secret.Data[corev1.TLSPrivateKeyKey]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s does not contain key %s", secret.Namespace, secret.Name, corev1.TLSPrivateKeyKey)
	}
	block, _ := pem.Decode(privateKey)
	if block == nil {
		return nil, fmt.Errorf("secret %s/%s key %s does not contain a valid PEM-encoded private key", secret.Namespace, secret.Name, corev1.TLSPrivateKeyKey)
	}

	certChain, ok := secret.Data[corev1.TLSCertKey]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s does not contain key %s", secret.Namespace, secret.Name, corev1.TLSCertKey)
	}
	block, _ = pem.Decode(certChain)
	if block == nil {
		return nil, fmt.Errorf("secret %s/%s key %s does not contain a valid PEM-encoded certificate chain", secret.Namespace, secret.Name, corev1.TLSCertKey)
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

func createEnvoyAddress(port uint32) *corev3.Address {
	return &corev3.Address{
		Address: &corev3.Address_SocketAddress{
			SocketAddress: &corev3.SocketAddress{
				Protocol: corev3.SocketAddress_TCP,
				Address:  "0.0.0.0",
				PortSpecifier: &corev3.SocketAddress_PortValue{
					PortValue: port,
				},
			},
		},
	}
}

func createListenerFilters() []*listener.ListenerFilter {
	tlsInspectorConfig, _ := anypb.New(&tlsinspector.TlsInspector{})
	return []*listener.ListenerFilter{
		{
			Name: wellknown.TlsInspector,
			ConfigType: &listener.ListenerFilter_TypedConfig{
				TypedConfig: tlsInspectorConfig,
			},
		},
	}
}

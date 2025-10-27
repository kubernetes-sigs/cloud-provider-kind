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
	"google.golang.org/protobuf/types/known/wrapperspb"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// setListenerCondition is a helper to safely set a condition on a listener's status
// in a map of conditions.
func setListenerCondition(
	conditionsMap map[gatewayv1.SectionName][]metav1.Condition,
	listenerName gatewayv1.SectionName,
	condition metav1.Condition,
) {
	// This "get, modify, set" pattern is the standard way to
	// work around the Go constraint that map values are not addressable.
	conditions := conditionsMap[listenerName]
	if conditions == nil {
		conditions = []metav1.Condition{}
	}
	meta.SetStatusCondition(&conditions, condition)
	conditionsMap[listenerName] = conditions
}

// validateListeners checks for conflicts among all listeners on a Gateway as per the spec.
// It returns a map of conflicted listener conditions and a Gateway-level condition if any conflicts exist.
func (c *Controller) validateListeners(gateway *gatewayv1.Gateway) map[gatewayv1.SectionName][]metav1.Condition {
	listenerConditions := make(map[gatewayv1.SectionName][]metav1.Condition)
	for _, listener := range gateway.Spec.Listeners {
		// Initialize with a fresh slice.
		listenerConditions[listener.Name] = []metav1.Condition{}
	}

	// Check for Port and Hostname Conflicts
	listenersByPort := make(map[gatewayv1.PortNumber][]gatewayv1.Listener)
	for _, listener := range gateway.Spec.Listeners {
		listenersByPort[listener.Port] = append(listenersByPort[listener.Port], listener)
	}

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
				setListenerCondition(listenerConditions, listener.Name, metav1.Condition{
					Type:    string(gatewayv1.ListenerConditionConflicted),
					Status:  metav1.ConditionTrue,
					Reason:  string(gatewayv1.ListenerReasonProtocolConflict),
					Message: "Protocol conflict: TCP/UDP listeners cannot share a port with HTTP/HTTPS/TLS listeners.",
				})
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
					conflictedCondition := metav1.Condition{
						Type:    string(gatewayv1.ListenerConditionConflicted),
						Status:  metav1.ConditionTrue,
						Reason:  string(gatewayv1.ListenerReasonHostnameConflict),
						Message: fmt.Sprintf("Hostname '%s' conflicts with another listener on the same port.", hostname),
					}
					setListenerCondition(listenerConditions, listener.Name, conflictedCondition)
					setListenerCondition(listenerConditions, conflictingListenerName, conflictedCondition)
				} else {
					seenHostnames[hostname] = listener.Name
				}
			}
		}
	}

	for _, listener := range gateway.Spec.Listeners {
		// If a listener is already conflicted, we don't need to check its secrets.
		if meta.IsStatusConditionTrue(listenerConditions[listener.Name], string(gatewayv1.ListenerConditionConflicted)) {
			continue
		}

		if listener.TLS == nil {
			// No TLS config, so no secrets to resolve. This listener is considered resolved.
			setListenerCondition(listenerConditions, listener.Name, metav1.Condition{
				Type:    string(gatewayv1.ListenerConditionResolvedRefs),
				Status:  metav1.ConditionTrue,
				Reason:  string(gatewayv1.ListenerReasonResolvedRefs),
				Message: "All references resolved",
			})
			continue
		}

		for _, certRef := range listener.TLS.CertificateRefs {
			if certRef.Group != nil && *certRef.Group != "" {
				setListenerCondition(listenerConditions, listener.Name, metav1.Condition{
					Type:    string(gatewayv1.ListenerConditionResolvedRefs),
					Status:  metav1.ConditionFalse,
					Reason:  string(gatewayv1.ListenerReasonInvalidCertificateRef),
					Message: fmt.Sprintf("unsupported certificate ref grup: %s", *certRef.Group),
				})
				break
			}

			if certRef.Kind != nil && *certRef.Kind != "Secret" {
				setListenerCondition(listenerConditions, listener.Name, metav1.Condition{
					Type:    string(gatewayv1.ListenerConditionResolvedRefs),
					Status:  metav1.ConditionFalse,
					Reason:  string(gatewayv1.ListenerReasonInvalidCertificateRef),
					Message: fmt.Sprintf("unsupported certificate ref kind: %s", *certRef.Kind),
				})
				break
			}

			secretNamespace := gateway.Namespace
			if certRef.Namespace != nil {
				secretNamespace = string(*certRef.Namespace)
			}

			if secretNamespace != gateway.Namespace {
				from := gatewayv1beta1.ReferenceGrantFrom{
					Group:     gatewayv1.GroupName,
					Kind:      "Gateway",
					Namespace: gatewayv1.Namespace(gateway.Namespace),
				}
				to := gatewayv1beta1.ReferenceGrantTo{
					Group: "", // Core group for Secret
					Kind:  "Secret",
					Name:  &certRef.Name,
				}
				if !isCrossNamespaceRefAllowed(from, to, secretNamespace, c.referenceGrantLister) {
					setListenerCondition(listenerConditions, listener.Name, metav1.Condition{
						Type:    string(gatewayv1.ListenerConditionResolvedRefs),
						Status:  metav1.ConditionFalse,
						Reason:  string(gatewayv1.ListenerReasonRefNotPermitted),
						Message: fmt.Sprintf("reference to Secret %s/%s not permitted by any ReferenceGrant", secretNamespace, certRef.Name),
					})
					break
				}
			}

			secret, err := c.secretLister.Secrets(secretNamespace).Get(string(certRef.Name))
			if err != nil {
				setListenerCondition(listenerConditions, listener.Name, metav1.Condition{
					Type:    string(gatewayv1.ListenerConditionResolvedRefs),
					Status:  metav1.ConditionFalse,
					Reason:  string(gatewayv1.ListenerReasonInvalidCertificateRef),
					Message: fmt.Sprintf("reference to Secret %s/%s not found", secretNamespace, certRef.Name),
				})
				break
			}
			if err := validateSecretCertificate(secret); err != nil {
				setListenerCondition(listenerConditions, listener.Name, metav1.Condition{
					Type:    string(gatewayv1.ListenerConditionResolvedRefs),
					Status:  metav1.ConditionFalse,
					Reason:  string(gatewayv1.ListenerReasonInvalidCertificateRef),
					Message: fmt.Sprintf("malformed Secret %s/%s : %v", secretNamespace, certRef.Name, err.Error()),
				})
				break
			}
		}

		// Set the ResolvedRefs condition based on the outcome of the secret validation.
		if !meta.IsStatusConditionFalse(listenerConditions[listener.Name], string(gatewayv1.ListenerConditionResolvedRefs)) {
			setListenerCondition(listenerConditions, listener.Name, metav1.Condition{
				Type:    string(gatewayv1.ListenerConditionResolvedRefs),
				Status:  metav1.ConditionTrue,
				Reason:  string(gatewayv1.ListenerReasonResolvedRefs),
				Message: "All references resolved",
			})
		}
	}

	return listenerConditions
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
			// Enable X-Forwarded-For header
			// https://github.com/kubernetes-sigs/cloud-provider-kind/issues/296
			UseRemoteAddress: &wrapperspb.BoolValue{Value: true},
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

		secretNamespace := gateway.Namespace
		if certRef.Namespace != nil {
			secretNamespace = string(*certRef.Namespace)
		}

		secretName := string(certRef.Name)
		secret, err := c.secretLister.Secrets(secretNamespace).Get(secretName)
		if err != nil {
			// Per the spec, if the grant was missing, we must not reveal that the secret doesn't exist.
			// The error from the grant check above takes precedence.
			return nil, fmt.Errorf("failed to get secret %s/%s: %w", secretNamespace, secretName, err)
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

func validateSecretCertificate(secret *corev1.Secret) error {
	privateKey, ok := secret.Data[corev1.TLSPrivateKeyKey]
	if !ok {
		return fmt.Errorf("secret %s/%s does not contain key %s", secret.Namespace, secret.Name, corev1.TLSPrivateKeyKey)
	}
	block, _ := pem.Decode(privateKey)
	if block == nil {
		return fmt.Errorf("secret %s/%s key %s does not contain a valid PEM-encoded private key", secret.Namespace, secret.Name, corev1.TLSPrivateKeyKey)
	}

	certChain, ok := secret.Data[corev1.TLSCertKey]
	if !ok {
		return fmt.Errorf("secret %s/%s does not contain key %s", secret.Namespace, secret.Name, corev1.TLSCertKey)
	}
	block, _ = pem.Decode(certChain)
	if block == nil {
		return fmt.Errorf("secret %s/%s key %s does not contain a valid PEM-encoded certificate chain", secret.Namespace, secret.Name, corev1.TLSCertKey)
	}
	return nil
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

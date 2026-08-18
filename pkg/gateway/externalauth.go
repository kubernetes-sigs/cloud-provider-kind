/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"crypto/sha256"
	"fmt"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	mutationrulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extauthzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	upstreamsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	corev1listers "k8s.io/client-go/listers/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewaylistersv1 "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

// GEP-1494 (HTTP Auth in Gateway API) is implemented on top of Envoy's ext_authz
// filter. The authorization server is per HTTPRoute rule, but Envoy only allows
// the server to be configured on the HTTP filter itself, so a distinct (disabled
// by default) ext_authz filter is installed in the connection manager for every
// unique ExternalAuth configuration, and each route enables the one it needs
// through `typed_per_filter_config`.
const (
	// externalAuthFilterPrefix namespaces the generated Envoy HTTP filter names.
	externalAuthFilterPrefix = "envoy.filters.http.ext_authz.gateway-api"
	// externalAuthTimeout bounds how long a request waits for the authorization server.
	externalAuthTimeout = 5 * time.Second
)

// externalAuthResources holds the Envoy resources derived from an ExternalAuth filter.
type externalAuthResources struct {
	// httpFilter is installed disabled on the connection manager and enabled per route.
	httpFilter *hcm.HttpFilter
	// cluster is the upstream for the authorization server.
	cluster *clusterv3.Cluster
}

// enableExtAuthzPerRoute is the typed_per_filter_config value that turns on an
// ext_authz filter that is disabled by default. It carries no configuration, so
// a single shared instance is enough.
var enableExtAuthzPerRoute = mustEnableExtAuthzPerRoute()

func mustEnableExtAuthzPerRoute() *anypb.Any {
	cfg, err := anypb.New(&extauthzv3.ExtAuthzPerRoute{
		Override: &extauthzv3.ExtAuthzPerRoute_CheckSettings{
			CheckSettings: &extauthzv3.CheckSettings{},
		},
	})
	if err != nil {
		panic(fmt.Sprintf("marshalling a constant ExtAuthzPerRoute cannot fail: %v", err))
	}
	return cfg
}

// resolveExternalAuthBackend validates the ExternalAuth backendRef and returns the
// Envoy cluster name to use for the authorization server.
func resolveExternalAuthBackend(
	routeNamespace string,
	extAuth *gatewayv1.HTTPExternalAuthFilter,
	serviceLister corev1listers.ServiceLister,
	referenceGrantLister gatewaylistersv1.ReferenceGrantLister,
) (string, error) {
	backendRef := extAuth.BackendRef

	if backendRef.Group != nil && *backendRef.Group != "" {
		return "", &ControllerError{
			Reason:  string(gatewayv1.RouteReasonInvalidKind),
			Message: fmt.Sprintf("unsupported externalAuth backend group: %s", *backendRef.Group),
		}
	}
	if backendRef.Kind != nil && *backendRef.Kind != "Service" {
		return "", &ControllerError{
			Reason:  string(gatewayv1.RouteReasonInvalidKind),
			Message: fmt.Sprintf("unsupported externalAuth backend kind: %s", *backendRef.Kind),
		}
	}
	if backendRef.Port == nil {
		return "", &ControllerError{
			Reason:  string(gatewayv1.RouteReasonUnsupportedProtocol),
			Message: "externalAuth backend port must be specified",
		}
	}

	ns := routeNamespace
	if backendRef.Namespace != nil {
		ns = string(*backendRef.Namespace)
	}

	if ns != routeNamespace {
		from := gatewayv1.ReferenceGrantFrom{
			Group:     gatewayv1.GroupName,
			Kind:      "HTTPRoute",
			Namespace: gatewayv1.Namespace(routeNamespace),
		}
		to := gatewayv1.ReferenceGrantTo{
			Group: "", // Core group for Service
			Kind:  "Service",
			Name:  &backendRef.Name,
		}
		if !isCrossNamespaceRefAllowed(from, to, ns, referenceGrantLister) {
			return "", &ControllerError{
				Reason:  string(gatewayv1.RouteReasonRefNotPermitted),
				Message: fmt.Sprintf("reference to Service %s/%s not permitted by any ReferenceGrant", ns, backendRef.Name),
			}
		}
	}

	if _, err := serviceLister.Services(ns).Get(string(backendRef.Name)); err != nil {
		return "", &ControllerError{
			Reason:  string(gatewayv1.RouteReasonBackendNotFound),
			Message: fmt.Sprintf("externalAuth backend Service %s/%s not found", ns, backendRef.Name),
		}
	}

	return externalAuthClusterName(ns, string(backendRef.Name), int32(*backendRef.Port)), nil
}

// externalAuthClusterName returns the Envoy cluster name for an authorization server.
// It is kept distinct from regular backend clusters because the authorization
// cluster may need different protocol options (HTTP/2 for the gRPC protocol).
func externalAuthClusterName(namespace, name string, port int32) string {
	return fmt.Sprintf("extauth_%s_%s_core_Service_%d", namespace, name, port)
}

// buildExtAuthzConfig translates an ExternalAuth filter into Envoy's ext_authz config.
func buildExtAuthzConfig(extAuth *gatewayv1.HTTPExternalAuthFilter, clusterName, authority string) (*extauthzv3.ExtAuthz, error) {
	extAuthz := &extauthzv3.ExtAuthz{
		TransportApiVersion: corev3.ApiVersion_V3,
		// A request must never reach the backend when the authorization server
		// is unreachable or returns an error.
		FailureModeAllow: false,
		// Reject responses from the authorization server that try to inject
		// malformed headers into the request.
		ValidateMutations: true,
		// GEP-1494 lets an authorization server copy every response header into
		// the upstream request. The defaults of HeaderMutationRules keep that
		// from rewriting Host, the :-prefixed routing headers and x-envoy-*.
		DecoderHeaderMutationRules: &mutationrulesv3.HeaderMutationRules{},
	}

	switch extAuth.ExternalAuthProtocol {
	case gatewayv1.HTTPRouteExternalAuthGRPCProtocol:
		extAuthz.Services = &extauthzv3.ExtAuthz_GrpcService{
			GrpcService: &corev3.GrpcService{
				TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
					EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: clusterName},
				},
				Timeout: durationpb.New(externalAuthTimeout),
			},
		}
		// When empty the Gateway API defaults apply, which match Envoy's own
		// default set of headers forwarded to a gRPC authorization server.
		if extAuth.GRPCAuthConfig != nil && len(extAuth.GRPCAuthConfig.AllowedRequestHeaders) > 0 {
			extAuthz.AllowedHeaders = exactStringMatchers(extAuth.GRPCAuthConfig.AllowedRequestHeaders)
		}

	case gatewayv1.HTTPRouteExternalAuthHTTPProtocol:
		httpService := &extauthzv3.HttpService{
			ServerUri: &corev3.HttpUri{
				Uri:              fmt.Sprintf("http://%s", authority),
				HttpUpstreamType: &corev3.HttpUri_Cluster{Cluster: clusterName},
				Timeout:          durationpb.New(externalAuthTimeout),
			},
		}
		if extAuth.HTTPAuthConfig != nil {
			httpService.PathPrefix = extAuth.HTTPAuthConfig.Path
			if len(extAuth.HTTPAuthConfig.AllowedRequestHeaders) > 0 {
				httpService.AuthorizationRequest = &extauthzv3.AuthorizationRequest{
					AllowedHeaders: exactStringMatchers(extAuth.HTTPAuthConfig.AllowedRequestHeaders),
				}
			}
			httpService.AuthorizationResponse = &extauthzv3.AuthorizationResponse{
				AllowedUpstreamHeaders: responseHeaderMatchers(extAuth.HTTPAuthConfig.AllowedResponseHeaders),
			}
		} else {
			httpService.AuthorizationResponse = &extauthzv3.AuthorizationResponse{
				AllowedUpstreamHeaders: responseHeaderMatchers(nil),
			}
		}
		extAuthz.Services = &extauthzv3.ExtAuthz_HttpService{HttpService: httpService}

	default:
		return nil, &ControllerError{
			Reason:  string(gatewayv1.RouteReasonUnsupportedValue),
			Message: fmt.Sprintf("unsupported externalAuth protocol: %q", extAuth.ExternalAuthProtocol),
		}
	}

	if extAuth.ForwardBody != nil && extAuth.ForwardBody.MaxSize > 0 {
		extAuthz.WithRequestBody = &extauthzv3.BufferSettings{
			MaxRequestBytes:     uint32(extAuth.ForwardBody.MaxSize),
			AllowPartialMessage: false, // GEP-1494
			PackAsBytes:         extAuth.ExternalAuthProtocol == gatewayv1.HTTPRouteExternalAuthGRPCProtocol,
		}
	}

	return extAuthz, nil
}

// exactStringMatchers builds a case-insensitive exact matcher list for header names.
func exactStringMatchers(names []string) *matcherv3.ListStringMatcher {
	list := &matcherv3.ListStringMatcher{}
	for _, name := range names {
		list.Patterns = append(list.Patterns, &matcherv3.StringMatcher{
			IgnoreCase:   true,
			MatchPattern: &matcherv3.StringMatcher_Exact{Exact: name},
		})
	}
	return list
}

// responseHeaderMatchers builds the matcher for headers copied from the
// authorization response into the request sent upstream. An empty list means
// "copy everything" as required by GEP-1494; the filter's header mutation rules
// keep Host and the other routing headers out of reach.
func responseHeaderMatchers(names []string) *matcherv3.ListStringMatcher {
	if len(names) == 0 {
		return &matcherv3.ListStringMatcher{
			Patterns: []*matcherv3.StringMatcher{{
				MatchPattern: &matcherv3.StringMatcher_SafeRegex{
					SafeRegex: &matcherv3.RegexMatcher{
						EngineType: &matcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &matcherv3.RegexMatcher_GoogleRE2{}},
						Regex:      ".*",
					},
				},
			}},
		}
	}
	return exactStringMatchers(names)
}

// buildExternalAuthFilter renders the Envoy HTTP filter for an ExternalAuth
// configuration. The name is a digest of the rendered configuration so that
// identical configurations share one filter. It is only meaningful within a
// single reconcile and must never be persisted or compared across builds.
func buildExternalAuthFilter(extAuth *gatewayv1.HTTPExternalAuthFilter, clusterName, authority string) (string, *hcm.HttpFilter, error) {
	extAuthz, err := buildExtAuthzConfig(extAuth, clusterName, authority)
	if err != nil {
		return "", nil, err
	}

	serialized, err := proto.MarshalOptions{Deterministic: true}.Marshal(extAuthz)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(serialized)
	name := fmt.Sprintf("%s.%x", externalAuthFilterPrefix, sum[:8])

	typedConfig, err := anypb.New(extAuthz)
	if err != nil {
		return "", nil, err
	}

	return name, &hcm.HttpFilter{
		Name: name,
		// The filter only applies to the routes that opt in through
		// typed_per_filter_config.
		Disabled: true,
		ConfigType: &hcm.HttpFilter_TypedConfig{
			TypedConfig: typedConfig,
		},
	}, nil
}

// resolveRuleExternalAuth builds the Envoy resources for every ExternalAuth
// filter of an HTTPRoute rule, keyed by the connection manager filter name that
// the rule's routes must reference. Resolution is all or nothing: any failure
// leaves the rule with no authorization at all, so the caller has to fail it
// closed rather than forward unauthorized traffic.
func resolveRuleExternalAuth(
	routeNamespace string,
	filters []gatewayv1.HTTPRouteFilter,
	serviceLister corev1listers.ServiceLister,
	referenceGrantLister gatewaylistersv1.ReferenceGrantLister,
) (map[string]*externalAuthResources, error) {
	var resolved map[string]*externalAuthResources

	for _, filter := range filters {
		if filter.Type != gatewayv1.HTTPRouteFilterExternalAuth || filter.ExternalAuth == nil {
			continue
		}
		extAuth := filter.ExternalAuth

		clusterName, err := resolveExternalAuthBackend(routeNamespace, extAuth, serviceLister, referenceGrantLister)
		if err != nil {
			return nil, err
		}
		name, httpFilter, err := buildExternalAuthFilter(extAuth, clusterName, externalAuthAuthority(routeNamespace, extAuth))
		if err != nil {
			return nil, err
		}
		cluster, err := externalAuthCluster(serviceLister, routeNamespace, clusterName, extAuth)
		if err != nil {
			return nil, err
		}

		if resolved == nil {
			resolved = make(map[string]*externalAuthResources)
		}
		resolved[name] = &externalAuthResources{httpFilter: httpFilter, cluster: cluster}
	}

	return resolved, nil
}

// externalAuthAuthority returns the value used as the Host header of the
// requests sent to an HTTP authorization server.
func externalAuthAuthority(routeNamespace string, extAuth *gatewayv1.HTTPExternalAuthFilter) string {
	ns := routeNamespace
	if extAuth.BackendRef.Namespace != nil {
		ns = string(*extAuth.BackendRef.Namespace)
	}
	port := int32(0)
	if extAuth.BackendRef.Port != nil {
		port = int32(*extAuth.BackendRef.Port)
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", extAuth.BackendRef.Name, ns, port)
}

// externalAuthCluster builds the Envoy cluster for an authorization server.
func externalAuthCluster(serviceLister corev1listers.ServiceLister, routeNamespace, clusterName string, extAuth *gatewayv1.HTTPExternalAuthFilter) (*clusterv3.Cluster, error) {
	backendRef := gatewayv1.BackendRef{BackendObjectReference: extAuth.BackendRef}
	cluster, err := translateBackendRefToCluster(serviceLister, routeNamespace, backendRef)
	if err != nil {
		return nil, err
	}
	cluster.Name = clusterName
	if cluster.LoadAssignment != nil {
		cluster.LoadAssignment.ClusterName = clusterName
	}

	// Envoy's ext_authz gRPC client requires an HTTP/2 upstream.
	if extAuth.ExternalAuthProtocol == gatewayv1.HTTPRouteExternalAuthGRPCProtocol {
		protocolOptions, err := anypb.New(&upstreamsv3.HttpProtocolOptions{
			UpstreamProtocolOptions: &upstreamsv3.HttpProtocolOptions_ExplicitHttpConfig_{
				ExplicitHttpConfig: &upstreamsv3.HttpProtocolOptions_ExplicitHttpConfig{
					ProtocolConfig: &upstreamsv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
						Http2ProtocolOptions: &corev3.Http2ProtocolOptions{},
					},
				},
			},
		})
		if err != nil {
			return nil, err
		}
		cluster.TypedExtensionProtocolOptions = map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": protocolOptions,
		}
	}

	return cluster, nil
}

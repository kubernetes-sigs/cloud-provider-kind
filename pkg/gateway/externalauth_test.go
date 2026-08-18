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
	"strings"
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	extauthzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func externalAuthFilter(extAuth *gatewayv1.HTTPExternalAuthFilter) gatewayv1.HTTPRouteFilter {
	return gatewayv1.HTTPRouteFilter{
		Type:         gatewayv1.HTTPRouteFilterExternalAuth,
		ExternalAuth: extAuth,
	}
}

func httpExternalAuth(namespace, name string, port int32) *gatewayv1.HTTPExternalAuthFilter {
	extAuth := &gatewayv1.HTTPExternalAuthFilter{
		ExternalAuthProtocol: gatewayv1.HTTPRouteExternalAuthHTTPProtocol,
		BackendRef: gatewayv1.BackendObjectReference{
			Name: gatewayv1.ObjectName(name),
			Port: ptr.To(gatewayv1.PortNumber(port)),
		},
	}
	if namespace != "" {
		extAuth.BackendRef.Namespace = ptr.To(gatewayv1.Namespace(namespace))
	}
	return extAuth
}

func TestResolveExternalAuthBackend(t *testing.T) {
	svcLister := newMockServiceLister(
		makeService("default", "authz", 9000),
		makeService("infra", "authz", 9000),
	)
	noGrants := newFakeReferenceGrantLister(nil, nil)
	grants := newFakeReferenceGrantLister([]*gatewayv1.ReferenceGrant{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "grant"},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{
				Group:     gatewayv1.GroupName,
				Kind:      "HTTPRoute",
				Namespace: "default",
			}},
			To: []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}}, nil)

	testCases := []struct {
		name        string
		extAuth     *gatewayv1.HTTPExternalAuthFilter
		wantCluster string
		wantReason  string
	}{
		{
			name:        "same namespace service",
			extAuth:     httpExternalAuth("", "authz", 9000),
			wantCluster: "extauth_default_authz_core_Service_9000",
		},
		{
			name:       "missing service",
			extAuth:    httpExternalAuth("", "missing", 9000),
			wantReason: string(gatewayv1.RouteReasonBackendNotFound),
		},
		{
			name: "unsupported kind",
			extAuth: func() *gatewayv1.HTTPExternalAuthFilter {
				e := httpExternalAuth("", "authz", 9000)
				e.BackendRef.Kind = ptr.To(gatewayv1.Kind("Pod"))
				return e
			}(),
			wantReason: string(gatewayv1.RouteReasonInvalidKind),
		},
		{
			name: "missing port",
			extAuth: func() *gatewayv1.HTTPExternalAuthFilter {
				e := httpExternalAuth("", "authz", 9000)
				e.BackendRef.Port = nil
				return e
			}(),
			wantReason: string(gatewayv1.RouteReasonUnsupportedProtocol),
		},
		{
			name:       "cross namespace without grant",
			extAuth:    httpExternalAuth("infra", "authz", 9000),
			wantReason: string(gatewayv1.RouteReasonRefNotPermitted),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveExternalAuthBackend("default", tc.extAuth, svcLister, noGrants)
			if tc.wantReason != "" {
				if err == nil {
					t.Fatalf("expected error with reason %q, got cluster %q", tc.wantReason, got)
				}
				controllerErr, ok := err.(*ControllerError)
				if !ok {
					t.Fatalf("expected *ControllerError, got %T", err)
				}
				if controllerErr.Reason != tc.wantReason {
					t.Errorf("got reason %q, want %q", controllerErr.Reason, tc.wantReason)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantCluster {
				t.Errorf("got cluster %q, want %q", got, tc.wantCluster)
			}
		})
	}

	t.Run("cross namespace with grant", func(t *testing.T) {
		got, err := resolveExternalAuthBackend("default", httpExternalAuth("infra", "authz", 9000), svcLister, grants)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "extauth_infra_authz_core_Service_9000"; got != want {
			t.Errorf("got cluster %q, want %q", got, want)
		}
	})
}

func TestBuildExtAuthzConfigHTTP(t *testing.T) {
	extAuth := httpExternalAuth("", "authz", 9000)
	extAuth.HTTPAuthConfig = &gatewayv1.HTTPAuthConfig{
		Path:                   "/auth",
		AllowedRequestHeaders:  []string{"X-Request-Id"},
		AllowedResponseHeaders: []string{"X-User"},
	}
	extAuth.ForwardBody = &gatewayv1.ForwardBodyConfig{MaxSize: 1024}

	got, err := buildExtAuthzConfig(extAuth, "cluster", "authz.default.svc.cluster.local:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("generated ext_authz config is invalid: %v", err)
	}

	httpService := got.GetHttpService()
	if httpService == nil {
		t.Fatal("expected an http_service to be configured")
	}
	if httpService.GetServerUri().GetCluster() != "cluster" {
		t.Errorf("got cluster %q, want %q", httpService.GetServerUri().GetCluster(), "cluster")
	}
	if httpService.GetPathPrefix() != "/auth" {
		t.Errorf("got path prefix %q, want %q", httpService.GetPathPrefix(), "/auth")
	}
	reqHeaders := httpService.GetAuthorizationRequest().GetAllowedHeaders().GetPatterns()
	if len(reqHeaders) != 1 || reqHeaders[0].GetExact() != "X-Request-Id" {
		t.Errorf("unexpected allowed request headers: %v", reqHeaders)
	}
	respHeaders := httpService.GetAuthorizationResponse().GetAllowedUpstreamHeaders().GetPatterns()
	if len(respHeaders) != 1 || respHeaders[0].GetExact() != "X-User" {
		t.Errorf("unexpected allowed response headers: %v", respHeaders)
	}
	if got.GetWithRequestBody().GetMaxRequestBytes() != 1024 {
		t.Errorf("got max request bytes %d, want 1024", got.GetWithRequestBody().GetMaxRequestBytes())
	}
	if got.GetWithRequestBody().GetAllowPartialMessage() {
		t.Error("expected allow_partial_message to be unset so oversized bodies are rejected with 413")
	}
	if got.GetWithRequestBody().GetPackAsBytes() {
		t.Error("pack_as_bytes only applies to the gRPC protocol")
	}
	if got.GetFailureModeAllow() {
		t.Error("requests must not be forwarded when authorization fails")
	}
}

func TestBuildExtAuthzConfigGRPC(t *testing.T) {
	extAuth := httpExternalAuth("", "authz", 9000)
	extAuth.ExternalAuthProtocol = gatewayv1.HTTPRouteExternalAuthGRPCProtocol
	extAuth.GRPCAuthConfig = &gatewayv1.GRPCAuthConfig{AllowedRequestHeaders: []string{"Authorization"}}
	extAuth.ForwardBody = &gatewayv1.ForwardBodyConfig{MaxSize: 64}

	got, err := buildExtAuthzConfig(extAuth, "cluster", "authz.default.svc.cluster.local:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("generated ext_authz config is invalid: %v", err)
	}
	if got.GetGrpcService().GetEnvoyGrpc().GetClusterName() != "cluster" {
		t.Errorf("got cluster %q, want %q", got.GetGrpcService().GetEnvoyGrpc().GetClusterName(), "cluster")
	}
	patterns := got.GetAllowedHeaders().GetPatterns()
	if len(patterns) != 1 || patterns[0].GetExact() != "Authorization" {
		t.Errorf("unexpected allowed headers: %v", patterns)
	}
	if !got.GetWithRequestBody().GetPackAsBytes() {
		t.Error("expected pack_as_bytes for the gRPC protocol")
	}
}

func TestBuildExtAuthzConfigDefaultResponseHeaders(t *testing.T) {
	got, err := buildExtAuthzConfig(httpExternalAuth("", "authz", 9000), "cluster", "authz:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	patterns := got.GetHttpService().GetAuthorizationResponse().GetAllowedUpstreamHeaders().GetPatterns()
	if len(patterns) != 1 || patterns[0].GetSafeRegex().GetRegex() != ".*" {
		t.Errorf("an empty allowedResponseHeaders must copy every header, got %v", patterns)
	}
}

func TestBuildExtAuthzConfigUnsupportedProtocol(t *testing.T) {
	extAuth := httpExternalAuth("", "authz", 9000)
	extAuth.ExternalAuthProtocol = "TCP"
	if _, err := buildExtAuthzConfig(extAuth, "cluster", "authz:9000"); err == nil {
		t.Fatal("expected an error for an unsupported protocol")
	}
}

func TestBuildExternalAuthFilterNameIsDeterministic(t *testing.T) {
	extAuth := httpExternalAuth("", "authz", 9000)

	first, filter, err := buildExternalAuthFilter(extAuth, "cluster", "authz:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(first, externalAuthFilterPrefix) {
		t.Errorf("filter name %q does not use the expected prefix", first)
	}
	if filter.Name != first {
		t.Errorf("filter name %q does not match returned name %q", filter.Name, first)
	}
	if !filter.GetDisabled() {
		t.Error("the ext_authz filter must be disabled so only opted-in routes use it")
	}

	second, _, err := buildExternalAuthFilter(extAuth, "cluster", "authz:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first != second {
		t.Errorf("identical configurations produced different names: %q vs %q", first, second)
	}

	changed := httpExternalAuth("", "authz", 9000)
	changed.HTTPAuthConfig = &gatewayv1.HTTPAuthConfig{Path: "/auth"}
	third, _, err := buildExternalAuthFilter(changed, "cluster", "authz:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first == third {
		t.Error("different configurations produced the same filter name")
	}
}

func TestTranslateHTTPRouteWithExternalAuth(t *testing.T) {
	svcLister := newMockServiceLister(makeService("default", "svc", 80), makeService("default", "authz", 9000))
	noGrants := newFakeReferenceGrantLister(nil, nil)

	extAuth := httpExternalAuth("", "authz", 9000)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "route"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{makeRuleWithFilters(externalAuthFilter(extAuth))},
		},
	}

	got := translateHTTPRouteToEnvoyRoutes(route, svcLister, noGrants)
	if got.notAccepted != nil {
		t.Fatalf("route was rejected: %v", got.notAccepted.Message)
	}
	if got.resolvedRefsFailure != nil {
		t.Fatalf("unexpected ResolvedRefs failure: %v", got.resolvedRefsFailure.Message)
	}
	if len(got.routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(got.routes))
	}

	wantName, _, err := buildExternalAuthFilter(extAuth, "extauth_default_authz_core_Service_9000", externalAuthAuthority("default", extAuth))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got.externalAuth[wantName]; !ok {
		t.Fatalf("translation did not return the resources for filter %q, got %v", wantName, got.externalAuth)
	}
	perFilter, ok := got.routes[0].TypedPerFilterConfig[wantName]
	if !ok {
		t.Fatalf("route is missing the per-filter config for %q, got %v", wantName, got.routes[0].TypedPerFilterConfig)
	}
	perRoute := &extauthzv3.ExtAuthzPerRoute{}
	if err := perFilter.UnmarshalTo(perRoute); err != nil {
		t.Fatalf("failed to unmarshal per-route config: %v", err)
	}
	if err := perRoute.Validate(); err != nil {
		t.Fatalf("generated per-route config is invalid: %v", err)
	}
	if perRoute.GetCheckSettings() == nil {
		t.Error("per-route config must enable the filter through check_settings")
	}
	if _, ok := got.routes[0].Action.(*routev3.Route_Route); !ok {
		t.Errorf("expected a forwarding action, got %T", got.routes[0].Action)
	}
}

func TestTranslateHTTPRouteWithUnresolvableExternalAuth(t *testing.T) {
	svcLister := newMockServiceLister(makeService("default", "svc", 80))
	noGrants := newFakeReferenceGrantLister(nil, nil)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "route"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{makeRuleWithFilters(externalAuthFilter(httpExternalAuth("", "missing", 9000)))},
		},
	}

	got := translateHTTPRouteToEnvoyRoutes(route, svcLister, noGrants)
	if got.resolvedRefsFailure == nil {
		t.Fatal("expected a ResolvedRefs failure when the authorization server cannot be resolved")
	}
	if got.resolvedRefsFailure.Reason != string(gatewayv1.RouteReasonBackendNotFound) {
		t.Errorf("got reason %q, want %q", got.resolvedRefsFailure.Reason, gatewayv1.RouteReasonBackendNotFound)
	}
	if len(got.routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(got.routes))
	}
	direct, ok := got.routes[0].Action.(*routev3.Route_DirectResponse)
	if !ok {
		t.Fatalf("expected the request to fail closed, got %T", got.routes[0].Action)
	}
	if direct.DirectResponse.Status != 500 {
		t.Errorf("got status %d, want 500", direct.DirectResponse.Status)
	}
	if len(got.routes[0].TypedPerFilterConfig) != 0 {
		t.Error("no per-filter config may reference a filter that was not installed")
	}
	if len(got.externalAuth) != 0 {
		t.Error("no Envoy resources may be produced for an unresolvable authorization server")
	}
}

func TestExternalAuthFilterIsSupported(t *testing.T) {
	filters := []gatewayv1.HTTPRouteFilter{externalAuthFilter(httpExternalAuth("", "authz", 9000))}
	if unsupported, found := findUnsupportedFilter(filters); found {
		t.Errorf("ExternalAuth must be a supported filter, got %q reported as unsupported", unsupported)
	}
}

func TestListenerFilterChainIncludesExternalAuth(t *testing.T) {
	_, extAuthFilter, err := buildExternalAuthFilter(httpExternalAuth("", "authz", 9000), "cluster", "authz:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := &Controller{}
	lis := gatewayv1.Listener{Name: "http", Protocol: gatewayv1.HTTPProtocolType, Port: 80}
	filterChain, err := c.translateListenerToFilterChain(&gatewayv1.Gateway{}, lis, nil, "route-80", []*hcm.HttpFilter{extAuthFilter})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	manager := &hcm.HttpConnectionManager{}
	if err := filterChain.Filters[0].GetTypedConfig().UnmarshalTo(manager); err != nil {
		t.Fatalf("failed to unmarshal the connection manager: %v", err)
	}
	if err := manager.Validate(); err != nil {
		t.Fatalf("generated connection manager is invalid: %v", err)
	}
	if len(manager.HttpFilters) != 2 {
		t.Fatalf("got %d http filters, want 2", len(manager.HttpFilters))
	}
	if manager.HttpFilters[0].Name != extAuthFilter.Name {
		t.Errorf("the authorization filter must run before the router, got %q first", manager.HttpFilters[0].Name)
	}
	if manager.HttpFilters[1].Name != wellknown.Router {
		t.Errorf("the router must be the last filter, got %q", manager.HttpFilters[1].Name)
	}
}

func routeWithExternalAuth(namespace, name string, extAuths ...*gatewayv1.HTTPExternalAuthFilter) *gatewayv1.HTTPRoute {
	filters := make([]gatewayv1.HTTPRouteFilter, 0, len(extAuths))
	for _, extAuth := range extAuths {
		filters = append(filters, externalAuthFilter(extAuth))
	}
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{makeRuleWithFilters(filters...)},
		},
	}
}

// makeAuthService builds an authorization Service with a ClusterIP, which the
// Envoy cluster needs to resolve to a valid endpoint address.
func makeAuthService(namespace, name string, port int32, clusterIP string) *corev1.Service {
	svc := makeService(namespace, name, port)
	svc.Spec.ClusterIP = clusterIP
	return svc
}

func TestResolveRuleExternalAuth(t *testing.T) {
	svcLister := newMockServiceLister(
		makeService("default", "svc", 80),
		makeAuthService("default", "authz", 9000, "10.0.0.10"),
		makeAuthService("default", "authz-grpc", 9001, "10.0.0.11"),
	)
	noGrants := newFakeReferenceGrantLister(nil, nil)

	grpcAuth := httpExternalAuth("", "authz-grpc", 9001)
	grpcAuth.ExternalAuthProtocol = gatewayv1.HTTPRouteExternalAuthGRPCProtocol
	pathAuth := httpExternalAuth("", "authz", 9000)
	pathAuth.HTTPAuthConfig = &gatewayv1.HTTPAuthConfig{Path: "/auth"}

	// The same configuration listed twice must collapse into one filter.
	filters := []gatewayv1.HTTPRouteFilter{
		externalAuthFilter(httpExternalAuth("", "authz", 9000)),
		externalAuthFilter(httpExternalAuth("", "authz", 9000)),
		externalAuthFilter(pathAuth),
		externalAuthFilter(grpcAuth),
	}

	got, err := resolveRuleExternalAuth("default", filters, svcLister, noGrants)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d ext_authz filters, want 3", len(got))
	}

	for name, resources := range got {
		if resources.httpFilter.Name != name {
			t.Errorf("filter %q is keyed under %q", resources.httpFilter.Name, name)
		}
		if err := resources.cluster.Validate(); err != nil {
			t.Errorf("cluster %q is invalid: %v", resources.cluster.Name, err)
		}
	}

	grpcName, _, err := buildExternalAuthFilter(grpcAuth, "extauth_default_authz-grpc_core_Service_9001", externalAuthAuthority("default", grpcAuth))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	grpcCluster := got[grpcName].cluster
	if grpcCluster.Name != "extauth_default_authz-grpc_core_Service_9001" {
		t.Errorf("got cluster name %q", grpcCluster.Name)
	}
	if _, ok := grpcCluster.TypedExtensionProtocolOptions["envoy.extensions.upstreams.http.v3.HttpProtocolOptions"]; !ok {
		t.Error("a gRPC authorization server requires an HTTP/2 upstream")
	}

	httpName, _, err := buildExternalAuthFilter(httpExternalAuth("", "authz", 9000), "extauth_default_authz_core_Service_9000", "authz.default.svc.cluster.local:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got[httpName].cluster.TypedExtensionProtocolOptions) != 0 {
		t.Error("an HTTP authorization server must not be forced to HTTP/2")
	}
}

// Resolution is all or nothing: one bad reference must not leave the rule with a
// partially applied authorization chain.
func TestResolveRuleExternalAuthIsAllOrNothing(t *testing.T) {
	svcLister := newMockServiceLister(makeAuthService("default", "authz", 9000, "10.0.0.10"))
	noGrants := newFakeReferenceGrantLister(nil, nil)

	got, err := resolveRuleExternalAuth("default", []gatewayv1.HTTPRouteFilter{
		externalAuthFilter(httpExternalAuth("", "authz", 9000)),
		externalAuthFilter(httpExternalAuth("", "missing", 9000)),
	}, svcLister, noGrants)
	if err == nil {
		t.Fatal("expected an error when one authorization server cannot be resolved")
	}
	if got != nil {
		t.Errorf("no resources may be returned on failure, got %v", got)
	}
}

// Envoy rejects a route configuration that references an HTTP filter which is
// not installed on the connection manager, so every filter a route enables must
// come back in the same translation result.
func TestExternalAuthRoutesOnlyReferenceReturnedFilters(t *testing.T) {
	svcLister := newMockServiceLister(
		makeService("default", "svc", 80),
		makeAuthService("default", "authz", 9000, "10.0.0.10"),
		makeAuthService("infra", "authz", 9000, "10.0.0.12"),
	)
	grants := newFakeReferenceGrantLister([]*gatewayv1.ReferenceGrant{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "grant"},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{
				Group:     gatewayv1.GroupName,
				Kind:      "HTTPRoute",
				Namespace: "default",
			}},
			To: []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}}, nil)

	grpcAuth := httpExternalAuth("", "authz", 9000)
	grpcAuth.ExternalAuthProtocol = gatewayv1.HTTPRouteExternalAuthGRPCProtocol
	bodyAuth := httpExternalAuth("", "authz", 9000)
	bodyAuth.ForwardBody = &gatewayv1.ForwardBodyConfig{MaxSize: 512}

	routes := []*gatewayv1.HTTPRoute{
		routeWithExternalAuth("default", "plain", httpExternalAuth("", "authz", 9000)),
		routeWithExternalAuth("default", "grpc", grpcAuth),
		routeWithExternalAuth("default", "body", bodyAuth),
		routeWithExternalAuth("default", "cross-ns", httpExternalAuth("infra", "authz", 9000)),
		routeWithExternalAuth("default", "unresolvable", httpExternalAuth("", "missing", 9000)),
		// A rule may chain more than one authorization server.
		routeWithExternalAuth("default", "chained", httpExternalAuth("", "authz", 9000), grpcAuth),
	}

	for _, route := range routes {
		got := translateHTTPRouteToEnvoyRoutes(route, svcLister, grants)
		for _, envoyRoute := range got.routes {
			for name := range envoyRoute.TypedPerFilterConfig {
				if _, ok := got.externalAuth[name]; !ok {
					t.Errorf("route %s/%s references filter %q which was not returned", route.Namespace, route.Name, name)
				}
			}
		}
	}

	chained := translateHTTPRouteToEnvoyRoutes(routes[5], svcLister, grants)
	if len(chained.routes[0].TypedPerFilterConfig) != 2 {
		t.Errorf("got %d enabled filters for the chained rule, want 2", len(chained.routes[0].TypedPerFilterConfig))
	}
	if len(chained.externalAuth) != 2 {
		t.Errorf("got %d returned filters for the chained rule, want 2", len(chained.externalAuth))
	}
}

// A rule whose authorization server is unresolvable must keep matching its own
// traffic, otherwise requests fall through to a lower precedence route that has
// no authorization at all.
func TestUnresolvableExternalAuthShadowsLowerPrecedenceRules(t *testing.T) {
	svcLister := newMockServiceLister(makeService("default", "svc", 80))
	noGrants := newFakeReferenceGrantLister(nil, nil)

	secure := makeRuleWithFilters(externalAuthFilter(httpExternalAuth("", "missing", 9000)))
	secure.Matches = []gatewayv1.HTTPRouteMatch{{
		Path: &gatewayv1.HTTPPathMatch{
			Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
			Value: ptr.To("/secure"),
		},
	}}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "route"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{secure, makeRuleWithBackend("svc")},
		},
	}

	got := translateHTTPRouteToEnvoyRoutes(route, svcLister, noGrants)
	if len(got.routes) != 2 {
		t.Fatalf("got %d routes, want 2: the broken rule must still claim its matches", len(got.routes))
	}
	if got.routes[0].Match.GetPathSeparatedPrefix() != "/secure" {
		t.Errorf("first route does not match /secure: %v", got.routes[0].Match)
	}
	if _, ok := got.routes[0].Action.(*routev3.Route_DirectResponse); !ok {
		t.Errorf("the unauthorized rule must answer directly, got %T", got.routes[0].Action)
	}
	// The healthy rule is untouched.
	if _, ok := got.routes[1].Action.(*routev3.Route_Route); !ok {
		t.Errorf("the healthy rule must still forward, got %T", got.routes[1].Action)
	}
}

func TestExternalAuthAcrossMultipleRules(t *testing.T) {
	svcLister := newMockServiceLister(
		makeService("default", "svc", 80),
		makeAuthService("default", "authz", 9000, "10.0.0.10"),
		makeAuthService("default", "authz-b", 9001, "10.0.0.11"),
	)
	noGrants := newFakeReferenceGrantLister(nil, nil)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "route"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithFilters(externalAuthFilter(httpExternalAuth("", "authz", 9000))),
				makeRuleWithFilters(externalAuthFilter(httpExternalAuth("", "authz-b", 9001))),
				makeRuleWithFilters(),
			},
		},
	}

	got := translateHTTPRouteToEnvoyRoutes(route, svcLister, noGrants)
	if got.resolvedRefsFailure != nil {
		t.Fatalf("unexpected ResolvedRefs failure: %v", got.resolvedRefsFailure.Message)
	}
	if len(got.externalAuth) != 2 {
		t.Fatalf("got %d filters for the route, want 2", len(got.externalAuth))
	}
	if len(got.routes) != 3 {
		t.Fatalf("got %d routes, want 3", len(got.routes))
	}
	for i, want := range []int{1, 1, 0} {
		if len(got.routes[i].TypedPerFilterConfig) != want {
			t.Errorf("route %d enables %d filters, want %d", i, len(got.routes[i].TypedPerFilterConfig), want)
		}
	}
	// Each rule must enable only its own authorization server.
	for name := range got.routes[0].TypedPerFilterConfig {
		if _, ok := got.routes[1].TypedPerFilterConfig[name]; ok {
			t.Errorf("filter %q leaked from rule 0 into rule 1", name)
		}
	}
}

func TestResolveRuleExternalAuthCrossNamespaceRequiresGrant(t *testing.T) {
	svcLister := newMockServiceLister(makeAuthService("infra", "authz", 9000, "10.0.0.12"))

	got, err := resolveRuleExternalAuth("default", []gatewayv1.HTTPRouteFilter{
		externalAuthFilter(httpExternalAuth("infra", "authz", 9000)),
	}, svcLister, newFakeReferenceGrantLister(nil, nil))
	if err == nil {
		t.Fatal("a cross-namespace reference without a ReferenceGrant must be rejected")
	}
	if got != nil {
		t.Errorf("no resources may be programmed, got %v", got)
	}
}

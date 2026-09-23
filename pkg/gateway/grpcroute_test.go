package gateway

import (
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func makeGRPCRuleWithFilters(filters ...gatewayv1.GRPCRouteFilter) gatewayv1.GRPCRouteRule {
	return gatewayv1.GRPCRouteRule{
		Filters: filters,
		BackendRefs: []gatewayv1.GRPCBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: "svc",
						Port: ptr.To(gatewayv1.PortNumber(80)),
					},
				},
			},
		},
	}
}

func makeGRPCRuleWithBackend(svcName string) gatewayv1.GRPCRouteRule {
	return gatewayv1.GRPCRouteRule{
		BackendRefs: []gatewayv1.GRPCBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(svcName),
						Port: ptr.To(gatewayv1.PortNumber(80)),
					},
				},
			},
		},
	}
}

func makeGRPCRuleWithNamespacedBackend(name, namespace string) gatewayv1.GRPCRouteRule {
	return gatewayv1.GRPCRouteRule{
		BackendRefs: []gatewayv1.GRPCBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name:      gatewayv1.ObjectName(name),
						Namespace: ptr.To(gatewayv1.Namespace(namespace)),
						Port:      ptr.To(gatewayv1.PortNumber(80)),
					},
				},
			},
		},
	}
}

func makeGRPCRuleWithMatch(match gatewayv1.GRPCRouteMatch) gatewayv1.GRPCRouteRule {
	return gatewayv1.GRPCRouteRule{
		Matches: []gatewayv1.GRPCRouteMatch{match},
		BackendRefs: []gatewayv1.GRPCBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: "svc",
						Port: ptr.To(gatewayv1.PortNumber(80)),
					},
				},
			},
		},
	}
}

func grpcHeaderModifierFilter() gatewayv1.GRPCRouteFilter {
	return gatewayv1.GRPCRouteFilter{
		Type: gatewayv1.GRPCRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set:    []gatewayv1.HTTPHeader{{Name: "X-Header-Set", Value: "set-overwritten-value"}},
			Add:    []gatewayv1.HTTPHeader{{Name: "X-Header-Add", Value: "add-value"}},
			Remove: []string{"X-Header-Remove"},
		},
	}
}

func grpcRequestMirrorFilter() gatewayv1.GRPCRouteFilter {
	return grpcRequestMirrorFilterTo("svc")
}

func grpcRequestMirrorFilterTo(name string) gatewayv1.GRPCRouteFilter {
	return gatewayv1.GRPCRouteFilter{
		Type: gatewayv1.GRPCRouteFilterRequestMirror,
		RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
			BackendRef: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name),
				Port: ptr.To(gatewayv1.PortNumber(80)),
			},
		},
	}
}

func grpcResponseHeaderFilter() gatewayv1.GRPCRouteFilter {
	return gatewayv1.GRPCRouteFilter{
		Type: gatewayv1.GRPCRouteFilterResponseHeaderModifier,
		ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set: []gatewayv1.HTTPHeader{{Name: "X-Response-Set", Value: "resp"}},
		},
	}
}

func grpcExtensionRefFilter() gatewayv1.GRPCRouteFilter {
	return gatewayv1.GRPCRouteFilter{
		Type: gatewayv1.GRPCRouteFilterExtensionRef,
		ExtensionRef: &gatewayv1.LocalObjectReference{
			Group: "example.com",
			Kind:  "MyFilter",
			Name:  "foo",
		},
	}
}

func TestTranslateGRPCRouteToEnvoyRoutes(t *testing.T) {
	svc := makeService("default", "svc", 80)
	crossSvc := makeService("other-ns", "cross-svc", 80)
	svcLister := newMockServiceLister(svc, crossSvc)
	noGrants := newFakeReferenceGrantLister(nil, nil)

	baseRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-route",
			Namespace:  "default",
			Generation: 1,
		},
	}

	tests := []struct {
		name                    string
		rules                   []gatewayv1.GRPCRouteRule
		wantRoutes              int
		wantAcceptedFalse       bool
		wantResolvedRefsFalse   bool
		wantResolvedRefsMessage string
		wantPartiallyInvalid    bool
	}{
		{
			name: "exact method match with service and method",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithMatch(gatewayv1.GRPCRouteMatch{
					Method: &gatewayv1.GRPCMethodMatch{
						Type:    ptr.To(gatewayv1.GRPCMethodMatchExact),
						Service: ptr.To("example.Echo"),
						Method:  ptr.To("Ping"),
					},
				}),
			},
			wantRoutes: 1,
		},
		{
			name: "request header modifier is a supported core filter",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcHeaderModifierFilter()),
			},
			wantRoutes: 1,
		},
		{
			name: "request mirror is a supported extended filter",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcRequestMirrorFilter()),
			},
			wantRoutes: 1,
		},
		{
			name: "response header modifier is a supported extended filter",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcResponseHeaderFilter()),
			},
			wantRoutes: 1,
		},
		{
			name: "single rule with unsupported filter - fully invalid",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcExtensionRefFilter()),
			},
			wantAcceptedFalse: true,
		},
		{
			name: "first rule unsupported filter, second rule supported - partially invalid",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcExtensionRefFilter()),
				makeGRPCRuleWithFilters(grpcHeaderModifierFilter()),
			},
			wantRoutes:           1,
			wantPartiallyInvalid: true,
		},
		{
			name: "backend not found - ResolvedRefs=False",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithBackend("missing-svc"),
			},
			wantRoutes:              1,
			wantResolvedRefsFalse:   true,
			wantResolvedRefsMessage: "reference to Service default/missing-svc not found",
		},
		{
			name: "missing request mirror backend - ResolvedRefs=False",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcRequestMirrorFilterTo("missing-mirror")),
			},
			wantRoutes:              1,
			wantResolvedRefsFalse:   true,
			wantResolvedRefsMessage: "reference to Service default/missing-mirror not found",
		},
		{
			name: "cross-namespace backend without ReferenceGrant - ResolvedRefs=False",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithNamespacedBackend("cross-svc", "other-ns"),
			},
			wantRoutes:              1,
			wantResolvedRefsFalse:   true,
			wantResolvedRefsMessage: "reference to Service other-ns/cross-svc not permitted by any ReferenceGrant",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := baseRoute.DeepCopy()
			route.Spec.Rules = tt.rules

			routes, _, notAccepted, resolvedRefsFailure, partiallyInvalid := translateGRPCRouteToEnvoyRoutes(route, svcLister, noGrants)

			if len(routes) != tt.wantRoutes {
				t.Errorf("got %d routes, want %d", len(routes), tt.wantRoutes)
			}

			if tt.wantAcceptedFalse {
				if notAccepted == nil {
					t.Fatalf("Accepted condition missing, want Accepted=False")
				}
				if notAccepted.Status != metav1.ConditionFalse {
					t.Errorf("Accepted.Status = %q, want False", notAccepted.Status)
				}
				if resolvedRefsFailure != nil {
					t.Errorf("unexpected ResolvedRefs condition when route is fully rejected")
				}
				if partiallyInvalid != nil {
					t.Errorf("unexpected PartiallyInvalid condition when route is fully rejected")
				}
			} else if notAccepted != nil {
				t.Errorf("unexpected Accepted=False condition")
			}

			if tt.wantResolvedRefsFalse {
				if resolvedRefsFailure == nil {
					t.Fatalf("ResolvedRefs condition is nil, want ResolvedRefs=False")
				}
				if tt.wantResolvedRefsMessage != "" && resolvedRefsFailure.Message != tt.wantResolvedRefsMessage {
					t.Errorf("ResolvedRefs.Message = %q, want %q", resolvedRefsFailure.Message, tt.wantResolvedRefsMessage)
				}
			} else if resolvedRefsFailure != nil {
				t.Errorf("unexpected ResolvedRefs condition")
			}

			if tt.wantPartiallyInvalid {
				if partiallyInvalid == nil {
					t.Fatalf("PartiallyInvalid condition missing, want PartiallyInvalid=True")
				}
				if !strings.HasPrefix(partiallyInvalid.Message, "Dropped Rule") {
					t.Errorf("PartiallyInvalid.Message = %q, want prefix \"Dropped Rule\"", partiallyInvalid.Message)
				}
			} else if partiallyInvalid != nil {
				t.Errorf("unexpected PartiallyInvalid condition")
			}
		})
	}
}

func TestGRPCRouteRequestHeaderModifierApplied(t *testing.T) {
	svc := makeService("default", "svc", 80)
	svcLister := newMockServiceLister(svc)
	noGrants := newFakeReferenceGrantLister(nil, nil)

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "hdr", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcHeaderModifierFilter()),
			},
		},
	}

	routes, _, notAccepted, _, _ := translateGRPCRouteToEnvoyRoutes(route, svcLister, noGrants)
	if notAccepted != nil {
		t.Fatalf("unexpected Accepted=False: %v", notAccepted)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}

	got := routes[0]
	if len(got.RequestHeadersToAdd) != 2 {
		t.Fatalf("got %d headers to add, want 2", len(got.RequestHeadersToAdd))
	}

	setHeader := got.RequestHeadersToAdd[0]
	if setHeader.Header.GetKey() != "X-Header-Set" || setHeader.Header.GetValue() != "set-overwritten-value" {
		t.Errorf("set header = %s=%s, want X-Header-Set=set-overwritten-value", setHeader.Header.GetKey(), setHeader.Header.GetValue())
	}
	if setHeader.AppendAction != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
		t.Errorf("set AppendAction = %v, want OVERWRITE_IF_EXISTS_OR_ADD", setHeader.AppendAction)
	}

	addHeader := got.RequestHeadersToAdd[1]
	if addHeader.Header.GetKey() != "X-Header-Add" || addHeader.Header.GetValue() != "add-value" {
		t.Errorf("add header = %s=%s, want X-Header-Add=add-value", addHeader.Header.GetKey(), addHeader.Header.GetValue())
	}
	if addHeader.AppendAction != corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD {
		t.Errorf("add AppendAction = %v, want APPEND_IF_EXISTS_OR_ADD", addHeader.AppendAction)
	}

	if len(got.RequestHeadersToRemove) != 1 || got.RequestHeadersToRemove[0] != "X-Header-Remove" {
		t.Errorf("RequestHeadersToRemove = %v, want [X-Header-Remove]", got.RequestHeadersToRemove)
	}
}

func TestTranslateGRPCRouteMatchExactServiceAndMethod(t *testing.T) {
	match := gatewayv1.GRPCRouteMatch{
		Method: &gatewayv1.GRPCMethodMatch{
			Type:    ptr.To(gatewayv1.GRPCMethodMatchExact),
			Service: ptr.To("example.Echo"),
			Method:  ptr.To("Ping"),
		},
	}
	routeMatch, dropReason := translateGRPCRouteMatch(match)
	if dropReason != nil {
		t.Fatalf("unexpected drop reason: %s", *dropReason)
	}
	if routeMatch.GetPath() != "/example.Echo/Ping" {
		t.Errorf("path = %q, want %q", routeMatch.GetPath(), "/example.Echo/Ping")
	}
}

func TestGRPCRouteUnavailableOnMissingBackend(t *testing.T) {
	svcLister := newMockServiceLister()
	noGrants := newFakeReferenceGrantLister(nil, nil)
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: "default", Generation: 1},
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{makeGRPCRuleWithBackend("missing-svc")},
		},
	}
	routes, _, _, resolvedRefsFailure, _ := translateGRPCRouteToEnvoyRoutes(route, svcLister, noGrants)
	if resolvedRefsFailure == nil {
		t.Fatal("ResolvedRefs condition is nil, want ResolvedRefs=False")
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	dr := routes[0].GetDirectResponse()
	if dr == nil {
		t.Fatal("want DirectResponse for invalid backend")
	}
	if dr.GetStatus() != 200 {
		t.Errorf("DirectResponse status = %d, want 200", dr.GetStatus())
	}
	foundStatus := false
	for _, h := range routes[0].ResponseHeadersToAdd {
		if h.GetHeader().GetKey() == "grpc-status" && h.GetHeader().GetValue() == "14" {
			foundStatus = true
		}
	}
	if !foundStatus {
		t.Error("missing grpc-status: 14 response header")
	}
}

func TestGRPCRouteWeightedInvalidBackend(t *testing.T) {
	svc := makeService("default", "svc", 80)
	svcLister := newMockServiceLister(svc)
	noGrants := newFakeReferenceGrantLister(nil, nil)
	w1 := int32(1)
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "mix", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{{
				BackendRefs: []gatewayv1.GRPCBackendRef{
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: ptr.To(gatewayv1.PortNumber(80))}, Weight: &w1}},
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "missing", Port: ptr.To(gatewayv1.PortNumber(80))}, Weight: &w1}},
				},
			}},
		},
	}
	routes, valid, _, resolvedRefsFailure, _ := translateGRPCRouteToEnvoyRoutes(route, svcLister, noGrants)
	if resolvedRefsFailure == nil {
		t.Fatal("want ResolvedRefs=False when one backend is missing")
	}
	if len(valid) != 1 {
		t.Fatalf("got %d valid backends, want 1", len(valid))
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	weighted := routes[0].GetRoute().GetWeightedClusters()
	if weighted == nil {
		t.Fatal("want WeightedClusters for mixed valid/invalid backends")
	}
	if len(weighted.Clusters) != 2 {
		t.Fatalf("got %d weighted clusters, want 2", len(weighted.Clusters))
	}
	foundUnavailable := false
	for _, c := range weighted.Clusters {
		if c.GetName() == grpcUnavailableClusterName {
			foundUnavailable = true
		}
	}
	if !foundUnavailable {
		t.Error("weighted clusters missing the gRPC unavailable cluster")
	}
}

func TestGRPCRouteDuplicateHeaderMatches(t *testing.T) {
	match := gatewayv1.GRPCRouteMatch{
		Headers: []gatewayv1.GRPCHeaderMatch{
			{Name: "Version", Value: "v1"},
			{Name: "version", Value: "v2"},
			{Name: "Other", Value: "x"},
		},
	}
	routeMatch, dropReason := translateGRPCRouteMatch(match)
	if dropReason != nil {
		t.Fatalf("unexpected drop reason: %s", *dropReason)
	}
	if len(routeMatch.Headers) != 2 {
		t.Fatalf("got %d header matches, want 2 (duplicate name ignored)", len(routeMatch.Headers))
	}
	if routeMatch.Headers[0].GetName() != "Version" {
		t.Errorf("first header = %q, want Version", routeMatch.Headers[0].GetName())
	}
}

func TestGRPCRouteRequestMirrorApplied(t *testing.T) {
	svc := makeService("default", "svc", 80)
	svcLister := newMockServiceLister(svc)
	noGrants := newFakeReferenceGrantLister(nil, nil)
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "mirror", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{makeGRPCRuleWithFilters(grpcRequestMirrorFilter())},
		},
	}
	routes, _, notAccepted, _, _ := translateGRPCRouteToEnvoyRoutes(route, svcLister, noGrants)
	if notAccepted != nil {
		t.Fatalf("unexpected Accepted=False: %v", notAccepted)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	mirrors := routes[0].GetRoute().GetRequestMirrorPolicies()
	if len(mirrors) != 1 {
		t.Fatalf("got %d request mirror policies, want 1", len(mirrors))
	}
}

func TestGRPCRouteResponseHeaderModifierApplied(t *testing.T) {
	svc := makeService("default", "svc", 80)
	svcLister := newMockServiceLister(svc)
	noGrants := newFakeReferenceGrantLister(nil, nil)
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "resp", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{makeGRPCRuleWithFilters(grpcResponseHeaderFilter())},
		},
	}
	routes, _, _, _, _ := translateGRPCRouteToEnvoyRoutes(route, svcLister, noGrants)
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	if len(routes[0].ResponseHeadersToAdd) != 1 {
		t.Fatalf("got %d response headers, want 1", len(routes[0].ResponseHeadersToAdd))
	}
}

func TestSortGRPCRoutesByServiceThenMethod(t *testing.T) {
	short := &routev3.Route{Name: "short", Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Path{Path: "/a/hi"}}}
	long := &routev3.Route{Name: "long", Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Path{Path: "/abcdef/x"}}}
	setRouteSortMeta(short, true, 1, 2, metav1.Now(), "default", "short")
	setRouteSortMeta(long, true, 6, 1, metav1.Now(), "default", "long")
	routes := []*routev3.Route{short, long}
	sortGRPCRoutes(routes)
	if routes[0].Name != "long" {
		t.Errorf("first route = %q, want long (longer service wins)", routes[0].Name)
	}
}

func TestHTTPGRPCHostnameUniquenessPrefersOldest(t *testing.T) {
	listener := gatewayv1.Listener{Name: "http", Hostname: ptr.To(gatewayv1.Hostname("foo.example.com"))}
	oldTS := metav1.NewTime(time.Now().Add(-time.Hour))
	newTS := metav1.Now()
	httpRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "http", Namespace: "default", CreationTimestamp: newTS},
		Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"foo.example.com"}},
	}
	grpcRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default", CreationTimestamp: oldTS},
		Spec:       gatewayv1.GRPCRouteSpec{Hostnames: []gatewayv1.Hostname{"foo.example.com"}},
	}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{listener}},
	}
	httpBy := map[gatewayv1.SectionName][]*gatewayv1.HTTPRoute{listener.Name: {httpRoute}}
	grpcBy := map[gatewayv1.SectionName][]*gatewayv1.GRPCRoute{listener.Name: {grpcRoute}}
	httpStatus := map[types.NamespacedName][]gatewayv1.RouteParentStatus{
		{Namespace: "default", Name: "http"}: {{
			ParentRef: gatewayv1.ParentReference{Name: "gw"},
			Conditions: []metav1.Condition{{
				Type:   string(gatewayv1.RouteConditionAccepted),
				Status: metav1.ConditionTrue,
			}},
		}},
	}
	grpcStatus := map[types.NamespacedName][]gatewayv1.RouteParentStatus{
		{Namespace: "default", Name: "grpc"}: {{
			ParentRef: gatewayv1.ParentReference{Name: "gw"},
			Conditions: []metav1.Condition{{
				Type:   string(gatewayv1.RouteConditionAccepted),
				Status: metav1.ConditionTrue,
			}},
		}},
	}
	applyHTTPGRPCHostnameUniqueness(gw, httpBy, grpcBy, httpStatus, grpcStatus)
	if len(grpcBy[listener.Name]) != 1 {
		t.Fatalf("oldest GRPCRoute should remain, got %d", len(grpcBy[listener.Name]))
	}
	if len(httpBy[listener.Name]) != 0 {
		t.Fatalf("newer HTTPRoute should be rejected, got %d", len(httpBy[listener.Name]))
	}
	if meta.IsStatusConditionTrue(httpStatus[types.NamespacedName{Namespace: "default", Name: "http"}][0].Conditions, string(gatewayv1.RouteConditionAccepted)) {
		t.Error("HTTPRoute should have Accepted=False after hostname conflict")
	}
	got := meta.FindStatusCondition(httpStatus[types.NamespacedName{Namespace: "default", Name: "http"}][0].Conditions, string(gatewayv1.RouteConditionAccepted))
	want := hostnameConflictMessage("GRPCRoute", "default", "grpc")
	if got == nil || got.Message != want {
		t.Errorf("HTTPRoute conflict message = %v, want %q", got, want)
	}
}

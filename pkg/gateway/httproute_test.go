package gateway

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/ptr"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// --- mock service lister helpers ---

type mockServiceNamespaceLister struct {
	services map[string]*corev1.Service
}

func (m *mockServiceNamespaceLister) List(_ labels.Selector) ([]*corev1.Service, error) {
	var out []*corev1.Service
	for _, svc := range m.services {
		out = append(out, svc)
	}
	return out, nil
}

func (m *mockServiceNamespaceLister) Get(name string) (*corev1.Service, error) {
	if svc, ok := m.services[name]; ok {
		return svc, nil
	}
	return nil, fmt.Errorf("service %q not found", name)
}

type mockServiceLister struct {
	namespaces map[string]*mockServiceNamespaceLister
}

func (m *mockServiceLister) List(_ labels.Selector) ([]*corev1.Service, error) {
	var out []*corev1.Service
	for _, ns := range m.namespaces {
		svcs, _ := ns.List(labels.Everything())
		out = append(out, svcs...)
	}
	return out, nil
}

func (m *mockServiceLister) Services(namespace string) corev1listers.ServiceNamespaceLister {
	if ns, ok := m.namespaces[namespace]; ok {
		return ns
	}
	return &mockServiceNamespaceLister{} // empty namespace
}

// newMockServiceLister creates a lister pre-populated with the given services.
func newMockServiceLister(svcs ...*corev1.Service) corev1listers.ServiceLister {
	m := &mockServiceLister{namespaces: make(map[string]*mockServiceNamespaceLister)}
	for _, svc := range svcs {
		if _, ok := m.namespaces[svc.Namespace]; !ok {
			m.namespaces[svc.Namespace] = &mockServiceNamespaceLister{services: make(map[string]*corev1.Service)}
		}
		m.namespaces[svc.Namespace].services[svc.Name] = svc
	}
	return m
}

// makeService is a convenience constructor for test services.
func makeService(namespace, name string, port int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: port}},
		},
	}
}

// --- helpers to build HTTPRoute rules ---

func makeRuleWithFilters(filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		Filters: filters,
		BackendRefs: []gatewayv1.HTTPBackendRef{
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

func makeRuleWithBackend(svcName string) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{
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

func makeRuleWithMatch(match gatewayv1.HTTPRouteMatch) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{match},
		BackendRefs: []gatewayv1.HTTPBackendRef{
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

func redirectFilter() gatewayv1.HTTPRouteFilter {
	return gatewayv1.HTTPRouteFilter{
		Type:            gatewayv1.HTTPRouteFilterRequestRedirect,
		RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{},
	}
}

func headerModifierFilter() gatewayv1.HTTPRouteFilter {
	return gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set: []gatewayv1.HTTPHeader{{Name: "X-Foo", Value: "bar"}},
		},
	}
}

func urlRewriteFilter() gatewayv1.HTTPRouteFilter {
	return gatewayv1.HTTPRouteFilter{
		Type:       gatewayv1.HTTPRouteFilterURLRewrite,
		URLRewrite: &gatewayv1.HTTPURLRewriteFilter{},
	}
}

func responseHeaderFilter() gatewayv1.HTTPRouteFilter {
	return gatewayv1.HTTPRouteFilter{
		Type:                   gatewayv1.HTTPRouteFilterResponseHeaderModifier,
		ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{},
	}
}

func TestTranslateHTTPRouteToEnvoyRoutes(t *testing.T) {
	svc := makeService("default", "svc", 80)
	svcLister := newMockServiceLister(svc)
	noGrants := newFakeReferenceGrantLister(nil, nil)

	baseRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-route",
			Namespace:  "default",
			Generation: 1,
		},
	}

	tests := []struct {
		name       string
		rules      []gatewayv1.HTTPRouteRule
		wantRoutes int
		// wantAcceptedFalse: the route spec is invalid;
		// expect Accepted=False and resolvedRefs=nil.
		wantAcceptedFalse bool
		// wantResolvedRefsFalse: a backend ref could not be resolved;
		// expect resolvedRefs non-nil with ResolvedRefs=False.
		wantResolvedRefsFalse bool
		wantPartiallyInvalid  bool
	}{
		// --- match validation ---
		{
			name: "two rules with valid matches",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithMatch(gatewayv1.HTTPRouteMatch{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  ptr.To(gatewayv1.PathMatchExact),
						Value: ptr.To("/foo"),
					},
				}),
				makeRuleWithMatch(gatewayv1.HTTPRouteMatch{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
						Value: ptr.To("/bar"),
					},
				}),
			},
			wantRoutes: 2,
		},
		{
			name: "one valid match, one unsupported match type - partially invalid",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithMatch(gatewayv1.HTTPRouteMatch{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  ptr.To(gatewayv1.PathMatchExact),
						Value: ptr.To("/foo"),
					},
				}),
				makeRuleWithMatch(gatewayv1.HTTPRouteMatch{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  ptr.To(gatewayv1.PathMatchType("UnsupportedType")),
						Value: ptr.To("/bar"),
					},
				}),
			},
			wantRoutes:           1,
			wantPartiallyInvalid: true,
		},
		{
			name: "nil path value - fully invalid",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithMatch(gatewayv1.HTTPRouteMatch{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  ptr.To(gatewayv1.PathMatchExact),
						Value: nil,
					},
				}),
			},
			wantAcceptedFalse: true,
		},
		// This test is hypothetical since CRD validation would stop this one today,
		// but we want to be sure that a future addition to the spec will produce the
		// correct result
		{
			name: "unsupported path match type - fully invalid",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithMatch(gatewayv1.HTTPRouteMatch{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  ptr.To(gatewayv1.PathMatchType("UnsupportedType")),
						Value: ptr.To("/foo"),
					},
				}),
			},
			wantAcceptedFalse: true,
		},
		{
			name: "unsupported header match type - fully invalid",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithMatch(gatewayv1.HTTPRouteMatch{
					Headers: []gatewayv1.HTTPHeaderMatch{
						{
							Type:  ptr.To(gatewayv1.HeaderMatchType("UnsupportedType")),
							Name:  "X-Foo",
							Value: "bar",
						},
					},
				}),
			},
			wantAcceptedFalse: true,
		},
		// --- filter validation ---
		{
			name: "all supported filters - no PartiallyInvalid",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithFilters(redirectFilter()),
				makeRuleWithFilters(headerModifierFilter()),
			},
			wantRoutes: 2,
		},
		{
			name: "single rule with unsupported filter - fully invalid",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithFilters(urlRewriteFilter()),
			},
			wantAcceptedFalse: true,
		},
		{
			name: "all rules have unsupported filters - fully invalid",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithFilters(urlRewriteFilter()),
				makeRuleWithFilters(responseHeaderFilter()),
			},
			wantAcceptedFalse: true,
		},
		{
			name: "first rule unsupported filter, second rule supported - partially invalid",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithFilters(urlRewriteFilter()),
				makeRuleWithFilters(redirectFilter()),
			},
			wantRoutes:           1,
			wantPartiallyInvalid: true,
		},
		{
			name: "first rule supported, second rule unsupported filter - partially invalid",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithFilters(redirectFilter()),
				makeRuleWithFilters(urlRewriteFilter()),
			},
			wantRoutes:           1,
			wantPartiallyInvalid: true,
		},
		{
			name: "mix of supported and unsupported filters across three rules",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithFilters(redirectFilter()),
				makeRuleWithFilters(urlRewriteFilter()),
				makeRuleWithFilters(responseHeaderFilter()),
			},
			wantRoutes:           1,
			wantPartiallyInvalid: true,
		},
		{
			name:  "no rules at all",
			rules: []gatewayv1.HTTPRouteRule{},
		},
		// --- backend validation ---
		{
			name: "backend found - no ResolvedRefs failure",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithBackend("svc"),
			},
			wantRoutes: 1,
		},
		{
			name: "backend not found - ResolvedRefs=False",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithBackend("missing-svc"),
			},
			// The route is still programmed (with a 500 direct-response), so one route is produced.
			wantRoutes:            1,
			wantResolvedRefsFalse: true,
		},
		{
			name: "one rule dropped by unsupported filter, one rule with missing backend - partially invalid and ResolvedRefs=False",
			rules: []gatewayv1.HTTPRouteRule{
				makeRuleWithFilters(urlRewriteFilter()),
				makeRuleWithBackend("missing-svc"),
			},
			wantRoutes:            1,
			wantResolvedRefsFalse: true,
			wantPartiallyInvalid:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := baseRoute.DeepCopy()
			route.Spec.Rules = tt.rules

			got := translateHTTPRouteToEnvoyRoutes(route, svcLister, noGrants)
			routes := got.routes
			notAccepted := got.notAccepted
			resolvedRefsFailure := got.resolvedRefsFailure
			partiallyInvalid := got.partiallyInvalid

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
				if notAccepted.Reason != string(gatewayv1.RouteReasonUnsupportedValue) {
					t.Errorf("Accepted.Reason = %q, want %q", notAccepted.Reason, gatewayv1.RouteReasonUnsupportedValue)
				}
				if resolvedRefsFailure != nil {
					t.Errorf("unexpected ResolvedRefs condition when route is fully rejected")
				}
				if partiallyInvalid != nil {
					t.Errorf("unexpected PartiallyInvalid condition when route is fully rejected")
				}
			} else {
				if notAccepted != nil {
					t.Errorf("unexpected Accepted=False condition")
				}
			}

			if tt.wantResolvedRefsFalse {
				if resolvedRefsFailure == nil {
					t.Fatalf("ResolvedRefs condition is nil, want ResolvedRefs=False")
				}
				if resolvedRefsFailure.Status != metav1.ConditionFalse {
					t.Errorf("ResolvedRefs.Status = %q, want False", resolvedRefsFailure.Status)
				}
			} else {
				if resolvedRefsFailure != nil {
					t.Errorf("unexpected ResolvedRefs condition (status=%q): success should be signalled by absence", resolvedRefsFailure.Status)
				}
			}

			if tt.wantPartiallyInvalid {
				if partiallyInvalid == nil {
					t.Fatalf("PartiallyInvalid condition missing, want PartiallyInvalid=True")
				}
				if partiallyInvalid.Status != metav1.ConditionTrue {
					t.Errorf("PartiallyInvalid.Status = %q, want True", partiallyInvalid.Status)
				}
				if partiallyInvalid.Reason != string(gatewayv1.RouteReasonUnsupportedValue) {
					t.Errorf("PartiallyInvalid.Reason = %q, want %q", partiallyInvalid.Reason, gatewayv1.RouteReasonUnsupportedValue)
				}
				if !strings.HasPrefix(partiallyInvalid.Message, "Dropped Rule") {
					t.Errorf("PartiallyInvalid.Message = %q, want prefix \"Dropped Rule\"", partiallyInvalid.Message)
				}
				if notAccepted != nil {
					t.Errorf("unexpected Accepted=False with PartiallyInvalid=True")
				}
			} else if partiallyInvalid != nil {
				t.Errorf("unexpected PartiallyInvalid condition (status=%q)", partiallyInvalid.Status)
			}
		})
	}
}

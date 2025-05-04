package gateway

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/ptr"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestIsRouteReferenced(t *testing.T) {
	var (
		gwGroup    = gatewayv1.Group(gatewayv1.GroupName)
		gwKind     = gatewayv1.Kind("Gateway")
		otherGroup = gatewayv1.Group("other.group")
		otherKind  = gatewayv1.Kind("OtherKind")
	)

	testGateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "test-ns",
		},
	}

	testListener := gatewayv1.Listener{
		Name:     "test-listener",
		Port:     80,
		Protocol: gatewayv1.HTTPProtocolType,
	}

	tests := []struct {
		name     string
		gateway  *gatewayv1.Gateway
		listener gatewayv1.Listener
		route    metav1.Object
		want     bool
	}{
		{
			name:     "HTTPRoute with no ParentRefs",
			gateway:  testGateway,
			listener: testListener,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec:       gatewayv1.HTTPRouteSpec{}, // No ParentRefs
			},
			want: false,
		},
		{
			name:     "HTTPRoute with matching ParentRef (name, namespace)",
			gateway:  testGateway,
			listener: testListener,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway"}, // Namespace defaults to route's ns
						},
					},
				},
			},
			want: true,
		},
		{
			name:     "HTTPRoute with matching ParentRef (name, explicit namespace)",
			gateway:  testGateway,
			listener: testListener,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway", Namespace: ptr.To(gatewayv1.Namespace("test-ns"))},
						},
					},
				},
			},
			want: true,
		},
		{
			name:     "HTTPRoute with matching ParentRef (name, namespace, sectionName)",
			gateway:  testGateway,
			listener: testListener, // Listener name is "test-listener"
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway", SectionName: ptr.To(gatewayv1.SectionName("test-listener"))},
						},
					},
				},
			},
			want: true,
		},
		{
			name:     "HTTPRoute with matching ParentRef (name, namespace, port)",
			gateway:  testGateway,
			listener: testListener, // Listener port is 80
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway", Port: ptr.To(gatewayv1.PortNumber(80))},
						},
					},
				},
			},
			want: true,
		},
		{
			name:     "HTTPRoute with matching ParentRef (name, namespace, sectionName, port)",
			gateway:  testGateway,
			listener: testListener, // Listener name "test-listener", port 80
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway", SectionName: ptr.To(gatewayv1.SectionName("test-listener")), Port: ptr.To(gatewayv1.PortNumber(80))},
						},
					},
				},
			},
			want: true,
		},
		{
			name:     "HTTPRoute with non-matching SectionName",
			gateway:  testGateway,
			listener: testListener, // Listener name is "test-listener"
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway", SectionName: ptr.To(gatewayv1.SectionName("wrong-listener"))}, // Mismatch
						},
					},
				},
			},
			want: false,
		},
		{
			name:     "HTTPRoute with non-matching Port",
			gateway:  testGateway,
			listener: testListener, // Listener port is 80
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway", Port: ptr.To(gatewayv1.PortNumber(9090))}, // Mismatch
						},
					},
				},
			},
			want: false,
		},
		{
			name:     "HTTPRoute with non-matching Gateway Name",
			gateway:  testGateway,
			listener: testListener,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "wrong-gateway"}, // Mismatch
						},
					},
				},
			},
			want: false,
		},
		{
			name:     "HTTPRoute with non-Gateway Kind",
			gateway:  testGateway,
			listener: testListener,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway", Kind: &otherKind}, // Mismatch
						},
					},
				},
			},
			want: false,
		},
		{
			name:     "HTTPRoute with non-Gateway Group",
			gateway:  testGateway,
			listener: testListener,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway", Group: &otherGroup}, // Mismatch
						},
					},
				},
			},
			want: false,
		},
		{
			name:     "GRPCRoute with matching ParentRef",
			gateway:  testGateway,
			listener: testListener, // Assuming listener protocol allows gRPC
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "grpc-route1", Namespace: "test-ns"},
				Spec: gatewayv1.GRPCRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway", SectionName: ptr.To(gatewayv1.SectionName("test-listener")), Port: ptr.To(gatewayv1.PortNumber(80))},
						},
					},
				},
			},
			want: true,
		},
		{
			name:     "HTTPRoute with multiple ParentRefs, one matching",
			gateway:  testGateway,
			listener: testListener,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "wrong-gateway"},
							{Name: "test-gateway", SectionName: ptr.To(gatewayv1.SectionName("test-listener"))}, // Match
							{Name: "test-gateway", SectionName: ptr.To(gatewayv1.SectionName("other-listener"))},
						},
					},
				},
			},
			want: true,
		},
		{
			name:     "HTTPRoute with multiple ParentRefs, none matching",
			gateway:  testGateway,
			listener: testListener, // Listener name is "test-listener"
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "wrong-gateway"},
							{Name: "test-gateway", SectionName: ptr.To(gatewayv1.SectionName("other-listener"))},
							{Name: "test-gateway", Port: ptr.To(gatewayv1.PortNumber(9090))},
						},
					},
				},
			},
			want: false,
		},
		{
			name:     "HTTPRoute ParentRef with default Group/Kind",
			gateway:  testGateway,
			listener: testListener,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway"}, // Group/Kind defaults should match Gateway
						},
					},
				},
			},
			want: true,
		},
		{
			name:     "HTTPRoute ParentRef with explicit Group/Kind",
			gateway:  testGateway,
			listener: testListener,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "test-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway", Group: &gwGroup, Kind: &gwKind}, // Explicitly matches Gateway
						},
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRouteReferenced(tt.gateway, tt.listener, tt.route); got != tt.want {
				t.Errorf("isRouteReferenced() = %v, want %v", got, tt.want)
			}
		})
	}
}

type mockNamespaceLister struct {
	namespaces map[string]*corev1.Namespace
}

func (m *mockNamespaceLister) List(selector labels.Selector) (ret []*corev1.Namespace, err error) {
	for _, ns := range m.namespaces {
		if selector.Matches(labels.Set(ns.Labels)) {
			ret = append(ret, ns)
		}
	}
	return ret, nil
}

func (m *mockNamespaceLister) Get(name string) (*corev1.Namespace, error) {
	ns, ok := m.namespaces[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "namespaces"}, name)
	}
	return ns, nil
}

func TestIsRouteAllowed(t *testing.T) {
	testGateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "gateway-ns",
		},
	}

	httpRouteKind := gatewayv1.RouteGroupKind{Kind: "HTTPRoute"}
	grpcRouteKind := gatewayv1.RouteGroupKind{Group: ptr.To(gatewayv1.Group(gatewayv1.GroupName)), Kind: "GRPCRoute"}
	otherRouteKind := gatewayv1.RouteGroupKind{Group: ptr.To(gatewayv1.Group("other.group")), Kind: "OtherRoute"}

	mockNsLister := &mockNamespaceLister{
		namespaces: map[string]*corev1.Namespace{
			"gateway-ns": {ObjectMeta: metav1.ObjectMeta{Name: "gateway-ns", Labels: map[string]string{"type": "gateway"}}},
			"route-ns-1": {ObjectMeta: metav1.ObjectMeta{Name: "route-ns-1", Labels: map[string]string{"app": "foo"}}},
			"route-ns-2": {ObjectMeta: metav1.ObjectMeta{Name: "route-ns-2", Labels: map[string]string{"app": "bar"}}},
		},
	}

	tests := []struct {
		name          string
		listener      gatewayv1.Listener
		route         metav1.Object
		namespaceList corev1listers.NamespaceLister // Use the interface type
		want          bool
	}{
		{
			name: "Nil AllowedRoutes, route in same namespace",
			listener: gatewayv1.Listener{
				Name:          "listener-1",
				AllowedRoutes: nil, // Default should be Same namespace
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "gateway-ns"}, // Same ns
			},
			namespaceList: mockNsLister,
			want:          true,
		},
		{
			name: "Nil AllowedRoutes, route in different namespace",
			listener: gatewayv1.Listener{
				Name:          "listener-1",
				AllowedRoutes: nil, // Default should be Same namespace
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "other-ns"}, // Different ns
			},
			namespaceList: mockNsLister,
			want:          false,
		},
		{
			name: "NamespacesFromSame, route in same namespace",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromSame)},
				},
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "gateway-ns"}, // Same ns
			},
			namespaceList: mockNsLister,
			want:          true,
		},
		{
			name: "NamespacesFromSame, route in different namespace",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromSame)},
				},
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "other-ns"}, // Different ns
			},
			namespaceList: mockNsLister,
			want:          false,
		},
		{
			name: "NamespacesFromAll, route in different namespace",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)},
				},
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "other-ns"}, // Different ns
			},
			namespaceList: mockNsLister,
			want:          true,
		},
		{
			name: "NamespacesFromSelector, matching namespace",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{
						From:     ptr.To(gatewayv1.NamespacesFromSelector),
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
					},
				},
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "route-ns-1"}, // Matches selector
			},
			namespaceList: mockNsLister,
			want:          true,
		},
		{
			name: "NamespacesFromSelector, non-matching namespace",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{
						From:     ptr.To(gatewayv1.NamespacesFromSelector),
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
					},
				},
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "route-ns-2"}, // Does not match selector
			},
			namespaceList: mockNsLister,
			want:          false,
		},
		{
			name: "NamespacesFromSelector, namespace not found",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{
						From:     ptr.To(gatewayv1.NamespacesFromSelector),
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
					},
				},
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "non-existent-ns"}, // NS doesn't exist
			},
			namespaceList: mockNsLister,
			want:          false,
		},
		{
			name: "Allowed Kind matches HTTPRoute (default group)",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)},
					Kinds:      []gatewayv1.RouteGroupKind{httpRouteKind},
				},
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
			},
			namespaceList: mockNsLister,
			want:          true,
		},
		{
			name: "Allowed Kind matches GRPCRoute (explicit group)",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)},
					Kinds:      []gatewayv1.RouteGroupKind{grpcRouteKind},
				},
			},
			route: &gatewayv1.GRPCRoute{ // GRPCRoute type
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
			},
			namespaceList: mockNsLister,
			want:          true,
		},
		{
			name: "Allowed Kind does not match route kind",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)},
					Kinds:      []gatewayv1.RouteGroupKind{grpcRouteKind}, // Only allow GRPCRoute
				},
			},
			route: &gatewayv1.HTTPRoute{ // Route is HTTPRoute
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
			},
			namespaceList: mockNsLister,
			want:          false,
		},
		{
			name: "Allowed Kind does not match route group",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)},
					Kinds:      []gatewayv1.RouteGroupKind{otherRouteKind}, // Allow other.group/OtherRoute
				},
			},
			route: &gatewayv1.HTTPRoute{ // Route is gateway.networking.k8s.io/HTTPRoute
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
			},
			namespaceList: mockNsLister,
			want:          false,
		},
		{
			name: "Empty Kinds list allows compatible kinds (HTTPRoute)",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)},
					Kinds:      []gatewayv1.RouteGroupKind{}, // Empty list
				},
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
			},
			namespaceList: mockNsLister,
			want:          true, // Namespace check passes, empty Kinds passes
		},
		{
			name: "Namespace allowed, Kind denied",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromSame)}, // Same NS allowed
					Kinds:      []gatewayv1.RouteGroupKind{grpcRouteKind},                              // Only GRPCRoute allowed
				},
			},
			route: &gatewayv1.HTTPRoute{ // HTTPRoute in same NS
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "gateway-ns"},
			},
			namespaceList: mockNsLister,
			want:          false, // Kind check fails
		},
		{
			name: "Namespace denied, Kind allowed",
			listener: gatewayv1.Listener{
				Name: "listener-1",
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromSame)}, // Same NS allowed
					Kinds:      []gatewayv1.RouteGroupKind{httpRouteKind},                              // HTTPRoute allowed
				},
			},
			route: &gatewayv1.HTTPRoute{ // HTTPRoute in different NS
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "other-ns"},
			},
			namespaceList: mockNsLister,
			want:          false, // Namespace check fails
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRouteAllowed(testGateway, tt.listener, tt.route, tt.namespaceList); got != tt.want {
				t.Errorf("isRouteAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

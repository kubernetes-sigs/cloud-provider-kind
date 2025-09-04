package gateway

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/ptr"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// A mock implementation of NamespaceLister for testing purposes.
type mockNamespaceLister struct {
	namespaces map[string]*corev1.Namespace
}

func (m *mockNamespaceLister) List(selector labels.Selector) ([]*corev1.Namespace, error) {
	var matching []*corev1.Namespace
	for _, ns := range m.namespaces {
		if selector.Matches(labels.Set(ns.Labels)) {
			matching = append(matching, ns)
		}
	}
	return matching, nil
}

func (m *mockNamespaceLister) Get(name string) (*corev1.Namespace, error) {
	if ns, ok := m.namespaces[name]; ok {
		return ns, nil
	}
	return nil, fmt.Errorf("namespace %s not found", name)
}

func (m *mockNamespaceLister) GetPodNamespaces(pod *corev1.Pod) ([]*corev1.Namespace, error) {
	return nil, nil
}

func (m *mockNamespaceLister) Pods(namespace string) corev1listers.PodNamespaceLister {
	return nil
}

func TestIsAllowedByListener(t *testing.T) {
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
			if got := isAllowedByListener(testGateway, tt.listener, tt.route, tt.namespaceList); got != tt.want {
				t.Errorf("isAllowedByListener() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsAllowedByHostname(t *testing.T) {
	tests := []struct {
		name     string
		listener gatewayv1.Listener
		route    metav1.Object
		want     bool
	}{
		{
			name: "Listener has no hostname, allows any route hostname",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: nil,
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
				Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"foo.example.com"}},
			},
			want: true,
		},
		{
			name: "Route has no hostname, inherits from listener",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("bar.example.com")),
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
				Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{}},
			},
			want: true,
		},
		{
			name: "Exact match",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("foo.example.com")),
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
				Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"foo.example.com"}},
			},
			want: true,
		},
		{
			name: "Wildcard listener, subdomain route",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("*.example.com")),
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
				Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"foo.example.com"}},
			},
			want: true,
		},
		{
			name: "No match",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("foo.example.com")),
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
				Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"bar.example.com"}},
			},
			want: false,
		},
		{
			name: "Wildcard listener, non-matching route",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("*.example.com")),
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
				Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"foo.another.com"}},
			},
			want: false,
		},
		{
			name: "Wildcard listener, parent domain route (not allowed)",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("*.example.com")),
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "any-ns"},
				Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"example.com"}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAllowedByHostname(tt.listener, tt.route); got != tt.want {
				t.Errorf("isAllowedByHostname() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsHostnameSubset(t *testing.T) {
	// Test cases are derived directly from the Gateway API specification
	// for hostname matching.
	testCases := []struct {
		name             string
		listenerHostname string
		routeHostname    string
		want             bool
	}{
		// --- Core Scenarios from Spec ---
		{
			name:             "Spec: Exact match",
			listenerHostname: "test.example.com",
			routeHostname:    "test.example.com",
			want:             true,
		},
		{
			name:             "Spec: Wildcard route matches specific listener",
			listenerHostname: "test.example.com",
			routeHostname:    "*.example.com",
			want:             true,
		},
		{
			name:             "Spec: Specific route matches wildcard listener",
			listenerHostname: "*.example.com",
			routeHostname:    "test.example.com",
			want:             true,
		},
		{
			name:             "Spec: Multi-level specific route matches wildcard listener",
			listenerHostname: "*.example.com",
			routeHostname:    "foo.test.example.com",
			want:             true,
		},
		{
			name:             "Spec: Identical wildcards match",
			listenerHostname: "*.example.com",
			routeHostname:    "*.example.com",
			want:             true,
		},

		// --- Explicit "Not Matching" Scenarios from Spec ---
		{
			name:             "Spec: Parent domain does not match wildcard listener",
			listenerHostname: "*.example.com",
			routeHostname:    "example.com",
			want:             false,
		},
		{
			name:             "Spec: Different TLD does not match wildcard listener",
			listenerHostname: "*.example.com",
			routeHostname:    "test.example.net",
			want:             false,
		},

		// --- Additional Edge Cases ---
		{
			name:             "Route with more specific wildcard matches listener",
			listenerHostname: "*.example.com",
			routeHostname:    "*.foo.example.com",
			want:             true,
		},
		{
			name:             "Route with less specific wildcard does NOT match listener",
			listenerHostname: "*.foo.example.com",
			routeHostname:    "*.example.com",
			want:             false,
		},
		{
			name:             "Mismatched specific hostnames",
			listenerHostname: "foo.example.com",
			routeHostname:    "bar.example.com",
			want:             false,
		},
		{
			name:             "Wildcard route does not match different specific TLD",
			listenerHostname: "foo.example.org",
			routeHostname:    "*.example.com",
			want:             false,
		},
		{
			name:             "Wildcard route does match specific TLD",
			listenerHostname: "very.specific.com",
			routeHostname:    "*.specific.com",
			want:             true,
		},
		{
			name:             "Wildcard route does match partially",
			listenerHostname: "*.specific.com",
			routeHostname:    "*.muchspecific.com",
			want:             false,
		},
		{
			name:             "Wildcard route does match partially",
			listenerHostname: "*.muchspecific.com",
			routeHostname:    "*.specific.com",
			want:             false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHostnameSubset(tc.routeHostname, tc.listenerHostname); got != tc.want {
				t.Errorf("isHostnameSubset(route: %q, listener: %q) = %v; want %v", tc.routeHostname, tc.listenerHostname, got, tc.want)
			}
		})
	}
}

func TestGetIntersectingHostnames(t *testing.T) {
	tests := []struct {
		name           string
		listener       gatewayv1.Listener
		routeHostnames []gatewayv1.Hostname
		want           []string
	}{
		{
			name: "Listener has no hostname, route has none",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: nil,
			},
			routeHostnames: []gatewayv1.Hostname{},
			want:           []string{"*"},
		},
		{
			name: "Listener has no hostname, route has one",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: nil,
			},
			routeHostnames: []gatewayv1.Hostname{"foo.example.com"},
			want:           []string{"foo.example.com"},
		},
		{
			name: "Listener has no hostname, route has multiple",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: nil,
			},
			routeHostnames: []gatewayv1.Hostname{"foo.example.com", "bar.example.com"},
			want:           []string{"foo.example.com", "bar.example.com"},
		},
		{
			name: "Listener has specific hostname, route has none",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("listener.example.com")),
			},
			routeHostnames: []gatewayv1.Hostname{},
			want:           []string{"listener.example.com"},
		},
		{
			name: "Exact match",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("foo.example.com")),
			},
			routeHostnames: []gatewayv1.Hostname{"foo.example.com"},
			want:           []string{"foo.example.com"},
		},
		{
			name: "Wildcard listener, specific route",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("*.example.com")),
			},
			routeHostnames: []gatewayv1.Hostname{"foo.example.com"},
			want:           []string{"foo.example.com"},
		},
		{
			name: "Specific listener, wildcard route",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("foo.example.com")),
			},
			routeHostnames: []gatewayv1.Hostname{"*.example.com"},
			// The result is the listener's more specific hostname
			want: []string{"foo.example.com"},
		},
		{
			name: "Wildcard listener, more specific wildcard route",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("*.example.com")),
			},
			routeHostnames: []gatewayv1.Hostname{"*.foo.example.com"},
			want:           []string{"*.foo.example.com"},
		},
		{
			name: "No intersection",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("a.com")),
			},
			routeHostnames: []gatewayv1.Hostname{"b.com"},
			want:           []string{},
		},
		{
			name: "Multiple valid intersections",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("*.example.com")),
			},
			routeHostnames: []gatewayv1.Hostname{"a.example.com", "b.example.com", "no.match.org"},
			want:           []string{"a.example.com", "b.example.com"},
		},
		{
			name: "Complex intersection with specific and wildcard",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("*.example.com")),
			},
			routeHostnames: []gatewayv1.Hostname{"a.example.com", "*.b.example.com", "example.com"},
			// "example.com" is not a valid subset
			want: []string{"a.example.com", "*.b.example.com"},
		},
		{
			name: "No intersection wildcards are per path",
			listener: gatewayv1.Listener{
				Name:     "listener-1",
				Hostname: ptr.To(gatewayv1.Hostname("*.wildcard.com")),
			},
			routeHostnames: []gatewayv1.Hostname{"*.examplewildcard.com", "*.b.example.com", "example.com"},
			want:           []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getIntersectingHostnames(tt.listener, tt.routeHostnames)
			if !equalUnordered(got, tt.want) {
				t.Errorf("getIntersectingHostnames() = %v, want %v", got, tt.want)
			}
		})
	}
}

// equalUnordered checks if two string slices are equal, ignoring order.
func equalUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		if m[v] == 0 {
			return false
		}
		m[v]--
	}
	return true
}

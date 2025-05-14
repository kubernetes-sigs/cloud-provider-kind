package gateway

import (
	"testing"

	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestHostnameMatches(t *testing.T) {
	tests := []struct {
		name             string
		listenerHostname gatewayv1.Hostname
		routeHostname    gatewayv1.Hostname
		want             bool
	}{
		{"exact match", "foo.example.com", "foo.example.com", true},
		{"listener wildcard, route exact match", "*.example.com", "foo.example.com", true},
		{"listener wildcard, route exact non-match (diff domain)", "*.example.com", "foo.example.net", false},
		{"listener wildcard, route exact non-match (domain itself)", "*.example.com", "example.com", false},
		{"listener wildcard, route wildcard match (subdomain)", "*.foo.example.com", "*.example.com", true},
		{"listener wildcard, route wildcard match (same)", "*.example.com", "*.example.com", true},
		{"listener wildcard, route wildcard non-match", "*.example.com", "*.example.net", false},
		{"listener exact, route wildcard match", "foo.example.com", "*.example.com", true},
		{"listener exact, route wildcard non-match (diff domain)", "foo.example.net", "*.example.com", false},
		{"listener exact, route wildcard non-match (domain itself)", "example.com", "*.example.com", false},
		{"no wildcard, non-match", "foo.example.com", "bar.example.com", false},
		{"no wildcard, non-match diff domain", "foo.example.com", "foo.example.net", false},
		{"empty listener", "", "foo.example.com", false}, // Exact match fails
		{"empty route", "foo.example.com", "", false},    // Exact match fails
		{"both empty", "", "", true}, // Exact match
		{"listener wildcard, route empty", "*.example.com", "", false},
		{"listener empty, route wildcard", "", "*.example.com", false},
		{"complex wildcard listener match", "*.apps.example.com", "foo.bar.apps.example.com", true},
		{"complex wildcard route match", "foo.bar.apps.example.com", "*.apps.example.com", true},
		{"wildcard vs non-wildcard TLD", "*.com", "example.com", true}, // *.com matches example.com
		{"non-wildcard TLD vs wildcard", "example.com", "*.com", true}, // *.com matches example.com
		{"wildcard TLD vs TLD", "*.com", "com", false},                 // *.com does not match com
		{"TLD vs wildcard TLD", "com", "*.com", false},                 // *.com does not match com
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostnameMatches(tt.listenerHostname, tt.routeHostname); got != tt.want {
				t.Errorf("hostnameMatches(%q, %q) = %v, want %v", tt.listenerHostname, tt.routeHostname, got, tt.want)
			}
		})
	}
}

func TestHostnamesIntersect(t *testing.T) {
	tests := []struct {
		name             string
		listenerHostname *gatewayv1.Hostname
		routeHostnames   []gatewayv1.Hostname
		want             bool
	}{
		{
			name:             "empty route hostnames",
			listenerHostname: ptr.To(gatewayv1.Hostname("foo.example.com")),
			routeHostnames:   []gatewayv1.Hostname{},
			want:             true,
		},
		{
			name:             "nil listener hostname",
			listenerHostname: nil,
			routeHostnames:   []gatewayv1.Hostname{"foo.example.com"},
			want:             true,
		},
		{
			name:             "empty listener hostname",
			listenerHostname: ptr.To(gatewayv1.Hostname("")),
			routeHostnames:   []gatewayv1.Hostname{"foo.example.com"},
			want:             true,
		},
		{
			name:             "exact match intersection",
			listenerHostname: ptr.To(gatewayv1.Hostname("foo.example.com")),
			routeHostnames:   []gatewayv1.Hostname{"bar.example.com", "foo.example.com"},
			want:             true,
		},
		{
			name:             "wildcard listener intersection",
			listenerHostname: ptr.To(gatewayv1.Hostname("*.example.com")),
			routeHostnames:   []gatewayv1.Hostname{"bar.example.net", "foo.example.com"},
			want:             true,
		},
		{
			name:             "wildcard route intersection",
			listenerHostname: ptr.To(gatewayv1.Hostname("foo.example.com")),
			routeHostnames:   []gatewayv1.Hostname{"bar.example.net", "*.example.com"},
			want:             true,
		},
		{
			name:             "wildcard both intersection",
			listenerHostname: ptr.To(gatewayv1.Hostname("*.example.com")),
			routeHostnames:   []gatewayv1.Hostname{"bar.example.net", "*.apps.example.com"},
			want:             true,
		},
		{
			name:             "no intersection",
			listenerHostname: ptr.To(gatewayv1.Hostname("foo.example.com")),
			routeHostnames:   []gatewayv1.Hostname{"bar.example.com", "baz.example.com"},
			want:             false,
		},
		{
			name:             "no intersection with wildcards",
			listenerHostname: ptr.To(gatewayv1.Hostname("*.example.net")),
			routeHostnames:   []gatewayv1.Hostname{"bar.example.com", "*.apps.example.com"},
			want:             false,
		},
		{
			name:             "listener wildcard, route domain itself (no intersection)",
			listenerHostname: ptr.To(gatewayv1.Hostname("*.example.com")),
			routeHostnames:   []gatewayv1.Hostname{"example.com", "foo.example.net"},
			want:             false,
		},
		{
			name:             "listener exact, route wildcard domain itself (no intersection)",
			listenerHostname: ptr.To(gatewayv1.Hostname("example.com")),
			routeHostnames:   []gatewayv1.Hostname{"*.example.com", "foo.example.net"},
			want:             false,
		},
		{
			name:             "both nil/empty",
			listenerHostname: nil,
			routeHostnames:   []gatewayv1.Hostname{},
			want:             true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostnamesIntersect(tt.listenerHostname, tt.routeHostnames); got != tt.want {
				t.Errorf("hostnamesIntersect() = %v, want %v", got, tt.want)
			}
		})
	}
}

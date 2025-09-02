package gateway

import (
	"reflect"
	"testing"

	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func Test_getSupportedKinds(t *testing.T) {
	type args struct {
		listener gatewayv1.Listener
	}
	group := gatewayv1.Group(gatewayv1.GroupName)
	tests := []struct {
		name  string
		args  args
		want  []gatewayv1.RouteGroupKind
		want1 bool
	}{
		{
			name: "default kinds for HTTP protocol",
			args: args{
				listener: gatewayv1.Listener{
					Protocol: gatewayv1.HTTPProtocolType,
				},
			},
			want: []gatewayv1.RouteGroupKind{
				{Group: &group, Kind: "HTTPRoute"},
				{Group: &group, Kind: "GRPCRoute"},
			},
			want1: true,
		},
		{
			name: "default kinds for HTTPS protocol",
			args: args{
				listener: gatewayv1.Listener{
					Protocol: gatewayv1.HTTPSProtocolType,
				},
			},
			want: []gatewayv1.RouteGroupKind{
				{Group: &group, Kind: "HTTPRoute"},
				{Group: &group, Kind: "GRPCRoute"},
			},
			want1: true,
		},
		{
			name: "no default kinds for other protocols",
			args: args{
				listener: gatewayv1.Listener{
					Protocol: gatewayv1.TCPProtocolType,
				},
			},
			want:  []gatewayv1.RouteGroupKind{},
			want1: true,
		},
		{
			name: "user defined kinds",
			args: args{
				listener: gatewayv1.Listener{
					Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Kinds: []gatewayv1.RouteGroupKind{
							{Kind: "HTTPRoute"},
						},
					},
				},
			},
			want: []gatewayv1.RouteGroupKind{
				{Group: &group, Kind: "HTTPRoute"},
			},
			want1: true,
		},
		{
			name: "user defined kinds with invalid kind",
			args: args{
				listener: gatewayv1.Listener{
					Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Kinds: []gatewayv1.RouteGroupKind{
							{Kind: "HTTPRoute"},
							{Kind: "TCPRoute"},
						},
					},
				},
			},
			want: []gatewayv1.RouteGroupKind{
				{Group: &group, Kind: "HTTPRoute"},
			},
			want1: false,
		},
		{
			name: "user defined kinds with invalid group",
			args: args{
				listener: gatewayv1.Listener{
					Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Kinds: []gatewayv1.RouteGroupKind{
							{Group: ptr.To(gatewayv1.Group("foo")), Kind: "HTTPRoute"},
						},
					},
				},
			},
			want:  []gatewayv1.RouteGroupKind{},
			want1: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1 := getSupportedKinds(tt.args.listener)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getSupportedKinds() got = %v, want %v", got, tt.want)
			}
			if got1 != tt.want1 {
				t.Errorf("getSupportedKinds() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}
}

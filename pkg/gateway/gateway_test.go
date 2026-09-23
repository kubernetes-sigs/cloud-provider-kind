package gateway

import (
	"reflect"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
				{Group: &group, Kind: "GRPCRoute"},
				{Group: &group, Kind: "HTTPRoute"},
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
				{Group: &group, Kind: "GRPCRoute"},
				{Group: &group, Kind: "HTTPRoute"},
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
			sortRouteGroupKinds(got)
			sortRouteGroupKinds(tt.want)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getSupportedKinds() got = %v, want %v", got, tt.want)
			}
			if got1 != tt.want1 {
				t.Errorf("getSupportedKinds() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}
}

func sortRouteGroupKinds(kinds []gatewayv1.RouteGroupKind) {
	sort.Slice(kinds, func(i, j int) bool {
		return kinds[i].Kind < kinds[j].Kind
	})
}

func TestRecordAcceptedRoute(t *testing.T) {
	statuses := make(map[types.NamespacedName][]gatewayv1.RouteParentStatus)
	var attached []gatewayv1.SectionName
	parentStatuses := []gatewayv1.RouteParentStatus{{ParentRef: gatewayv1.ParentReference{Name: "gw"}}}
	listeners := []gatewayv1.Listener{
		{Name: "http"},
		{Name: "http"},
		{Name: "https"},
	}

	recordAcceptedRoute("route", "default", parentStatuses, listeners, statuses, func(name gatewayv1.SectionName) {
		attached = append(attached, name)
	})

	key := types.NamespacedName{Name: "route", Namespace: "default"}
	if !reflect.DeepEqual(statuses[key], parentStatuses) {
		t.Errorf("statuses[%v] = %v, want %v", key, statuses[key], parentStatuses)
	}
	wantAttached := []gatewayv1.SectionName{"http", "https"}
	if !reflect.DeepEqual(attached, wantAttached) {
		t.Errorf("attached = %v, want %v", attached, wantAttached)
	}

	recordAcceptedRoute("empty", "default", nil, nil, statuses, func(gatewayv1.SectionName) {
		t.Error("attach should not be called for empty statuses and listeners")
	})
	if _, ok := statuses[types.NamespacedName{Name: "empty", Namespace: "default"}]; ok {
		t.Error("empty parent statuses should not be stored")
	}
}

func TestMergeTranslatedRouteStatus(t *testing.T) {
	key := types.NamespacedName{Name: "route", Namespace: "default"}
	statuses := map[types.NamespacedName][]gatewayv1.RouteParentStatus{
		key: {{
			ParentRef: gatewayv1.ParentReference{Name: "gw"},
			Conditions: []metav1.Condition{{
				Type:   string(gatewayv1.RouteConditionAccepted),
				Status: metav1.ConditionTrue,
			}},
		}},
	}
	resolved := createNotResolvedCondition(gatewayv1.RouteReasonBackendNotFound, "reference to Service default/missing not found", 1)
	mergeTranslatedRouteStatus("route", "default", 1, statuses, nil, &resolved, nil)

	got := meta.FindStatusCondition(statuses[key][0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	if got == nil || got.Status != metav1.ConditionFalse {
		t.Fatalf("ResolvedRefs = %v, want False", got)
	}
	if got.Message != resolved.Message {
		t.Errorf("ResolvedRefs.Message = %q, want %q", got.Message, resolved.Message)
	}
}

/*
Copyright 2026 The Kubernetes Authors.

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
	"testing"
	"time"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

type fakeBackendTLSPolicyLister struct {
	policies []*gatewayv1.BackendTLSPolicy
}

func (f *fakeBackendTLSPolicyLister) List(selector labels.Selector) ([]*gatewayv1.BackendTLSPolicy, error) {
	return f.policies, nil
}

func (f *fakeBackendTLSPolicyLister) Get(name string) (*gatewayv1.BackendTLSPolicy, error) {
	for _, p := range f.policies {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, nil
}

func (f *fakeBackendTLSPolicyLister) BackendTLSPolicies(namespace string) gatewaylisters.BackendTLSPolicyNamespaceLister {
	var filtered []*gatewayv1.BackendTLSPolicy
	for _, p := range f.policies {
		if p.Namespace == namespace {
			filtered = append(filtered, p)
		}
	}
	return &fakeBackendTLSPolicyLister{policies: filtered}
}

type fakeConfigMapLister struct {
	configMaps []*corev1.ConfigMap
}

func (f *fakeConfigMapLister) List(selector labels.Selector) ([]*corev1.ConfigMap, error) {
	return f.configMaps, nil
}

func (f *fakeConfigMapLister) Get(name string) (*corev1.ConfigMap, error) {
	for _, cm := range f.configMaps {
		if cm.Name == name {
			return cm, nil
		}
	}
	return nil, &notFoundError{name: name}
}

func (f *fakeConfigMapLister) ConfigMaps(namespace string) corev1listers.ConfigMapNamespaceLister {
	var filtered []*corev1.ConfigMap
	for _, cm := range f.configMaps {
		if cm.Namespace == namespace {
			filtered = append(filtered, cm)
		}
	}
	return &fakeConfigMapNamespaceLister{configMaps: filtered}
}

type fakeConfigMapNamespaceLister struct {
	configMaps []*corev1.ConfigMap
}

func (f *fakeConfigMapNamespaceLister) List(selector labels.Selector) ([]*corev1.ConfigMap, error) {
	return f.configMaps, nil
}

func (f *fakeConfigMapNamespaceLister) Get(name string) (*corev1.ConfigMap, error) {
	for _, cm := range f.configMaps {
		if cm.Name == name {
			return cm, nil
		}
	}
	return nil, &notFoundError{name: name}
}

type notFoundError struct {
	name string
}

func (e *notFoundError) Error() string {
	return e.name + " not found"
}

func makeBackendTLSPolicy(namespace, name, serviceName, hostname string, createdAt time.Time) *gatewayv1.BackendTLSPolicy {
	return &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              name,
			CreationTimestamp: metav1.NewTime(createdAt),
		},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{
					LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
						Group: "",
						Kind:  "Service",
						Name:  gatewayv1.ObjectName(serviceName),
					},
				},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				Hostname: gatewayv1.PreciseHostname(hostname),
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{
						Group: "",
						Kind:  "ConfigMap",
						Name:  "ca-bundle",
					},
				},
			},
		},
	}
}

func makeBackendTLSPolicyWithSection(namespace, name, serviceName, sectionName, hostname string, createdAt time.Time) *gatewayv1.BackendTLSPolicy {
	policy := makeBackendTLSPolicy(namespace, name, serviceName, hostname, createdAt)
	section := gatewayv1.SectionName(sectionName)
	policy.Spec.TargetRefs[0].SectionName = &section
	return policy
}

func TestLookupBackendTLSPolicy(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name             string
		policies         []*gatewayv1.BackendTLSPolicy
		serviceNamespace string
		serviceName      string
		portName         string
		wantPolicyName   string
	}{
		{
			name:             "no policies",
			policies:         nil,
			serviceNamespace: "default",
			serviceName:      "my-svc",
			wantPolicyName:   "",
		},
		{
			name: "single matching policy",
			policies: []*gatewayv1.BackendTLSPolicy{
				makeBackendTLSPolicy("default", "policy-1", "my-svc", "my-svc.example.com", now),
			},
			serviceNamespace: "default",
			serviceName:      "my-svc",
			wantPolicyName:   "policy-1",
		},
		{
			name: "policy targets different service",
			policies: []*gatewayv1.BackendTLSPolicy{
				makeBackendTLSPolicy("default", "policy-1", "other-svc", "other.example.com", now),
			},
			serviceNamespace: "default",
			serviceName:      "my-svc",
			wantPolicyName:   "",
		},
		{
			name: "policy in different namespace",
			policies: []*gatewayv1.BackendTLSPolicy{
				makeBackendTLSPolicy("other-ns", "policy-1", "my-svc", "my-svc.example.com", now),
			},
			serviceNamespace: "default",
			serviceName:      "my-svc",
			wantPolicyName:   "",
		},
		{
			name: "multiple matching policies - oldest wins",
			policies: []*gatewayv1.BackendTLSPolicy{
				makeBackendTLSPolicy("default", "policy-newer", "my-svc", "my-svc.example.com", now.Add(time.Minute)),
				makeBackendTLSPolicy("default", "policy-older", "my-svc", "my-svc.example.com", now),
			},
			serviceNamespace: "default",
			serviceName:      "my-svc",
			wantPolicyName:   "policy-older",
		},
		{
			name: "multiple matching policies - same timestamp, alphabetical wins",
			policies: []*gatewayv1.BackendTLSPolicy{
				makeBackendTLSPolicy("default", "policy-b", "my-svc", "my-svc.example.com", now),
				makeBackendTLSPolicy("default", "policy-a", "my-svc", "my-svc.example.com", now),
			},
			serviceNamespace: "default",
			serviceName:      "my-svc",
			wantPolicyName:   "policy-a",
		},
		{
			name: "policy targets non-Service kind is ignored",
			policies: []*gatewayv1.BackendTLSPolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-1"},
					Spec: gatewayv1.BackendTLSPolicySpec{
						TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
							{
								LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
									Group: "",
									Kind:  "ServiceImport",
									Name:  "my-svc",
								},
							},
						},
					},
				},
			},
			serviceNamespace: "default",
			serviceName:      "my-svc",
			wantPolicyName:   "",
		},
		{
			name: "sectionName match takes precedence over whole-service policy",
			policies: []*gatewayv1.BackendTLSPolicy{
				makeBackendTLSPolicy("default", "policy-whole", "my-svc", "whole.example.com", now),
				makeBackendTLSPolicyWithSection("default", "policy-https", "my-svc", "https", "https.example.com", now.Add(time.Minute)),
			},
			serviceNamespace: "default",
			serviceName:      "my-svc",
			portName:         "https",
			wantPolicyName:   "policy-https",
		},
		{
			name: "sectionName for a different port is ignored",
			policies: []*gatewayv1.BackendTLSPolicy{
				makeBackendTLSPolicyWithSection("default", "policy-https", "my-svc", "https", "https.example.com", now),
			},
			serviceNamespace: "default",
			serviceName:      "my-svc",
			portName:         "http",
			wantPolicyName:   "",
		},
		{
			name: "whole-service policy applies when sectionName does not match",
			policies: []*gatewayv1.BackendTLSPolicy{
				makeBackendTLSPolicy("default", "policy-whole", "my-svc", "whole.example.com", now),
				makeBackendTLSPolicyWithSection("default", "policy-https", "my-svc", "https", "https.example.com", now),
			},
			serviceNamespace: "default",
			serviceName:      "my-svc",
			portName:         "http",
			wantPolicyName:   "policy-whole",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Controller{
				backendTLSPolicyLister: &fakeBackendTLSPolicyLister{policies: tt.policies},
			}
			got := c.lookupBackendTLSPolicy(tt.serviceNamespace, tt.serviceName, tt.portName)
			if tt.wantPolicyName == "" {
				if got != nil {
					t.Errorf("expected nil, got %s/%s", got.Namespace, got.Name)
				}
			} else {
				if got == nil {
					t.Fatalf("expected policy %q, got nil", tt.wantPolicyName)
				}
				if got.Name != tt.wantPolicyName {
					t.Errorf("expected policy %q, got %q", tt.wantPolicyName, got.Name)
				}
			}
		})
	}
}

func TestBuildUpstreamTLSContext(t *testing.T) {
	tests := []struct {
		name       string
		policy     *gatewayv1.BackendTLSPolicy
		configMaps []*corev1.ConfigMap
		wantErr    bool
	}{
		{
			name:   "valid ConfigMap with ca.crt in Data",
			policy: makeBackendTLSPolicy("default", "policy-1", "my-svc", "my-svc.example.com", time.Now()),
			configMaps: []*corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ca-bundle"},
					Data:       map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"},
				},
			},
			wantErr: false,
		},
		{
			name:   "valid ConfigMap with ca.crt in BinaryData",
			policy: makeBackendTLSPolicy("default", "policy-1", "my-svc", "my-svc.example.com", time.Now()),
			configMaps: []*corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ca-bundle"},
					BinaryData: map[string][]byte{"ca.crt": []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n")},
				},
			},
			wantErr: false,
		},
		{
			name:   "ConfigMap missing ca.crt key",
			policy: makeBackendTLSPolicy("default", "policy-1", "my-svc", "my-svc.example.com", time.Now()),
			configMaps: []*corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ca-bundle"},
					Data:       map[string]string{"other-key": "value"},
				},
			},
			wantErr: true,
		},
		{
			name:   "ConfigMap with empty ca.crt in Data",
			policy: makeBackendTLSPolicy("default", "policy-1", "my-svc", "my-svc.example.com", time.Now()),
			configMaps: []*corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ca-bundle"},
					Data:       map[string]string{"ca.crt": ""},
				},
			},
			wantErr: true,
		},
		{
			name:   "ConfigMap with whitespace-only ca.crt in Data",
			policy: makeBackendTLSPolicy("default", "policy-1", "my-svc", "my-svc.example.com", time.Now()),
			configMaps: []*corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ca-bundle"},
					Data:       map[string]string{"ca.crt": "  \n"},
				},
			},
			wantErr: true,
		},
		{
			name:   "ConfigMap with empty ca.crt in BinaryData",
			policy: makeBackendTLSPolicy("default", "policy-1", "my-svc", "my-svc.example.com", time.Now()),
			configMaps: []*corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ca-bundle"},
					BinaryData: map[string][]byte{"ca.crt": {}},
				},
			},
			wantErr: true,
		},
		{
			name:       "ConfigMap does not exist",
			policy:     makeBackendTLSPolicy("default", "policy-1", "my-svc", "my-svc.example.com", time.Now()),
			configMaps: nil,
			wantErr:    true,
		},
		{
			name: "empty CACertificateRefs",
			policy: &gatewayv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-1"},
				Spec: gatewayv1.BackendTLSPolicySpec{
					Validation: gatewayv1.BackendTLSPolicyValidation{
						Hostname:          "my-svc.example.com",
						CACertificateRefs: []gatewayv1.LocalObjectReference{},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "unsupported CACertificateRef kind",
			policy: &gatewayv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-1"},
				Spec: gatewayv1.BackendTLSPolicySpec{
					Validation: gatewayv1.BackendTLSPolicyValidation{
						Hostname: "my-svc.example.com",
						CACertificateRefs: []gatewayv1.LocalObjectReference{
							{Group: "", Kind: "Secret", Name: "my-secret"},
						},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Controller{
				configMapLister: &fakeConfigMapLister{configMaps: tt.configMaps},
			}
			got, err := c.buildUpstreamTLSContext(tt.policy)
			if (err != nil) != tt.wantErr {
				t.Errorf("buildUpstreamTLSContext() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got == nil {
				t.Error("buildUpstreamTLSContext() returned nil without error")
			}
			if !tt.wantErr && got != nil {
				upstreamTLS := &tlsv3.UpstreamTlsContext{}
				if err := got.UnmarshalTo(upstreamTLS); err != nil {
					t.Fatalf("UnmarshalTo: %v", err)
				}
				hostname := string(tt.policy.Spec.Validation.Hostname)
				if upstreamTLS.Sni != hostname {
					t.Errorf("SNI = %q, want %q", upstreamTLS.Sni, hostname)
				}
				sans := upstreamTLS.GetCommonTlsContext().GetValidationContext().GetMatchTypedSubjectAltNames()
				if len(sans) != 1 {
					t.Fatalf("MatchTypedSubjectAltNames len = %d, want 1", len(sans))
				}
				if sans[0].GetSanType() != tlsv3.SubjectAltNameMatcher_DNS {
					t.Errorf("SAN type = %v, want DNS", sans[0].GetSanType())
				}
				if sans[0].GetMatcher().GetExact() != hostname {
					t.Errorf("SAN exact = %q, want %q", sans[0].GetMatcher().GetExact(), hostname)
				}
			}
		})
	}
}

func TestLoadCACertificateRefEmpty(t *testing.T) {
	c := &Controller{
		configMapLister: &fakeConfigMapLister{
			configMaps: []*corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ca-bundle"},
					Data:       map[string]string{"ca.crt": ""},
				},
			},
		},
	}
	policy := makeBackendTLSPolicy("default", "policy-1", "my-svc", "my-svc.example.com", time.Now())
	_, err := c.loadCACertificateRef(policy, policy.Spec.Validation.CACertificateRefs[0])
	if err == nil {
		t.Fatal("expected error for empty ca.crt")
	}
	tlsErr, ok := err.(*backendTLSError)
	if !ok {
		t.Fatalf("error type = %T, want *backendTLSError", err)
	}
	if tlsErr.reason != gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef {
		t.Errorf("reason = %q, want %q", tlsErr.reason, gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef)
	}
}

func TestRewriteRoutesForInvalidBackendTLS(t *testing.T) {
	routes := []*routev3.Route{
		{
			Name: "keep",
			Action: &routev3.Route_Route{
				Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: "good-cluster"},
				},
			},
		},
		{
			Name: "rewrite",
			Action: &routev3.Route_Route{
				Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: "bad-cluster"},
				},
			},
		},
	}
	rewriteRoutesForInvalidBackendTLS(routes, map[string]struct{}{"bad-cluster": {}})

	if _, ok := routes[0].Action.(*routev3.Route_Route); !ok {
		t.Errorf("route %q action was rewritten, want cluster route", routes[0].Name)
	}
	direct, ok := routes[1].Action.(*routev3.Route_DirectResponse)
	if !ok {
		t.Fatalf("route %q action type = %T, want DirectResponse", routes[1].Name, routes[1].Action)
	}
	if direct.DirectResponse.GetStatus() != 503 {
		t.Errorf("DirectResponse status = %d, want 503", direct.DirectResponse.GetStatus())
	}
}

func TestBackendTLSPolicyConditionsConflicted(t *testing.T) {
	conds := backendTLSPolicyConditions(1, true, nil, nil)
	var accepted *metav1.Condition
	for i := range conds {
		if conds[i].Type == string(gatewayv1.PolicyConditionAccepted) {
			accepted = &conds[i]
		}
	}
	if accepted == nil {
		t.Fatal("Accepted condition is missing")
	}
	if accepted.Status != metav1.ConditionFalse {
		t.Errorf("Accepted.Status = %s, want False", accepted.Status)
	}
	if accepted.Reason != string(gatewayv1.PolicyReasonConflicted) {
		t.Errorf("Accepted.Reason = %q, want %q", accepted.Reason, gatewayv1.PolicyReasonConflicted)
	}
}

func TestBackendTLSPolicyConditionsInvalidCA(t *testing.T) {
	caErr := &backendTLSError{
		reason:  gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef,
		message: "ConfigMap default/ca-bundle does not contain key 'ca.crt'",
	}
	conds := backendTLSPolicyConditions(1, false, caErr, nil)
	var accepted, resolved *metav1.Condition
	for i := range conds {
		switch conds[i].Type {
		case string(gatewayv1.PolicyConditionAccepted):
			accepted = &conds[i]
		case string(gatewayv1.BackendTLSPolicyConditionResolvedRefs):
			resolved = &conds[i]
		}
	}
	if accepted == nil || resolved == nil {
		t.Fatal("Accepted or ResolvedRefs condition is missing")
	}
	if accepted.Status != metav1.ConditionFalse {
		t.Errorf("Accepted.Status = %s, want False", accepted.Status)
	}
	if accepted.Reason != string(gatewayv1.BackendTLSPolicyReasonNoValidCACertificate) {
		t.Errorf("Accepted.Reason = %q, want %q", accepted.Reason, gatewayv1.BackendTLSPolicyReasonNoValidCACertificate)
	}
	if resolved.Status != metav1.ConditionFalse {
		t.Errorf("ResolvedRefs.Status = %s, want False", resolved.Status)
	}
	if resolved.Reason != string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef) {
		t.Errorf("ResolvedRefs.Reason = %q, want %q", resolved.Reason, gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef)
	}
}

func TestRouteReferencesService(t *testing.T) {
	tests := []struct {
		name             string
		route            *gatewayv1.HTTPRoute
		serviceName      string
		serviceNamespace string
		want             bool
	}{
		{
			name: "route references the service",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{
							BackendRefs: []gatewayv1.HTTPBackendRef{
								{BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "my-svc",
										Port: ptrTo(gatewayv1.PortNumber(8080)),
									},
								}},
							},
						},
					},
				},
			},
			serviceName:      "my-svc",
			serviceNamespace: "default",
			want:             true,
		},
		{
			name: "route references a different service",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{
							BackendRefs: []gatewayv1.HTTPBackendRef{
								{BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "other-svc",
										Port: ptrTo(gatewayv1.PortNumber(8080)),
									},
								}},
							},
						},
					},
				},
			},
			serviceName:      "my-svc",
			serviceNamespace: "default",
			want:             false,
		},
		{
			name: "route in different namespace with explicit namespace ref",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "route-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{
							BackendRefs: []gatewayv1.HTTPBackendRef{
								{BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name:      "my-svc",
										Namespace: ptrTo(gatewayv1.Namespace("default")),
										Port:      ptrTo(gatewayv1.PortNumber(8080)),
									},
								}},
							},
						},
					},
				},
			},
			serviceName:      "my-svc",
			serviceNamespace: "default",
			want:             true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := routeReferencesService(tt.route, tt.serviceName, tt.serviceNamespace)
			if got != tt.want {
				t.Errorf("routeReferencesService() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBackendTLSPolicySpecChanged(t *testing.T) {
	base := makeBackendTLSPolicy("default", "policy-1", "my-svc", "my-svc.example.com", time.Now())
	base.Generation = 1

	statusOnly := base.DeepCopy()
	statusOnly.Status.Ancestors = []gatewayv1.PolicyAncestorStatus{{}}

	specChanged := base.DeepCopy()
	specChanged.Generation = 2
	specChanged.Spec.Validation.Hostname = "other.example.com"

	if backendTLSPolicySpecChanged(base, statusOnly) {
		t.Error("status-only update must not count as a spec change")
	}
	if !backendTLSPolicySpecChanged(base, specChanged) {
		t.Error("generation change must count as a spec change")
	}
	if !backendTLSPolicySpecChanged(nil, base) {
		t.Error("untyped or missing old object must count as a spec change")
	}
}

func ptrTo[T any](v T) *T {
	return &v
}

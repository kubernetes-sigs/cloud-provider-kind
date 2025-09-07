package gateway

import (
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	gatewayv1beta1listers "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1beta1"
)

// fakeReferenceGrantLister is a mock implementation of ReferenceGrantLister for testing.
type fakeReferenceGrantLister struct {
	grants []*gatewayv1beta1.ReferenceGrant
	err    error
}

// newFakeReferenceGrantLister creates a new fakeReferenceGrantLister.
func newFakeReferenceGrantLister(grants []*gatewayv1beta1.ReferenceGrant, err error) gatewayv1beta1listers.ReferenceGrantLister {
	return &fakeReferenceGrantLister{grants: grants, err: err}
}

// List returns the stored grants or an error.
func (f *fakeReferenceGrantLister) List(selector labels.Selector) ([]*gatewayv1beta1.ReferenceGrant, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.grants, nil
}

// Get returns the grant with the given name or an error.
func (f *fakeReferenceGrantLister) Get(name string) (*gatewayv1beta1.ReferenceGrant, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, grant := range f.grants {
		if grant.Name == name {
			return grant, nil
		}
	}
	return nil, errors.New("not found")
}

// ReferenceGrants returns a lister for a specific namespace.
func (f *fakeReferenceGrantLister) ReferenceGrants(namespace string) gatewayv1beta1listers.ReferenceGrantNamespaceLister {
	// For testing purposes, we can return the lister itself, as we don't use the namespace in the mock List.
	// A more sophisticated mock could filter by namespace here.
	return f
}

func TestIsCrossNamespaceRefAllowed(t *testing.T) {
	serviceKind := gatewayv1beta1.Kind("Service")
	httpRouteKind := gatewayv1beta1.Kind("HTTPRoute")
	gatewayGroup := gatewayv1beta1.Group("gateway.networking.k8s.io")
	coreGroup := gatewayv1beta1.Group("")

	specificServiceName := gatewayv1beta1.ObjectName("specific-service")

	testCases := []struct {
		name        string
		from        gatewayv1beta1.ReferenceGrantFrom
		to          gatewayv1beta1.ReferenceGrantTo
		toNamespace string
		grants      []*gatewayv1beta1.ReferenceGrant
		listerError error
		expected    bool
	}{
		{
			name: "allowed by grant",
			from: gatewayv1beta1.ReferenceGrantFrom{
				Group:     gatewayGroup,
				Kind:      httpRouteKind,
				Namespace: "default",
			},
			to: gatewayv1beta1.ReferenceGrantTo{
				Group: coreGroup,
				Kind:  serviceKind,
			},
			toNamespace: "backend-ns",
			grants: []*gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "backend-ns"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: gatewayGroup, Kind: httpRouteKind, Namespace: "default"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: coreGroup, Kind: serviceKind},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "allowed by grant with specific resource name",
			from: gatewayv1beta1.ReferenceGrantFrom{
				Group:     gatewayGroup,
				Kind:      httpRouteKind,
				Namespace: "default",
			},
			to: gatewayv1beta1.ReferenceGrantTo{
				Group: coreGroup,
				Kind:  serviceKind,
				Name:  &specificServiceName,
			},
			toNamespace: "backend-ns",
			grants: []*gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "backend-ns"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: gatewayGroup, Kind: httpRouteKind, Namespace: "default"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: coreGroup, Kind: serviceKind, Name: &specificServiceName},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "denied because from namespace does not match",
			from: gatewayv1beta1.ReferenceGrantFrom{
				Group:     gatewayGroup,
				Kind:      httpRouteKind,
				Namespace: "another-ns", // Mismatch
			},
			to: gatewayv1beta1.ReferenceGrantTo{
				Group: coreGroup,
				Kind:  serviceKind,
			},
			toNamespace: "backend-ns",
			grants: []*gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "backend-ns"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: gatewayGroup, Kind: httpRouteKind, Namespace: "default"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: coreGroup, Kind: serviceKind},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "denied because to kind does not match",
			from: gatewayv1beta1.ReferenceGrantFrom{
				Group:     gatewayGroup,
				Kind:      httpRouteKind,
				Namespace: "default",
			},
			to: gatewayv1beta1.ReferenceGrantTo{
				Group: coreGroup,
				Kind:  "Secret", // Mismatch
			},
			toNamespace: "backend-ns",
			grants: []*gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "backend-ns"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: gatewayGroup, Kind: httpRouteKind, Namespace: "default"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: coreGroup, Kind: serviceKind},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "denied because specific resource name does not match",
			from: gatewayv1beta1.ReferenceGrantFrom{
				Group:     gatewayGroup,
				Kind:      httpRouteKind,
				Namespace: "default",
			},
			to: gatewayv1beta1.ReferenceGrantTo{
				Group: coreGroup,
				Kind:  serviceKind,
				Name:  func() *gatewayv1beta1.ObjectName { s := gatewayv1beta1.ObjectName("other-service"); return &s }(),
			},
			toNamespace: "backend-ns",
			grants: []*gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Namespace: "backend-ns"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: gatewayGroup, Kind: httpRouteKind, Namespace: "default"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: coreGroup, Kind: serviceKind, Name: &specificServiceName},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "no grants in namespace",
			from: gatewayv1beta1.ReferenceGrantFrom{
				Group:     gatewayGroup,
				Kind:      httpRouteKind,
				Namespace: "default",
			},
			to: gatewayv1beta1.ReferenceGrantTo{
				Group: coreGroup,
				Kind:  serviceKind,
			},
			toNamespace: "backend-ns",
			grants:      []*gatewayv1beta1.ReferenceGrant{},
			expected:    false,
		},
		{
			name: "lister returns error",
			from: gatewayv1beta1.ReferenceGrantFrom{
				Group:     gatewayGroup,
				Kind:      httpRouteKind,
				Namespace: "default",
			},
			to: gatewayv1beta1.ReferenceGrantTo{
				Group: coreGroup,
				Kind:  serviceKind,
			},
			toNamespace: "backend-ns",
			grants:      nil,
			listerError: errors.New("failed to list"),
			expected:    false,
		},
		{
			name: "multiple grants, one allows",
			from: gatewayv1beta1.ReferenceGrantFrom{
				Group:     gatewayGroup,
				Kind:      httpRouteKind,
				Namespace: "default",
			},
			to: gatewayv1beta1.ReferenceGrantTo{
				Group: coreGroup,
				Kind:  serviceKind,
			},
			toNamespace: "backend-ns",
			grants: []*gatewayv1beta1.ReferenceGrant{
				{ // This one doesn't match
					ObjectMeta: metav1.ObjectMeta{Namespace: "backend-ns"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: gatewayGroup, Kind: "OtherKind", Namespace: "default"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: coreGroup, Kind: serviceKind},
						},
					},
				},
				{ // This one matches
					ObjectMeta: metav1.ObjectMeta{Namespace: "backend-ns"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: gatewayGroup, Kind: httpRouteKind, Namespace: "default"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: coreGroup, Kind: serviceKind},
						},
					},
				},
			},
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			lister := newFakeReferenceGrantLister(tc.grants, tc.listerError)
			result := isCrossNamespaceRefAllowed(tc.from, tc.to, tc.toNamespace, lister)
			if result != tc.expected {
				t.Errorf("expected %v, but got %v", tc.expected, result)
			}
		})
	}
}

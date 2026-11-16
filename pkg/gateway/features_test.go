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

	"sigs.k8s.io/gateway-api/pkg/features"
)

func TestSupportedFeaturesAreSorted(t *testing.T) {
	for i := 1; i < len(supportedFeatures); i++ {
		if supportedFeatures[i].Name < supportedFeatures[i-1].Name {
			t.Errorf("supportedFeatures is not sorted: %q appears after %q",
				supportedFeatures[i].Name, supportedFeatures[i-1].Name)
		}
	}
}

func TestSupportedFeaturesContainsCore(t *testing.T) {
	required := []features.FeatureName{
		features.SupportGateway,
		features.SupportGRPCRoute,
		features.SupportHTTPRoute,
		features.SupportReferenceGrant,
	}
	featureSet := make(map[string]bool, len(supportedFeatures))
	for _, f := range supportedFeatures {
		featureSet[string(f.Name)] = true
	}
	for _, name := range required {
		if !featureSet[string(name)] {
			t.Errorf("core feature %q is missing from supportedFeatures", name)
		}
	}
}

func TestBuildSupportedFeaturesSort(t *testing.T) {
	got := buildSupportedFeatures(
		features.SupportHTTPRouteQueryParamMatching, // "HTTPRouteQueryParamMatching"
		features.SupportGateway,                     // "Gateway"
		features.SupportHTTPRoute,                   // "HTTPRoute"
	)
	want := []features.FeatureName{
		features.SupportGateway,
		features.SupportHTTPRoute,
		features.SupportHTTPRouteQueryParamMatching,
	}
	for i, name := range want {
		if string(got[i].Name) != string(name) {
			t.Errorf("index %d: got %q, want %q", i, got[i].Name, name)
		}
	}
}

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
	"sort"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/features"

	"sigs.k8s.io/cloud-provider-kind/pkg/config"
)

// standardFeatureNames are the Gateway API features supported by
// cloud-provider-kind in every release channel.
var standardFeatureNames = []features.FeatureName{
	// Core
	features.SupportGateway,
	features.SupportGRPCRoute,
	features.SupportHTTPRoute,
	features.SupportReferenceGrant,
	// Extended
	features.SupportGatewayAddressEmpty,
	features.SupportGatewayPort8080,
	features.SupportGatewayStaticAddresses,
	features.SupportHTTPRoute303RedirectStatusCode,
	features.SupportHTTPRoute307RedirectStatusCode,
	features.SupportHTTPRoute308RedirectStatusCode,
	features.SupportHTTPRouteBackendProtocolWebSocket,
	features.SupportHTTPRouteParentRefPort,
	features.SupportHTTPRouteQueryParamMatching,
}

// experimentalFeatureNames are only advertised when the experimental CRDs are
// installed, since the corresponding API fields do not exist otherwise.
// GEP-1494 (HTTP Auth) is still experimental, so the feature names are not yet
// published as constants by sigs.k8s.io/gateway-api/pkg/features.
var experimentalFeatureNames = []features.FeatureName{
	"HTTPRouteExternalAuth",
	"HTTPRouteExternalAuthForwardBody",
	"HTTPRouteExternalAuthGRPC",
	"HTTPRouteExternalAuthHTTP",
}

// supportedFeatures is the sorted list of Gateway API features that
// cloud-provider-kind supports, as defined by GEP-2162.
var supportedFeatures = buildSupportedFeatures(standardFeatureNames...)

// supportedFeaturesForChannel returns the features to advertise on the
// GatewayClass status for the Gateway API release channel in use.
func supportedFeaturesForChannel(channel config.GatewayReleaseChannel) []gatewayv1.SupportedFeature {
	if channel != config.Experimental {
		return supportedFeatures
	}
	names := make([]features.FeatureName, 0, len(standardFeatureNames)+len(experimentalFeatureNames))
	names = append(names, standardFeatureNames...)
	names = append(names, experimentalFeatureNames...)
	return buildSupportedFeatures(names...)
}

// The spec requires features to be sorted in ascending alphabetical order.
func buildSupportedFeatures(names ...features.FeatureName) []gatewayv1.SupportedFeature {
	result := make([]gatewayv1.SupportedFeature, len(names))
	for i, name := range names {
		result[i] = gatewayv1.SupportedFeature{Name: gatewayv1.FeatureName(name)}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

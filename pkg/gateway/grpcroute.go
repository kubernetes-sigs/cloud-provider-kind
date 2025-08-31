package gateway

import (
	"fmt"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// translateGRPCRouteToEnvoyVirtualHost takes a GRPCRoute and adds its rules as Envoy routes
// to the provided VirtualHost.
func translateGRPCRouteToEnvoyVirtualHost(grpcRoute *gatewayv1.GRPCRoute, virtualHost *routev3.VirtualHost) error {
	for ruleIndex, rule := range grpcRoute.Spec.Rules {
		if len(rule.BackendRefs) == 0 {
			klog.Warningf("GRPCRoute %s/%s rule %d has no backendRefs, skipping", grpcRoute.Namespace, grpcRoute.Name, ruleIndex)
			continue
		}

		routeAction, err := buildGRPCRouteAction(grpcRoute.Namespace, rule.BackendRefs)
		if err != nil {
			klog.Errorf("Error building route action for GRPCRoute %s/%s rule %d: %v", grpcRoute.Namespace, grpcRoute.Name, ruleIndex, err)
			continue
		}

		// GRPCRoute requires at least one match. If empty, the rule is ignored.
		if len(rule.Matches) == 0 {
			klog.Warningf("GRPCRoute %s/%s rule %d has no matches, skipping", grpcRoute.Namespace, grpcRoute.Name, ruleIndex)
			continue
		}

		for matchIndex, match := range rule.Matches {
			routeMatch, err := translateGRPCRouteMatch(match)
			if err != nil {
				klog.Errorf("Failed to translate match %d for GRPCRoute %s/%s: %v", matchIndex, grpcRoute.Namespace, grpcRoute.Name, err)
				continue
			}

			envoyRoute := &routev3.Route{
				Name:   fmt.Sprintf("%s-%s-rule%d-match%d", grpcRoute.Namespace, grpcRoute.Name, ruleIndex, matchIndex),
				Match:  routeMatch,
				Action: routeAction,
			}
			virtualHost.Routes = append(virtualHost.Routes, envoyRoute)
		}
	}
	return nil
}

// buildGRPCRouteAction creates a RouteAction that supports weighted distribution of traffic
// across multiple backend services.
func buildGRPCRouteAction(namespace string, backendRefs []gatewayv1.GRPCBackendRef) (*routev3.Route_Route, error) {
	weightedClusters := &routev3.WeightedCluster{}

	for _, backendRef := range backendRefs {
		if backendRef.Weight != nil && *backendRef.Weight < 0 {
			return nil, fmt.Errorf("backend weight must be non-negative")
		}
		weight := int32(1)
		if backendRef.Weight != nil {
			weight = *backendRef.Weight
		}
		if weight == 0 {
			continue
		}

		clusterName, err := backendRefToClusterName(namespace, backendRef.BackendRef)
		if err != nil {
			return nil, err
		}

		weightedClusters.Clusters = append(weightedClusters.Clusters, &routev3.WeightedCluster_ClusterWeight{
			Name:   clusterName,
			Weight: &wrapperspb.UInt32Value{Value: uint32(weight)},
		})
	}

	if len(weightedClusters.Clusters) == 0 {
		return &routev3.Route_Route{
			Route: &routev3.RouteAction{
				ClusterNotFoundResponseCode: routev3.RouteAction_ClusterNotFoundResponseCode(503),
			},
		}, nil
	}

	if len(weightedClusters.Clusters) == 1 {
		return &routev3.Route_Route{
			Route: &routev3.RouteAction{
				ClusterSpecifier: &routev3.RouteAction_Cluster{
					Cluster: weightedClusters.Clusters[0].Name,
				},
			},
		}, nil
	}

	return &routev3.Route_Route{
		Route: &routev3.RouteAction{
			ClusterSpecifier: &routev3.RouteAction_WeightedClusters{
				WeightedClusters: weightedClusters,
			},
		},
	}, nil
}

// translateGRPCRouteMatch translates a Gateway API GRPCRouteMatch into an Envoy RouteMatch.
func translateGRPCRouteMatch(match gatewayv1.GRPCRouteMatch) (*routev3.RouteMatch, error) {
	routeMatch := &routev3.RouteMatch{
		// All gRPC requests are HTTP/2 POST requests.
		Headers: []*routev3.HeaderMatcher{
			{
				Name:                 ":method",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_ExactMatch{ExactMatch: "POST"},
			},
			{
				Name: "content-type",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
					StringMatch: &matcherv3.StringMatcher{
						MatchPattern: &matcherv3.StringMatcher_Prefix{Prefix: "application/grpc"},
						IgnoreCase:   true,
					},
				},
			},
		},
	}

	if match.Method != nil {
		if match.Method.Service == nil || match.Method.Method == nil {
			return nil, fmt.Errorf("GRPCMethodMatch requires both service and method to be set")
		}
		path := fmt.Sprintf("/%s/%s", *match.Method.Service, *match.Method.Method)
		matchType := gatewayv1.GRPCMethodMatchExact
		if match.Method.Type != nil {
			matchType = *match.Method.Type
		}

		pathMatcher := &routev3.HeaderMatcher{Name: ":path"}
		switch matchType {
		case gatewayv1.GRPCMethodMatchExact:
			pathMatcher.HeaderMatchSpecifier = &routev3.HeaderMatcher_ExactMatch{ExactMatch: path}
		case gatewayv1.GRPCMethodMatchRegularExpression:
			pathMatcher.HeaderMatchSpecifier = &routev3.HeaderMatcher_SafeRegexMatch{
				SafeRegexMatch: &matcherv3.RegexMatcher{
					EngineType: &matcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &matcherv3.RegexMatcher_GoogleRE2{}},
					Regex:      path,
				},
			}
		default:
			return nil, fmt.Errorf("unsupported gRPC method match type: %s", matchType)
		}
		routeMatch.Headers = append(routeMatch.Headers, pathMatcher)
	}

	for _, headerMatch := range match.Headers {
		headerMatcher := &routev3.HeaderMatcher{
			Name: string(headerMatch.Name),
		}
		matchType := gatewayv1.GRPCHeaderMatchExact
		if headerMatch.Type != nil {
			matchType = *headerMatch.Type
		}

		switch matchType {
		case gatewayv1.GRPCHeaderMatchExact:
			headerMatcher.HeaderMatchSpecifier = &routev3.HeaderMatcher_StringMatch{
				StringMatch: &matcherv3.StringMatcher{
					MatchPattern: &matcherv3.StringMatcher_Exact{Exact: headerMatch.Value},
				},
			}
		case gatewayv1.GRPCHeaderMatchRegularExpression:
			headerMatcher.HeaderMatchSpecifier = &routev3.HeaderMatcher_SafeRegexMatch{
				SafeRegexMatch: &matcherv3.RegexMatcher{
					EngineType: &matcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &matcherv3.RegexMatcher_GoogleRE2{}},
					Regex:      headerMatch.Value,
				},
			}
		default:
			return nil, fmt.Errorf("unsupported header match type: %s", matchType)
		}
		routeMatch.Headers = append(routeMatch.Headers, headerMatcher)
	}

	return routeMatch, nil
}

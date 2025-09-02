package gateway

import (
	"fmt"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// translateHTTPRouteToEnvoyVirtualHost takes an HTTPRoute and adds its rules as Envoy routes
// to the provided VirtualHost.
func translateHTTPRouteToEnvoyVirtualHost(httpRoute *gatewayv1.HTTPRoute, virtualHost *routev3.VirtualHost) error {
	for ruleIndex, rule := range httpRoute.Spec.Rules {
		if len(rule.BackendRefs) == 0 {
			klog.Warningf("HTTPRoute %s/%s rule %d has no backendRefs, skipping", httpRoute.Namespace, httpRoute.Name, ruleIndex)
			continue
		}

		routeAction, err := buildHTTPRouteAction(httpRoute.Namespace, rule.BackendRefs)
		if err != nil {
			return fmt.Errorf("can not build route action for HTTPRoute %s/%s rule %d: %v", httpRoute.Namespace, httpRoute.Name, ruleIndex, err)
		}

		if len(rule.Matches) == 0 {
			envoyRoute := &routev3.Route{
				Name:   fmt.Sprintf("%s-%s-rule%d-matchall", httpRoute.Namespace, httpRoute.Name, ruleIndex),
				Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
				Action: routeAction,
			}
			virtualHost.Routes = append(virtualHost.Routes, envoyRoute)
			continue
		}

		for matchIndex, match := range rule.Matches {
			routeMatch, err := translateHTTPRouteMatch(match)
			if err != nil {
				return fmt.Errorf("can not translate match %d for HTTPRoute %s/%s: %v", matchIndex, httpRoute.Namespace, httpRoute.Name, err)
			}

			envoyRoute := &routev3.Route{
				Name:   fmt.Sprintf("%s-%s-rule%d-match%d", httpRoute.Namespace, httpRoute.Name, ruleIndex, matchIndex),
				Match:  routeMatch,
				Action: routeAction,
			}
			virtualHost.Routes = append(virtualHost.Routes, envoyRoute)
		}
	}
	return nil
}

// buildHTTPRouteAction creates a RouteAction that supports weighted distribution of traffic
// across multiple backend services.
func buildHTTPRouteAction(namespace string, backendRefs []gatewayv1.HTTPBackendRef) (*routev3.Route_Route, error) {
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

// translateHTTPRouteMatch translates a Gateway API HTTPRouteMatch into an Envoy RouteMatch.
func translateHTTPRouteMatch(match gatewayv1.HTTPRouteMatch) (*routev3.RouteMatch, error) {
	routeMatch := &routev3.RouteMatch{}

	if match.Path != nil {
		pathType := gatewayv1.PathMatchPathPrefix
		if match.Path.Type != nil {
			pathType = *match.Path.Type
		}
		if match.Path.Value == nil {
			return nil, fmt.Errorf("path match value cannot be nil")
		}
		pathValue := *match.Path.Value

		switch pathType {
		case gatewayv1.PathMatchExact:
			routeMatch.PathSpecifier = &routev3.RouteMatch_Path{Path: pathValue}
		case gatewayv1.PathMatchPathPrefix:
			routeMatch.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: pathValue}
		case gatewayv1.PathMatchRegularExpression:
			routeMatch.PathSpecifier = &routev3.RouteMatch_SafeRegex{
				SafeRegex: &matcherv3.RegexMatcher{
					EngineType: &matcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &matcherv3.RegexMatcher_GoogleRE2{}},
					Regex:      pathValue,
				},
			}
		default:
			return nil, fmt.Errorf("unsupported path match type: %s", pathType)
		}
	} else {
		routeMatch.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/"}
	}

	for _, headerMatch := range match.Headers {
		headerMatcher := &routev3.HeaderMatcher{
			Name: string(headerMatch.Name),
		}
		matchType := gatewayv1.HeaderMatchExact
		if headerMatch.Type != nil {
			matchType = *headerMatch.Type
		}

		switch matchType {
		case gatewayv1.HeaderMatchExact:
			headerMatcher.HeaderMatchSpecifier = &routev3.HeaderMatcher_StringMatch{
				StringMatch: &matcherv3.StringMatcher{
					MatchPattern: &matcherv3.StringMatcher_Exact{Exact: headerMatch.Value},
				},
			}
		case gatewayv1.HeaderMatchRegularExpression:
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

	for _, queryMatch := range match.QueryParams {
		queryMatcher := &routev3.QueryParameterMatcher{
			Name: string(queryMatch.Name),
		}
		queryMatcher.QueryParameterMatchSpecifier = &routev3.QueryParameterMatcher_StringMatch{
			StringMatch: &matcherv3.StringMatcher{
				MatchPattern: &matcherv3.StringMatcher_Exact{Exact: queryMatch.Value},
			},
		}
		routeMatch.QueryParameters = append(routeMatch.QueryParameters, queryMatcher)
	}

	return routeMatch, nil
}

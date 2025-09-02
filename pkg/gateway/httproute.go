package gateway

import (
	"errors"
	"fmt"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// translateHTTPRouteToEnvoyRoutes returns Envoy routes, a list of *valid* BackendRefs, and a condition.
func translateHTTPRouteToEnvoyRoutes(httpRoute *gatewayv1.HTTPRoute, serviceLister corev1listers.ServiceLister) ([]*routev3.Route, []gatewayv1.BackendRef, metav1.Condition) {
	var envoyRoutes []*routev3.Route
	var validBackendRefs []gatewayv1.BackendRef
	isOverallSuccess := true
	var finalFailureMessage string
	var finalFailureReason gatewayv1.RouteConditionReason

	for ruleIndex, rule := range httpRoute.Spec.Rules {
		routeAction, backendRefs, err := buildHTTPRouteAction(httpRoute.Namespace, rule.BackendRefs, serviceLister)
		validBackendRefs = append(validBackendRefs, backendRefs...)

		buildRoutesForRule := func(match gatewayv1.HTTPRouteMatch, matchIndex int) (*routev3.Route, metav1.Condition) {
			routeMatch, matchCondition := translateHTTPRouteMatch(match, httpRoute.Generation)
			if matchCondition.Status == metav1.ConditionFalse {
				return nil, matchCondition
			}
			envoyRoute := &routev3.Route{
				Name:  fmt.Sprintf("%s-%s-rule%d-match%d", httpRoute.Namespace, httpRoute.Name, ruleIndex, matchIndex),
				Match: routeMatch,
			}
			var controllerErr *ControllerError
			if errors.As(err, &controllerErr) {
				if isOverallSuccess {
					isOverallSuccess = false
					finalFailureMessage = controllerErr.Message
					finalFailureReason = gatewayv1.RouteConditionReason(controllerErr.Reason)
				}
				envoyRoute.Action = &routev3.Route_DirectResponse{
					DirectResponse: &routev3.DirectResponseAction{Status: 500},
				}
			} else {
				envoyRoute.Action = &routev3.Route_Route{
					Route: routeAction,
				}
			}
			return envoyRoute, createSuccessCondition(httpRoute.Generation)
		}

		if len(rule.Matches) == 0 {
			envoyRoute, _ := buildRoutesForRule(gatewayv1.HTTPRouteMatch{}, 0)
			envoyRoutes = append(envoyRoutes, envoyRoute)
		} else {
			for matchIndex, match := range rule.Matches {
				envoyRoute, cond := buildRoutesForRule(match, matchIndex)
				if cond.Status == metav1.ConditionFalse {
					return nil, nil, cond
				}
				envoyRoutes = append(envoyRoutes, envoyRoute)
			}
		}
	}

	if isOverallSuccess {
		return envoyRoutes, validBackendRefs, createSuccessCondition(httpRoute.Generation)
	}
	return envoyRoutes, validBackendRefs, createFailureCondition(finalFailureReason, finalFailureMessage, httpRoute.Generation)
}

// buildHTTPRouteAction returns an action, a list of *valid* BackendRefs, and a structured error.
func buildHTTPRouteAction(namespace string, backendRefs []gatewayv1.HTTPBackendRef, serviceLister corev1listers.ServiceLister) (*routev3.RouteAction, []gatewayv1.BackendRef, error) {
	weightedClusters := &routev3.WeightedCluster{}
	var validBackendRefs []gatewayv1.BackendRef

	for _, httpBackendRef := range backendRefs {
		backendRef := httpBackendRef.BackendRef

		ns := namespace
		if backendRef.Namespace != nil {
			ns = string(*backendRef.Namespace)
		}
		if _, err := serviceLister.Services(ns).Get(string(backendRef.Name)); err != nil {
			return nil, nil, &ControllerError{
				Reason:  string(gatewayv1.RouteReasonBackendNotFound),
				Message: "backend not found",
			}
		}
		clusterName, err := backendRefToClusterName(namespace, backendRef)
		if err != nil {
			return nil, nil, err
		}

		weight := int32(1)
		if httpBackendRef.Weight != nil {
			weight = *httpBackendRef.Weight
		}
		if weight == 0 {
			continue
		}
		validBackendRefs = append(validBackendRefs, backendRef)
		weightedClusters.Clusters = append(weightedClusters.Clusters, &routev3.WeightedCluster_ClusterWeight{
			Name:   clusterName,
			Weight: &wrapperspb.UInt32Value{Value: uint32(weight)},
		})
	}

	if len(weightedClusters.Clusters) == 0 {
		return nil, nil, &ControllerError{Reason: string(gatewayv1.RouteReasonUnsupportedValue), Message: "no valid backends provided with a weight > 0"}
	}

	var action *routev3.RouteAction
	if len(weightedClusters.Clusters) == 1 {
		action = &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: weightedClusters.Clusters[0].Name}}
	} else {
		action = &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_WeightedClusters{WeightedClusters: weightedClusters}}
	}

	return action, validBackendRefs, nil
}

// translateHTTPRouteMatch translates a Gateway API HTTPRouteMatch into an Envoy RouteMatch.
// It returns the result and a condition indicating success or failure.
func translateHTTPRouteMatch(match gatewayv1.HTTPRouteMatch, generation int64) (*routev3.RouteMatch, metav1.Condition) {
	routeMatch := &routev3.RouteMatch{}

	if match.Path != nil {
		pathType := gatewayv1.PathMatchPathPrefix
		if match.Path.Type != nil {
			pathType = *match.Path.Type
		}
		if match.Path.Value == nil {
			msg := "path match value cannot be nil"
			return nil, createFailureCondition(gatewayv1.RouteReasonUnsupportedValue, msg, generation)
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
			msg := fmt.Sprintf("unsupported path match type: %s", pathType)
			return nil, createFailureCondition(gatewayv1.RouteReasonUnsupportedValue, msg, generation)
		}
	} else {
		// As per Gateway API spec, a nil path match defaults to matching everything.
		routeMatch.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/"}
	}

	// --- 2. Translate Header Matches ---
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
			msg := fmt.Sprintf("unsupported header match type: %s", matchType)
			return nil, createFailureCondition(gatewayv1.RouteReasonUnsupportedValue, msg, generation)
		}
		routeMatch.Headers = append(routeMatch.Headers, headerMatcher)
	}

	// --- 3. Translate Query Parameter Matches ---
	for _, queryMatch := range match.QueryParams {
		// Gateway API only supports "Exact" match for query parameters.
		queryMatcher := &routev3.QueryParameterMatcher{
			Name: string(queryMatch.Name),
			QueryParameterMatchSpecifier: &routev3.QueryParameterMatcher_StringMatch{
				StringMatch: &matcherv3.StringMatcher{
					MatchPattern: &matcherv3.StringMatcher_Exact{Exact: queryMatch.Value},
				},
			},
		}
		routeMatch.QueryParameters = append(routeMatch.QueryParameters, queryMatcher)
	}

	// If all translations were successful, return the final object and a success condition.
	return routeMatch, createSuccessCondition(generation)
}

func createSuccessCondition(generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               string(gatewayv1.RouteConditionResolvedRefs),
		Status:             metav1.ConditionTrue,
		Reason:             string(gatewayv1.RouteReasonResolvedRefs),
		Message:            "All references resolved",
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
	}
}

func createFailureCondition(reason gatewayv1.RouteConditionReason, message string, generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               string(gatewayv1.RouteConditionResolvedRefs),
		Status:             metav1.ConditionFalse,
		Reason:             string(reason),
		Message:            message,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
	}
}

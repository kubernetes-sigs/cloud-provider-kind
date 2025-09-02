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

// translateGRPCRouteToEnvoyRoutes translates a full GRPCRoute into a slice of Envoy Routes.
// It is a pure function that returns the result, a list of required cluster names, and a final
// status condition without mutating any arguments.
func translateGRPCRouteToEnvoyRoutes(
	grpcRoute *gatewayv1.GRPCRoute,
	serviceLister corev1listers.ServiceLister,
) ([]*routev3.Route, []gatewayv1.BackendRef, metav1.Condition) {

	var envoyRoutes []*routev3.Route
	var validBackendRefs []gatewayv1.BackendRef
	isOverallSuccess := true
	var finalFailureMessage string
	var finalFailureReason gatewayv1.RouteConditionReason

	for ruleIndex, rule := range grpcRoute.Spec.Rules {
		// Attempt to build the forwarding action. This will return an error if backends are invalid.
		routeAction, backendRefs, err := buildGRPCRouteAction(
			grpcRoute.Namespace,
			rule.BackendRefs,
			serviceLister,
		)

		validBackendRefs = append(validBackendRefs, backendRefs...)

		// This helper function creates the appropriate Envoy route (forward or direct response)
		// based on the success of the backend resolution.
		buildRoutesForRule := func(match gatewayv1.GRPCRouteMatch, matchIndex int) (*routev3.Route, metav1.Condition) {
			routeMatch, matchCondition := translateGRPCRouteMatch(match, grpcRoute.Generation)
			if matchCondition.Status == metav1.ConditionFalse {
				return nil, matchCondition
			}

			envoyRoute := &routev3.Route{
				Name:  fmt.Sprintf("%s-%s-rule%d-match%d", grpcRoute.Namespace, grpcRoute.Name, ruleIndex, matchIndex),
				Match: routeMatch,
			}
			var controllerErr *ControllerError
			if errors.As(err, &controllerErr) {
				// Backend resolution failed. Create a DirectResponse and a False condition.
				if isOverallSuccess { // Capture first failure details
					isOverallSuccess = false
					finalFailureMessage = controllerErr.Message
					finalFailureReason = gatewayv1.RouteConditionReason(controllerErr.Reason)
				}
				envoyRoute.Action = &routev3.Route_DirectResponse{
					DirectResponse: &routev3.DirectResponseAction{Status: 500},
				}
			} else {
				// Backend resolution succeeded. Create a normal forwarding route.
				envoyRoute.Action = &routev3.Route_Route{
					Route: routeAction,
				}
			}
			return envoyRoute, createSuccessCondition(grpcRoute.Generation)
		}

		// GRPCRoute requires at least one match per rule.
		if len(rule.Matches) == 0 {
			if isOverallSuccess {
				isOverallSuccess = false
				finalFailureMessage = "GRPCRoute rule must have at least one match"
				finalFailureReason = gatewayv1.RouteReasonBackendNotFound
			}
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
		return envoyRoutes, validBackendRefs, createSuccessCondition(grpcRoute.Generation)
	}

	return envoyRoutes, validBackendRefs, createFailureCondition(finalFailureReason, finalFailureMessage, grpcRoute.Generation)
}

// buildGRPCRouteAction attempts to create a forwarding RouteAction and returns a structured error on failure.
func buildGRPCRouteAction(
	namespace string,
	backendRefs []gatewayv1.GRPCBackendRef,
	serviceLister corev1listers.ServiceLister,
) (*routev3.RouteAction, []gatewayv1.BackendRef, error) {
	weightedClusters := &routev3.WeightedCluster{}
	var validBackendRefs []gatewayv1.BackendRef

	for _, backendRef := range backendRefs {
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
		// GRPCRoute uses BackendObjectReference which is slightly different but can be converted.
		clusterName, err := backendRefToClusterName(namespace, backendRef.BackendRef)
		if err != nil {
			return nil, nil, err
		}

		weight := int32(1)
		if backendRef.Weight != nil {
			weight = *backendRef.Weight
		}
		if weight == 0 {
			continue
		}
		validBackendRefs = append(validBackendRefs, backendRef.BackendRef)
		weightedClusters.Clusters = append(weightedClusters.Clusters, &routev3.WeightedCluster_ClusterWeight{
			Name:   clusterName,
			Weight: &wrapperspb.UInt32Value{Value: uint32(weight)},
		})
	}

	if len(weightedClusters.Clusters) == 0 {
		return nil, nil, &ControllerError{Reason: string(gatewayv1.RouteReasonAccepted), Message: "no valid backends provided with a weight > 0"}
	}

	var action *routev3.RouteAction
	if len(weightedClusters.Clusters) == 1 {
		action = &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: weightedClusters.Clusters[0].Name}}
	} else {
		action = &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_WeightedClusters{WeightedClusters: weightedClusters}}
	}

	return action, validBackendRefs, nil
}

// translateGRPCRouteMatch translates a Gateway API GRPCRouteMatch into an Envoy RouteMatch.
func translateGRPCRouteMatch(match gatewayv1.GRPCRouteMatch, generation int64) (*routev3.RouteMatch, metav1.Condition) {
	routeMatch := &routev3.RouteMatch{
		// gRPC requests are HTTP/2 POSTs with a specific content-type.
		Headers: []*routev3.HeaderMatcher{
			{
				Name:                 ":method",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_ExactMatch{ExactMatch: "POST"},
			},
			{
				Name:                 "content-type",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_PrefixMatch{PrefixMatch: "application/grpc"},
			},
		},
	}

	// Translate gRPC method match into a :path header match.
	if match.Method != nil {
		if match.Method.Service == nil || match.Method.Method == nil {
			msg := "GRPCMethodMatch requires both service and method to be set"
			return nil, createFailureCondition(gatewayv1.RouteReasonUnsupportedValue, msg, generation)
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
			msg := fmt.Sprintf("unsupported gRPC method match type: %s", matchType)
			return nil, createFailureCondition(gatewayv1.RouteReasonUnsupportedValue, msg, generation)
		}
		routeMatch.Headers = append(routeMatch.Headers, pathMatcher)
	}

	// Translate header matches.
	for _, headerMatch := range match.Headers {
		headerMatcher := &routev3.HeaderMatcher{Name: string(headerMatch.Name)}
		matchType := gatewayv1.GRPCHeaderMatchExact
		if headerMatch.Type != nil {
			matchType = *headerMatch.Type
		}

		switch matchType {
		case gatewayv1.GRPCHeaderMatchExact:
			// CHANGED: Use the modern direct `ExactMatch` field.
			headerMatcher.HeaderMatchSpecifier = &routev3.HeaderMatcher_ExactMatch{ExactMatch: headerMatch.Value}
		case gatewayv1.GRPCHeaderMatchRegularExpression:
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

	return routeMatch, createSuccessCondition(generation)
}

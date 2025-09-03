package gateway

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1listers "k8s.io/client-go/listers/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// translateHTTPRouteToEnvoyRoutes translates a full HTTPRoute into a slice of Envoy Routes.
// It now correctly handles RequestHeaderModifier filters.
func translateHTTPRouteToEnvoyRoutes(
	httpRoute *gatewayv1.HTTPRoute,
	serviceLister corev1listers.ServiceLister,
) ([]*routev3.Route, []gatewayv1.BackendRef, metav1.Condition) {

	var envoyRoutes []*routev3.Route
	var allValidBackendRefs []gatewayv1.BackendRef
	overallCondition := createSuccessCondition(httpRoute.Generation)

	for ruleIndex, rule := range httpRoute.Spec.Rules {
		var redirectAction *routev3.RedirectAction
		var headersToAdd []*corev3.HeaderValueOption
		var headersToRemove []string
		for _, filter := range rule.Filters {
			if filter.Type == gatewayv1.HTTPRouteFilterRequestRedirect && filter.RequestRedirect != nil {
				redirect := filter.RequestRedirect
				redirectAction = &routev3.RedirectAction{}

				if redirect.Hostname != nil {
					redirectAction.HostRedirect = string(*redirect.Hostname)
				}

				if redirect.StatusCode != nil {
					switch *redirect.StatusCode {
					case 301:
						redirectAction.ResponseCode = routev3.RedirectAction_MOVED_PERMANENTLY
					case 302:
						redirectAction.ResponseCode = routev3.RedirectAction_FOUND
					case 303:
						redirectAction.ResponseCode = routev3.RedirectAction_SEE_OTHER
					case 307:
						redirectAction.ResponseCode = routev3.RedirectAction_TEMPORARY_REDIRECT
					case 308:
						redirectAction.ResponseCode = routev3.RedirectAction_PERMANENT_REDIRECT
					default:
						redirectAction.ResponseCode = routev3.RedirectAction_MOVED_PERMANENTLY
					}
				} else {
					// The Gateway API spec defaults to a 302 redirect.
					// The corresponding Envoy enum is "FOUND".
					redirectAction.ResponseCode = routev3.RedirectAction_FOUND
				}

				break // Only one redirect filter is allowed per rule.
			}

			if filter.Type == gatewayv1.HTTPRouteFilterRequestHeaderModifier && filter.RequestHeaderModifier != nil {
				// Handle "set" actions (overwrite)
				for _, header := range filter.RequestHeaderModifier.Set {
					headersToAdd = append(headersToAdd, &corev3.HeaderValueOption{
						Header: &corev3.HeaderValue{
							Key:   string(header.Name),
							Value: header.Value,
						},
						// This tells Envoy to overwrite the header if it exists.
						AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
					})
				}

				// Handle "add" actions (append)
				for _, header := range filter.RequestHeaderModifier.Add {
					headersToAdd = append(headersToAdd, &corev3.HeaderValueOption{
						Header: &corev3.HeaderValue{
							Key:   string(header.Name),
							Value: header.Value,
						},
						// This tells Envoy to append the value if the header already exists.
						AppendAction: corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD,
					})
				}

				// Handle "remove" actions
				headersToRemove = append(headersToRemove, filter.RequestHeaderModifier.Remove...)
			}
		}

		buildRoutesForRule := func(match gatewayv1.HTTPRouteMatch, matchIndex int) {
			routeMatch, matchCondition := translateHTTPRouteMatch(match, httpRoute.Generation)
			if matchCondition.Status == metav1.ConditionFalse {
				overallCondition = matchCondition
				return
			}

			envoyRoute := &routev3.Route{
				Name:                   fmt.Sprintf("%s-%s-rule%d-match%d", httpRoute.Namespace, httpRoute.Name, ruleIndex, matchIndex),
				Match:                  routeMatch,
				RequestHeadersToAdd:    headersToAdd,
				RequestHeadersToRemove: headersToRemove,
			}

			if redirectAction != nil {
				// If this is a redirect, set the Redirect action. No backends are needed.
				envoyRoute.Action = &routev3.Route_Redirect{
					Redirect: redirectAction,
				}
			} else {
				// Attempt to build the forwarding action and get valid backends.
				routeAction, validBackends, err := buildHTTPRouteAction(
					httpRoute.Namespace,
					rule.BackendRefs,
					serviceLister,
				)
				var controllerErr *ControllerError
				if errors.As(err, &controllerErr) {
					overallCondition = createFailureCondition(gatewayv1.RouteConditionReason(controllerErr.Reason), controllerErr.Message, httpRoute.Generation)
					envoyRoute.Action = &routev3.Route_DirectResponse{
						DirectResponse: &routev3.DirectResponseAction{Status: 500},
					}
				} else {
					allValidBackendRefs = append(allValidBackendRefs, validBackends...)
					envoyRoute.Action = &routev3.Route_Route{
						Route: routeAction,
					}
				}
			}
			envoyRoutes = append(envoyRoutes, envoyRoute)
		}

		if len(rule.Matches) == 0 {
			buildRoutesForRule(gatewayv1.HTTPRouteMatch{}, 0)
		} else {
			for matchIndex, match := range rule.Matches {
				buildRoutesForRule(match, matchIndex)
			}
		}
	}
	return envoyRoutes, allValidBackendRefs, overallCondition
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
			if pathValue == "/" {
				routeMatch.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/"}
			} else {
				path := strings.TrimSuffix(pathValue, "/")
				routeMatch.PathSpecifier = &routev3.RouteMatch_PathSeparatedPrefix{PathSeparatedPrefix: path}
			}
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

// sortRoutes is the definitive sorter for Envoy routes based on Gateway API precedence.
func sortRoutes(routes []*routev3.Route) {
	sort.Slice(routes, func(i, j int) bool {
		matchI := routes[i].GetMatch()
		matchJ := routes[j].GetMatch()

		// De-prioritize the catch-all route, ensuring it's always last.
		isCatchAllI := isCatchAll(matchI)
		isCatchAllJ := isCatchAll(matchJ)

		if isCatchAllI != isCatchAllJ {
			// If I is the catch-all, it should come after J (return false).
			// If J is the catch-all, it should come after I (return true).
			return isCatchAllJ
		}

		// Precedence Rule 1: Exact Path Match vs. Other Path Matches
		isExactPathI := matchI.GetPath() != ""
		isExactPathJ := matchJ.GetPath() != ""
		if isExactPathI != isExactPathJ {
			return isExactPathI // Exact path is higher precedence
		}

		// Precedence Rule 2: Longest Prefix Match
		prefixI := getPathMatchValue(matchI)
		prefixJ := getPathMatchValue(matchJ)

		if len(prefixI) != len(prefixJ) {
			return len(prefixI) > len(prefixJ) // Longer prefix is higher precedence
		}

		// Precedence Rule 3: Number of Header Matches
		headerCountI := len(matchI.GetHeaders())
		headerCountJ := len(matchJ.GetHeaders())
		if headerCountI != headerCountJ {
			return headerCountI > headerCountJ // More headers is higher precedence
		}

		// Precedence Rule 4: Number of Query Param Matches
		queryCountI := len(matchI.GetQueryParameters())
		queryCountJ := len(matchJ.GetQueryParameters())
		if queryCountI != queryCountJ {
			return queryCountI > queryCountJ // More query params is higher precedence
		}

		// If all else is equal, maintain original order (stable sort)
		return false
	})
}

// getPathMatchValue is a helper to extract the path string for comparison.
func getPathMatchValue(match *routev3.RouteMatch) string {
	if match.GetPath() != "" {
		return match.GetPath()
	}
	if match.GetPrefix() != "" {
		return match.GetPrefix()
	}
	if match.GetPathSeparatedPrefix() != "" {
		return match.GetPathSeparatedPrefix()
	}
	if sr := match.GetSafeRegex(); sr != nil { // Regex Match (used for other PathPrefix)
		// This correctly handles the output of translateHTTPRouteMatch.
		regex := sr.GetRegex()
		// Remove the trailing regex that matches subpaths.
		path := strings.TrimSuffix(regex, "(/.*)?")
		// Remove the quoting added by regexp.QuoteMeta.
		path = strings.ReplaceAll(path, `\`, "")
		return path
	}
	return ""
}

// isCatchAll determines if a route match is a generic "catch-all" rule.
// A catch-all matches all paths ("/") and has no other specific conditions.
func isCatchAll(match *routev3.RouteMatch) bool {
	if match == nil {
		return false
	}
	// It's a catch-all if the path match is for "/" AND there are no other constraints.
	isRootPrefix := match.GetPrefix() == "/"
	hasNoHeaders := len(match.GetHeaders()) == 0
	hasNoParams := len(match.GetQueryParameters()) == 0

	return isRootPrefix && hasNoHeaders && hasNoParams
}

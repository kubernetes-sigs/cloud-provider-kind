package gateway

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1listers "k8s.io/client-go/listers/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewaylistersv1 "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

const (
	grpcClusterNameSuffix       = "-h2"
	grpcUnavailableClusterName  = "kind-grpc-unavailable"
	grpcStatusUnavailable       = "14" // gRPC status code UNAVAILABLE
	grpcUnavailableStatusHeader = "grpc-status"
	grpcUnavailableMessage      = "unavailable"
)

// grpcClusterName returns the HTTP/2 cluster name for a gRPC backend so it
// does not share an HTTP/1.1 cluster with HTTPRoute.
func grpcClusterName(clusterName string) string {
	return clusterName + grpcClusterNameSuffix
}

// translateGRPCRouteToEnvoyRoutes translates a GRPCRoute into Envoy routes.
// It returns the translated routes, the valid backend refs, and up to three conditions:
//   - notAccepted: non-nil (Accepted=False) when the entire route is rejected
//   - resolvedRefsFailure: non-nil (ResolvedRefs=False) only when a backend ref could not be resolved;
//     nil signals success and the caller is responsible for setting ResolvedRefs=True
//   - partiallyInvalid: non-nil (PartiallyInvalid=True) when some rules were dropped
func translateGRPCRouteToEnvoyRoutes(
	grpcRoute *gatewayv1.GRPCRoute,
	serviceLister corev1listers.ServiceLister,
	referenceGrantLister gatewaylistersv1.ReferenceGrantLister,
) (
	envoyRoutes []*routev3.Route,
	allValidBackendRefs []gatewayv1.BackendRef,
	notAccepted *metav1.Condition,
	resolvedRefsFailure *metav1.Condition,
	partiallyInvalid *metav1.Condition,
) {
	var droppedRuleMessages []string

	for ruleIndex, rule := range grpcRoute.Spec.Rules {
		if unsupportedType, found := findUnsupportedGRPCFilter(rule.Filters); found {
			droppedRuleMessages = append(droppedRuleMessages,
				fmt.Sprintf("rule[%d] has unsupported filter type %q", ruleIndex, unsupportedType))
			continue
		}

		var requestHeadersToAdd []*corev3.HeaderValueOption
		var requestHeadersToRemove []string
		var responseHeadersToAdd []*corev3.HeaderValueOption
		var responseHeadersToRemove []string
		for _, filter := range rule.Filters {
			switch filter.Type {
			case gatewayv1.GRPCRouteFilterRequestHeaderModifier:
				if filter.RequestHeaderModifier != nil {
					add, remove := translateHTTPHeaderFilter(filter.RequestHeaderModifier)
					requestHeadersToAdd = append(requestHeadersToAdd, add...)
					requestHeadersToRemove = append(requestHeadersToRemove, remove...)
				}
			case gatewayv1.GRPCRouteFilterResponseHeaderModifier:
				if filter.ResponseHeaderModifier != nil {
					add, remove := translateHTTPHeaderFilter(filter.ResponseHeaderModifier)
					responseHeadersToAdd = append(responseHeadersToAdd, add...)
					responseHeadersToRemove = append(responseHeadersToRemove, remove...)
				}
			}
		}

		mirrors, mirrorBackends, mirrorErr := buildGRPCRequestMirrors(
			grpcRoute.Namespace,
			rule.Filters,
			serviceLister,
			referenceGrantLister,
		)
		if mirrorErr != nil && resolvedRefsFailure == nil {
			var controllerErr *ControllerError
			if errors.As(mirrorErr, &controllerErr) {
				cond := createNotResolvedCondition(gatewayv1.RouteConditionReason(controllerErr.Reason), controllerErr.Message, grpcRoute.Generation)
				resolvedRefsFailure = &cond
			}
		}
		allValidBackendRefs = append(allValidBackendRefs, mirrorBackends...)

		buildRoutesForRule := func(match gatewayv1.GRPCRouteMatch, matchIndex int) {
			routeMatch, dropReason := translateGRPCRouteMatch(match)
			if dropReason != nil {
				droppedRuleMessages = append(droppedRuleMessages,
					fmt.Sprintf("rule[%d] match[%d]: %s", ruleIndex, matchIndex, *dropReason))
				return
			}

			envoyRoute := &routev3.Route{
				Name:                    fmt.Sprintf("%s-%s-rule%d-match%d", grpcRoute.Namespace, grpcRoute.Name, ruleIndex, matchIndex),
				Match:                   routeMatch,
				RequestHeadersToAdd:     requestHeadersToAdd,
				RequestHeadersToRemove:  requestHeadersToRemove,
				ResponseHeadersToAdd:    responseHeadersToAdd,
				ResponseHeadersToRemove: responseHeadersToRemove,
			}
			serviceLen, methodLen := grpcMethodMatchLengths(match.Method)
			setRouteSortMeta(envoyRoute, true, serviceLen, methodLen, grpcRoute.CreationTimestamp, grpcRoute.Namespace, grpcRoute.Name)

			routeAction, validBackends, err := buildGRPCRouteAction(
				grpcRoute.Namespace,
				rule.BackendRefs,
				serviceLister,
				referenceGrantLister,
			)
			var controllerErr *ControllerError
			var noBackendsErr *noEffectiveBackendsError
			switch {
			case errors.As(err, &noBackendsErr):
				if len(mirrors) > 0 {
					envoyRoute.Action = &routev3.Route_Route{
						Route: &routev3.RouteAction{
							ClusterSpecifier:      &routev3.RouteAction_Cluster{Cluster: grpcUnavailableClusterName},
							RequestMirrorPolicies: mirrors,
						},
					}
				} else {
					applyGRPCUnavailableDirectResponse(envoyRoute)
				}
			case errors.As(err, &controllerErr):
				if resolvedRefsFailure == nil {
					cond := createNotResolvedCondition(gatewayv1.RouteConditionReason(controllerErr.Reason), controllerErr.Message, grpcRoute.Generation)
					resolvedRefsFailure = &cond
				}
				switch {
				case routeAction != nil:
					routeAction.RequestMirrorPolicies = mirrors
					allValidBackendRefs = append(allValidBackendRefs, validBackends...)
					envoyRoute.Action = &routev3.Route_Route{Route: routeAction}
				case len(mirrors) > 0:
					envoyRoute.Action = &routev3.Route_Route{
						Route: &routev3.RouteAction{
							ClusterSpecifier:      &routev3.RouteAction_Cluster{Cluster: grpcUnavailableClusterName},
							RequestMirrorPolicies: mirrors,
						},
					}
				default:
					applyGRPCUnavailableDirectResponse(envoyRoute)
				}
			default:
				allValidBackendRefs = append(allValidBackendRefs, validBackends...)
				if routeAction != nil {
					routeAction.RequestMirrorPolicies = mirrors
				}
				envoyRoute.Action = &routev3.Route_Route{
					Route: routeAction,
				}
			}
			envoyRoutes = append(envoyRoutes, envoyRoute)
		}

		if len(rule.Matches) == 0 {
			buildRoutesForRule(gatewayv1.GRPCRouteMatch{}, 0)
		} else {
			for matchIndex, match := range rule.Matches {
				buildRoutesForRule(match, matchIndex)
			}
		}
	}

	if len(droppedRuleMessages) > 0 && len(envoyRoutes) == 0 {
		msg := fmt.Sprintf("no rules could be translated: %s", strings.Join(droppedRuleMessages, "; "))
		cond := createNotAcceptedCondition(gatewayv1.RouteReasonUnsupportedValue, msg, grpcRoute.Generation)
		notAccepted = &cond
		return nil, nil, notAccepted, nil, nil
	}

	if len(droppedRuleMessages) > 0 {
		msg := fmt.Sprintf("Dropped Rule(s): %s", strings.Join(droppedRuleMessages, "; "))
		cond := createPartiallyInvalidCondition(msg, grpcRoute.Generation)
		partiallyInvalid = &cond
	}
	return envoyRoutes, allValidBackendRefs, nil, resolvedRefsFailure, partiallyInvalid
}

// findUnsupportedGRPCFilter returns the first filter type that this controller
// does not implement. ExtensionRef is unsupported and causes the rule to be dropped.
func findUnsupportedGRPCFilter(filters []gatewayv1.GRPCRouteFilter) (gatewayv1.GRPCRouteFilterType, bool) {
	for _, filter := range filters {
		switch filter.Type {
		case gatewayv1.GRPCRouteFilterRequestHeaderModifier,
			gatewayv1.GRPCRouteFilterResponseHeaderModifier,
			gatewayv1.GRPCRouteFilterRequestMirror:
		default:
			return filter.Type, true
		}
	}
	return "", false
}

// applyGRPCUnavailableDirectResponse configures a gRPC UNAVAILABLE (status 14)
// response. GEP-1016 requires this instead of HTTP 500 when no backend is usable.
func applyGRPCUnavailableDirectResponse(route *routev3.Route) {
	route.Action = &routev3.Route_DirectResponse{
		DirectResponse: &routev3.DirectResponseAction{Status: 200},
	}
	overwrite := corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD
	route.ResponseHeadersToAdd = append(route.ResponseHeadersToAdd,
		&corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: "content-type", Value: "application/grpc"},
			AppendAction: overwrite,
		},
		&corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: grpcUnavailableStatusHeader, Value: grpcStatusUnavailable},
			AppendAction: overwrite,
		},
		&corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: "grpc-message", Value: grpcUnavailableMessage},
			AppendAction: overwrite,
		},
	)
}

// buildGRPCRouteAction returns an Envoy route action, the valid BackendRefs, and
// a structured error. Invalid backends contribute weight to kind-grpc-unavailable.
func buildGRPCRouteAction(namespace string, backendRefs []gatewayv1.GRPCBackendRef, serviceLister corev1listers.ServiceLister, referenceGrantLister gatewaylistersv1.ReferenceGrantLister) (*routev3.RouteAction, []gatewayv1.BackendRef, error) {
	weightedClusters := &routev3.WeightedCluster{}
	var validBackendRefs []gatewayv1.BackendRef
	var firstErr error
	var unavailableWeight uint32

	for _, grpcBackendRef := range backendRefs {
		backendRef := grpcBackendRef.BackendRef

		weight := int32(1)
		if grpcBackendRef.Weight != nil {
			weight = *grpcBackendRef.Weight
		}
		if weight == 0 {
			continue
		}

		ns := namespace
		if backendRef.Namespace != nil {
			ns = string(*backendRef.Namespace)
		}

		if ns != namespace {
			from := gatewayv1.ReferenceGrantFrom{
				Group:     gatewayv1.GroupName,
				Kind:      "GRPCRoute",
				Namespace: gatewayv1.Namespace(namespace),
			}
			to := gatewayv1.ReferenceGrantTo{
				Group: "",
				Kind:  "Service",
				Name:  &backendRef.Name,
			}

			if !isCrossNamespaceRefAllowed(from, to, ns, referenceGrantLister) {
				if firstErr == nil {
					firstErr = &ControllerError{
						Reason:  string(gatewayv1.RouteReasonRefNotPermitted),
						Message: fmt.Sprintf("reference to Service %s/%s not permitted by any ReferenceGrant", ns, backendRef.Name),
					}
				}
				unavailableWeight += uint32(weight)
				continue
			}
		}

		if _, err := serviceLister.Services(ns).Get(string(backendRef.Name)); err != nil {
			if firstErr == nil {
				firstErr = &ControllerError{
					Reason:  string(gatewayv1.RouteReasonBackendNotFound),
					Message: fmt.Sprintf("reference to Service %s/%s not found", ns, backendRef.Name),
				}
			}
			unavailableWeight += uint32(weight)
			continue
		}
		clusterName, err := backendRefToClusterName(namespace, backendRef)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			unavailableWeight += uint32(weight)
			continue
		}

		validBackendRefs = append(validBackendRefs, backendRef)
		weightedClusters.Clusters = append(weightedClusters.Clusters, &routev3.WeightedCluster_ClusterWeight{
			Name:   grpcClusterName(clusterName),
			Weight: &wrapperspb.UInt32Value{Value: uint32(weight)},
		})
	}

	if len(weightedClusters.Clusters) == 0 {
		if firstErr != nil {
			return nil, nil, firstErr
		}
		return nil, nil, &noEffectiveBackendsError{}
	}

	if unavailableWeight > 0 {
		weightedClusters.Clusters = append(weightedClusters.Clusters, &routev3.WeightedCluster_ClusterWeight{
			Name:   grpcUnavailableClusterName,
			Weight: &wrapperspb.UInt32Value{Value: unavailableWeight},
		})
	}

	var action *routev3.RouteAction
	if len(weightedClusters.Clusters) == 1 {
		action = &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: weightedClusters.Clusters[0].Name}}
	} else {
		action = &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_WeightedClusters{WeightedClusters: weightedClusters}}
	}

	return action, validBackendRefs, firstErr
}

// buildGRPCRequestMirrors translates RequestMirror filters into Envoy request
// mirror policies. Unresolvable mirror backends are skipped and reported.
func buildGRPCRequestMirrors(
	namespace string,
	filters []gatewayv1.GRPCRouteFilter,
	serviceLister corev1listers.ServiceLister,
	referenceGrantLister gatewaylistersv1.ReferenceGrantLister,
) ([]*routev3.RouteAction_RequestMirrorPolicy, []gatewayv1.BackendRef, error) {
	var policies []*routev3.RouteAction_RequestMirrorPolicy
	var validBackendRefs []gatewayv1.BackendRef
	var firstErr error

	for _, filter := range filters {
		if filter.Type != gatewayv1.GRPCRouteFilterRequestMirror || filter.RequestMirror == nil {
			continue
		}
		backendRef := gatewayv1.BackendRef{BackendObjectReference: filter.RequestMirror.BackendRef}
		ns := namespace
		if backendRef.Namespace != nil {
			ns = string(*backendRef.Namespace)
		}
		if ns != namespace {
			from := gatewayv1.ReferenceGrantFrom{
				Group:     gatewayv1.GroupName,
				Kind:      "GRPCRoute",
				Namespace: gatewayv1.Namespace(namespace),
			}
			to := gatewayv1.ReferenceGrantTo{
				Group: "",
				Kind:  "Service",
				Name:  &backendRef.Name,
			}
			if !isCrossNamespaceRefAllowed(from, to, ns, referenceGrantLister) {
				if firstErr == nil {
					firstErr = &ControllerError{
						Reason:  string(gatewayv1.RouteReasonRefNotPermitted),
						Message: fmt.Sprintf("reference to Service %s/%s not permitted by any ReferenceGrant", ns, backendRef.Name),
					}
				}
				continue
			}
		}
		if _, err := serviceLister.Services(ns).Get(string(backendRef.Name)); err != nil {
			if firstErr == nil {
				firstErr = &ControllerError{
					Reason:  string(gatewayv1.RouteReasonBackendNotFound),
					Message: fmt.Sprintf("reference to Service %s/%s not found", ns, backendRef.Name),
				}
			}
			continue
		}
		clusterName, err := backendRefToClusterName(namespace, backendRef)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		validBackendRefs = append(validBackendRefs, backendRef)
		policies = append(policies, &routev3.RouteAction_RequestMirrorPolicy{
			Cluster:         grpcClusterName(clusterName),
			RuntimeFraction: mirrorRuntimeFraction(filter.RequestMirror),
		})
	}
	return policies, validBackendRefs, firstErr
}

// mirrorRuntimeFraction converts a Gateway API mirror fraction or percent into
// Envoy RuntimeFractionalPercent.
func mirrorRuntimeFraction(filter *gatewayv1.HTTPRequestMirrorFilter) *corev3.RuntimeFractionalPercent {
	fp := &typev3.FractionalPercent{
		Numerator:   100,
		Denominator: typev3.FractionalPercent_HUNDRED,
	}
	if filter == nil {
		return &corev3.RuntimeFractionalPercent{DefaultValue: fp}
	}
	switch {
	case filter.Fraction != nil:
		den := int32(100)
		if filter.Fraction.Denominator != nil {
			den = *filter.Fraction.Denominator
		}
		// Envoy FractionalPercent supports 100, 10_000, and 1_000_000.
		switch den {
		case 10000:
			fp.Numerator = uint32(filter.Fraction.Numerator)
			fp.Denominator = typev3.FractionalPercent_TEN_THOUSAND
		case 1000000:
			fp.Numerator = uint32(filter.Fraction.Numerator)
			fp.Denominator = typev3.FractionalPercent_MILLION
		default:
			// Convert N/den to millionths so 1000 and other denominators work.
			if den > 0 {
				fp.Numerator = uint32(int64(filter.Fraction.Numerator) * 1000000 / int64(den))
				fp.Denominator = typev3.FractionalPercent_MILLION
			}
		}
	case filter.Percent != nil:
		fp.Numerator = uint32(*filter.Percent)
	}
	return &corev3.RuntimeFractionalPercent{DefaultValue: fp}
}

// grpcMethodMatchLengths returns service and method string lengths used for
// GEP-1016 route precedence (longer match wins).
func grpcMethodMatchLengths(method *gatewayv1.GRPCMethodMatch) (serviceLen, methodLen int) {
	if method == nil {
		return 0, 0
	}
	if method.Service != nil {
		serviceLen = len(*method.Service)
	}
	if method.Method != nil {
		methodLen = len(*method.Method)
	}
	return serviceLen, methodLen
}

// translateGRPCRouteMatch translates a GRPCRouteMatch to an Envoy RouteMatch.
// gRPC service/method maps to the HTTP/2 path /package.Service/Method.
// Duplicate header match names are ignored after the first occurrence.
func translateGRPCRouteMatch(match gatewayv1.GRPCRouteMatch) (*routev3.RouteMatch, *string) {
	routeMatch := &routev3.RouteMatch{}

	if match.Method != nil {
		service := ""
		if match.Method.Service != nil {
			service = *match.Method.Service
		}
		method := ""
		if match.Method.Method != nil {
			method = *match.Method.Method
		}

		matchType := gatewayv1.GRPCMethodMatchExact
		if match.Method.Type != nil {
			matchType = *match.Method.Type
		}

		switch matchType {
		case gatewayv1.GRPCMethodMatchExact:
			switch {
			case service != "" && method != "":
				routeMatch.PathSpecifier = &routev3.RouteMatch_Path{Path: "/" + service + "/" + method}
			case service != "":
				routeMatch.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/" + service + "/"}
			case method != "":
				routeMatch.PathSpecifier = &routev3.RouteMatch_SafeRegex{
					SafeRegex: &matcherv3.RegexMatcher{
						EngineType: &matcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &matcherv3.RegexMatcher_GoogleRE2{}},
						Regex:      "/[^/]+/" + method,
					},
				}
			default:
				routeMatch.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/"}
			}
		case gatewayv1.GRPCMethodMatchRegularExpression:
			var pattern string
			switch {
			case service != "" && method != "":
				pattern = "/" + service + "/" + method
			case service != "":
				pattern = "/" + service + "/.*"
			case method != "":
				pattern = "/[^/]+/" + method
			default:
				pattern = "/.*"
			}
			routeMatch.PathSpecifier = &routev3.RouteMatch_SafeRegex{
				SafeRegex: &matcherv3.RegexMatcher{
					EngineType: &matcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &matcherv3.RegexMatcher_GoogleRE2{}},
					Regex:      pattern,
				},
			}
		default:
			msg := fmt.Sprintf("unsupported gRPC method match type: %s", matchType)
			return nil, &msg
		}
	} else {
		routeMatch.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/"}
	}

	seenHeaders := map[string]struct{}{}
	for _, headerMatch := range match.Headers {
		canonical := strings.ToLower(string(headerMatch.Name))
		if _, seen := seenHeaders[canonical]; seen {
			continue
		}
		seenHeaders[canonical] = struct{}{}

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
			msg := fmt.Sprintf("unsupported header match type: %s", matchType)
			return nil, &msg
		}
		routeMatch.Headers = append(routeMatch.Headers, headerMatcher)
	}

	return routeMatch, nil
}

// translateHTTPHeaderFilter converts a Gateway API HTTPHeaderFilter into Envoy
// header add and remove operations. Set overwrites an existing header. Add
// appends to an existing header. Remove deletes the named headers.
func translateHTTPHeaderFilter(filter *gatewayv1.HTTPHeaderFilter) (toAdd []*corev3.HeaderValueOption, toRemove []string) {
	if filter == nil {
		return nil, nil
	}
	for _, header := range filter.Set {
		toAdd = append(toAdd, &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{
				Key:   string(header.Name),
				Value: header.Value,
			},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}
	for _, header := range filter.Add {
		toAdd = append(toAdd, &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{
				Key:   string(header.Name),
				Value: header.Value,
			},
			AppendAction: corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD,
		})
	}
	toRemove = append(toRemove, filter.Remove...)
	return toAdd, toRemove
}

const routeSortMetadataKey = "cloud-provider-kind.gateway"

type routeSortMeta struct {
	isGRPC     bool
	serviceLen int
	methodLen  int
	created    int64
	nsName     string
}

func setRouteSortMeta(route *routev3.Route, isGRPC bool, serviceLen, methodLen int, created metav1.Time, namespace, name string) {
	if route == nil {
		return
	}
	st, err := structpb.NewStruct(map[string]interface{}{
		"grpc":        isGRPC,
		"service_len": serviceLen,
		"method_len":  methodLen,
		"created":     created.UnixMicro(),
		"ns_name":     namespace + "/" + name,
	})
	if err != nil {
		return
	}
	route.Metadata = &corev3.Metadata{
		FilterMetadata: map[string]*structpb.Struct{
			routeSortMetadataKey: st,
		},
	}
}

func getRouteSortMeta(route *routev3.Route) routeSortMeta {
	if route == nil || route.Metadata == nil {
		return routeSortMeta{}
	}
	st := route.Metadata.FilterMetadata[routeSortMetadataKey]
	if st == nil {
		return routeSortMeta{}
	}
	fields := st.Fields
	meta := routeSortMeta{}
	if v, ok := fields["grpc"]; ok {
		meta.isGRPC = v.GetBoolValue()
	}
	if v, ok := fields["service_len"]; ok {
		meta.serviceLen = int(v.GetNumberValue())
	}
	if v, ok := fields["method_len"]; ok {
		meta.methodLen = int(v.GetNumberValue())
	}
	if v, ok := fields["created"]; ok {
		meta.created = int64(v.GetNumberValue())
	}
	if v, ok := fields["ns_name"]; ok {
		meta.nsName = v.GetStringValue()
	}
	return meta
}

func compareRouteMetaTieBreak(a, b routeSortMeta) int {
	if a.created != b.created {
		if a.created < b.created {
			return -1
		}
		return 1
	}
	if a.nsName != b.nsName {
		if a.nsName < b.nsName {
			return -1
		}
		return 1
	}
	return 0
}

func virtualHostHasGRPCRoutes(routes []*routev3.Route) bool {
	for _, route := range routes {
		if getRouteSortMeta(route).isGRPC {
			return true
		}
	}
	return false
}

func sortGRPCRoutes(routes []*routev3.Route) {
	sort.SliceStable(routes, func(i, j int) bool {
		return lessGRPCRoute(routes[i], routes[j], getRouteSortMeta(routes[i]), getRouteSortMeta(routes[j]))
	})
}

func lessGRPCRoute(routeI, routeJ *routev3.Route, metaI, metaJ routeSortMeta) bool {
	matchI := routeI.GetMatch()
	matchJ := routeJ.GetMatch()

	isCatchAllI := isCatchAll(matchI)
	isCatchAllJ := isCatchAll(matchJ)
	if isCatchAllI != isCatchAllJ {
		return isCatchAllJ
	}
	if metaI.serviceLen != metaJ.serviceLen {
		return metaI.serviceLen > metaJ.serviceLen
	}
	if metaI.methodLen != metaJ.methodLen {
		return metaI.methodLen > metaJ.methodLen
	}
	headerCountI := len(matchI.GetHeaders())
	headerCountJ := len(matchJ.GetHeaders())
	if headerCountI != headerCountJ {
		return headerCountI > headerCountJ
	}
	return compareRouteMetaTieBreak(metaI, metaJ) < 0
}

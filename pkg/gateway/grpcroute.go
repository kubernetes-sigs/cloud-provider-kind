package gateway

import (
	"fmt"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	corev1 "k8s.io/api/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// translateGRPCRouteToEnvoyResources translates a Gateway API GRPCRoute
// into a slice of Envoy Route and Cluster resources.
// TODO
func translateGRPCRouteToEnvoyResources(serviceLister corev1listers.ServiceLister, route *gatewayv1.GRPCRoute) (map[resourcev3.Type][]interface{}, error) {
	resources := make(map[resourcev3.Type][]interface{})
	var envoyRoutes []*routev3.Route
	var envoyClusters []*clusterv3.Cluster

	for i, rule := range route.Spec.Rules {
		if len(rule.BackendRefs) == 0 {
			klog.V(2).Infof("Warning: grpcroute rule %d has no backendRefs, skipping.", i)
			continue
		}

		// Create a unique cluster for each unique backend in the rule
		backendClusters := make(map[string]*clusterv3.Cluster)
		ruleHasValidBackend := false // Track if at least one backend in the rule is valid
		for j, backendRef := range rule.BackendRefs {
			if backendRef.Port == nil {
				klog.V(2).Infof("Warning: grpcroute rule %d, backendRef %d has no port specified, skipping.", i, j)
				continue
			}

			// --- Start BackendRef Validation ---

			// 1. InvalidKind: Ensure Group and Kind point to a Service
			//    Group defaults to "" (core), Kind defaults to "Service"
			if backendRef.Group == nil {
				// Use default group ""
			} else if *backendRef.Group != "" && *backendRef.Group != "core" {
				klog.Warningf("grpcroute %s/%s rule %d, backendRef %d: Invalid Kind: Unsupported Group '%s'. Only the core group ('') is supported. Skipping.", route.Namespace, route.Name, i, j, *backendRef.Group)
				continue
			}
			if backendRef.Kind == nil {
				// Use default kind "Service"
			} else if *backendRef.Kind != "Service" {
				klog.Warningf("grpcroute %s/%s rule %d, backendRef %d: Invalid Kind: Unsupported Kind '%s'. Only 'Service' is supported. Skipping.", route.Namespace, route.Name, i, j, *backendRef.Kind)
				continue
			}

			// 2. RefNotPermitted: Check cross-namespace references (ReferenceGrant check needed)
			backendNamespace := route.Namespace
			if backendRef.Namespace != nil {
				backendNamespace = string(*backendRef.Namespace)
			}
			if backendNamespace != route.Namespace {
				// TODO: Implement ReferenceGrant check.
				// For now, log a warning but allow the reference.
				// If ReferenceGrants are enforced, this should `continue` if not permitted.
				klog.Warningf("grpcroute %s/%s rule %d, backendRef %d: Cross-namespace reference to %s/%s detected. ReferenceGrant check is required but not implemented. Allowing for now.", route.Namespace, route.Name, i, j, backendNamespace, backendRef.Name)
				// Set Reason: RefNotPermitted if check fails
				// continue
			}

			// 3. BackendNotFound: Check if the Service exists
			backendName := string(backendRef.Name)
			service, err := serviceLister.Services(backendNamespace).Get(backendName)
			if err != nil {
				if apierrors.IsNotFound(err) {
					klog.Warningf("grpcroute %s/%s rule %d, backendRef %d: Backend Not Found: Service %s/%s not found. Skipping.", route.Namespace, route.Name, i, j, backendNamespace, backendName)
					// Set Reason: BackendNotFound
				} else {
					klog.Errorf("grpcroute %s/%s rule %d, backendRef %d: Error getting Service %s/%s: %v", route.Namespace, route.Name, i, j, backendNamespace, backendName, err)
				}
				continue
			}

			// 4. BackendNotFound / InvalidBackendPort: Check if the port exists on the Service
			backendPort := uint32(*backendRef.Port)
			var foundPort *corev1.ServicePort
			for _, sp := range service.Spec.Ports {
				if sp.Port == int32(backendPort) {
					foundPort = &sp // Use a pointer to the found port
					break
				}
			}
			if foundPort == nil {
				klog.Warningf("grpcroute %s/%s rule %d, backendRef %d: Backend Not Found: Port %d not found on Service %s/%s. Skipping.", route.Namespace, route.Name, i, j, backendPort, backendNamespace, backendName)
				// Set Reason: BackendNotFound (or a more specific one if defined)
				continue
			}

			// 5. UnsupportedProtocol: Check Service appProtocol (for grpcroute)
			// For grpcroute, compatible protocols are typically nil, "", "http", "https".
			// We allow these explicitly. Other protocols might be vendor-specific HTTP variants.
			// For simplicity, we'll warn on anything else but allow it for now.
			// A stricter implementation might reject protocols like "tcp".
			if foundPort.AppProtocol != nil && *foundPort.AppProtocol != "" && *foundPort.AppProtocol != "http" && *foundPort.AppProtocol != "https" {
				// TODO: Potentially reject based on stricter compatibility rules.
				klog.Warningf("grpcroute %s/%s rule %d, backendRef %d: Potentially Unsupported Protocol: Service %s/%s Port %d has appProtocol '%s'. Ensure compatibility.", route.Namespace, route.Name, i, j, backendNamespace, backendName, backendPort, *foundPort.AppProtocol)
				// If strictly incompatible (e.g., "tcp"), set Reason: UnsupportedProtocol and `continue`
			}

			// 6. BackendTLSPolicy Check (Skipped - Experimental)
			// TODO: Add BackendTLSPolicy validation when the feature is stable.

			// --- End BackendRef Validation ---

			// If we reach here, the backendRef is considered valid for processing
			ruleHasValidBackend = true

			// Generate Envoy Cluster config
			clusterName := fmt.Sprintf("gateway-grpcroute-%s-%s-rule-%d-backend-%s-%d",
				route.Namespace, route.Name, i, backendName, backendPort)

			if _, exists := backendClusters[clusterName]; !exists {
				cluster := &clusterv3.Cluster{
					Name:                 clusterName,
					ConnectTimeout:       durationpb.New(5 * time.Second), // Default connect timeout
					ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
					LbPolicy:             clusterv3.Cluster_ROUND_ROBIN,
					LoadAssignment: &endpointv3.ClusterLoadAssignment{
						ClusterName: clusterName,
						Endpoints: []*endpointv3.LocalityLbEndpoints{
							&endpointv3.LocalityLbEndpoints{
								LbEndpoints: []*endpointv3.LbEndpoint{{
									HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
										Endpoint: &endpointv3.Endpoint{
											Address: &corev3.Address{
												Address: &corev3.Address_SocketAddress{
													SocketAddress: &corev3.SocketAddress{
														Protocol: corev3.SocketAddress_TCP,
														Address:  service.Spec.ClusterIP, // Use Service ClusterIP
														PortSpecifier: &corev3.SocketAddress_PortValue{
															PortValue: uint32(foundPort.Port), // Use the *Service* port number
														},
													},
												},
											},
										},
									},
								}},
							},
						},
					},
				}
				backendClusters[clusterName] = cluster
				envoyClusters = append(envoyClusters, cluster)
			}
		}

		// Create an Envoy Route for this rule
		// Only create the route if there was at least one valid backendRef
		if !ruleHasValidBackend {
			klog.Warningf("grpcroute %s/%s rule %d: No valid backendRefs found after validation. Skipping rule.", route.Namespace, route.Name, i)
			continue
		}

	}

	if len(envoyRoutes) > 0 {
		resources[resourcev3.RouteType] = append(resources[resourcev3.RouteType], toInterfaceSlice(envoyRoutes)...)
	}
	if len(envoyClusters) > 0 {
		resources[resourcev3.ClusterType] = append(resources[resourcev3.ClusterType], toInterfaceSlice(envoyClusters)...)
	}

	return resources, nil
}

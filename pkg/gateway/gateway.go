package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	envoyproxytypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func (c *Controller) syncGateway(ctx context.Context, key string) error {
	startTime := time.Now()
	defer func() {
		klog.V(2).Infof("Finished syncing gateway %q (%v)", key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	gw, err := c.gatewayLister.Gateways(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		klog.V(2).Infof("Gateway %s has been deleted, cleaning up resources", key)
		return c.deleteGatewayResources(ctx, name, namespace)
	}
	if err != nil {
		return fmt.Errorf("failed to get gateway %s: %w", key, err)
	}

	if gw.Spec.GatewayClassName != GWClassName {
		klog.V(2).Infof("Gateway %s is not for this controller, ignoring", key)
		return nil
	}

	containerName := gatewayName(c.clusterName, namespace, name)
	klog.Infof("Syncing Gateway %s, container %s", key, containerName)

	err = c.ensureGatewayContainer(ctx, gw)
	if err != nil {
		return fmt.Errorf("failed to ensure gateway container %s: %w", containerName, err)
	}

	newGw := gw.DeepCopy()
	ipv4, ipv6, err := container.IPs(containerName)
	if err != nil {
		if strings.Contains(err.Error(), "failed to get container details") {
			return err
		}
		return err
	}

	newGw.Status.Addresses = []gatewayv1.GatewayStatusAddress{}
	if net.ParseIP(ipv4) != nil {
		newGw.Status.Addresses = append(newGw.Status.Addresses,
			gatewayv1.GatewayStatusAddress{
				Type:  ptr.To(gatewayv1.IPAddressType),
				Value: ipv4,
			})
	}
	if net.ParseIP(ipv6) != nil {
		newGw.Status.Addresses = append(newGw.Status.Addresses,
			gatewayv1.GatewayStatusAddress{
				Type:  ptr.To(gatewayv1.IPAddressType),
				Value: ipv6,
			})
	}

	// TODO
	// Validate and update gatewayv1.GatewayStatusAddress correctly

	envoyResources, listenerStatuses, httpRoutes, grpcRoutes := c.buildEnvoyResourcesForGateway(newGw)
	newGw.Status.Listeners = listenerStatuses
	err = c.UpdateXDSServer(ctx, containerName, envoyResources)

	programmedCondition := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		ObservedGeneration: newGw.Generation,
	}
	if err != nil {
		// If the Envoy update fails, the Gateway is not programmed.
		programmedCondition.Status = metav1.ConditionFalse
		programmedCondition.Reason = "ReconciliationError"
		programmedCondition.Message = fmt.Sprintf("Failed to program envoy config: %s", err.Error())
	} else {
		// If the Envoy update succeeds, check if all individual listeners were programmed.
		listenersProgrammed := 0
		for _, listenerStatus := range listenerStatuses {
			if meta.IsStatusConditionTrue(listenerStatus.Conditions, string(gatewayv1.ListenerConditionProgrammed)) {
				listenersProgrammed++
			}
		}

		if listenersProgrammed == len(listenerStatuses) {
			// The Gateway is only fully programmed if all listeners are programmed.
			programmedCondition.Status = metav1.ConditionTrue
			programmedCondition.Reason = string(gatewayv1.GatewayReasonProgrammed)
			programmedCondition.Message = "Envoy configuration updated successfully"
		} else {
			// If any listener failed, the Gateway as a whole is not fully programmed.
			programmedCondition.Status = metav1.ConditionFalse
			programmedCondition.Reason = "ListenersNotProgrammed"
			programmedCondition.Message = fmt.Sprintf("%d out of %d listeners failed to be programmed", listenersProgrammed, len(listenerStatuses))
		}
	}
	meta.SetStatusCondition(&newGw.Status.Conditions, programmedCondition)

	meta.SetStatusCondition(&newGw.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gatewayv1.GatewayReasonAccepted),
		Message:            "Gateway is accepted",
		ObservedGeneration: newGw.Generation,
	})

	if !reflect.DeepEqual(gw.Status, newGw.Status) {
		_, err := c.gwClient.GatewayV1().Gateways(newGw.Namespace).UpdateStatus(ctx, newGw, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("Failed to update gateway status: %v", err)
			return err
		}
	}
	return c.updateRouteStatuses(ctx, newGw, programmedCondition, httpRoutes, grpcRoutes)
}

func (c *Controller) buildEnvoyResourcesForGateway(gateway *gatewayv1.Gateway) (map[resourcev3.Type][]envoyproxytypes.Resource, []gatewayv1.ListenerStatus, []*gatewayv1.HTTPRoute, []*gatewayv1.GRPCRoute) {
	envoyRoutes := []envoyproxytypes.Resource{}
	envoyClusters := make(map[string]envoyproxytypes.Resource)
	allListenerStatuses := make(map[gatewayv1.SectionName]gatewayv1.ListenerStatus)
	var processedHTTPRoutes []*gatewayv1.HTTPRoute
	var processedGRPCRoutes []*gatewayv1.GRPCRoute

	// Aggregate Listeners by Port
	listenersByPort := make(map[gatewayv1.PortNumber][]gatewayv1.Listener)
	for _, listener := range gateway.Spec.Listeners {
		listenersByPort[listener.Port] = append(listenersByPort[listener.Port], listener)
	}

	finalEnvoyListeners := []envoyproxytypes.Resource{}
	// Process Listeners by Port
	for port, listeners := range listenersByPort {
		var filterChains []*listenerv3.FilterChain
		// All these listeners have the same port
		for _, listener := range listeners {
			var attachedRoutes int32
			listenerStatus := gatewayv1.ListenerStatus{
				Name:           gatewayv1.SectionName(listener.Name),
				SupportedKinds: []gatewayv1.RouteGroupKind{},
				Conditions:     []metav1.Condition{},
				AttachedRoutes: 0,
			}
			supportedKinds, allKindsValid := getSupportedKinds(listener)
			listenerStatus.SupportedKinds = supportedKinds

			if !allKindsValid {
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionFalse,
					Reason:             string(gatewayv1.ListenerReasonInvalidRouteKinds),
					Message:            "Invalid route kinds specified in allowedRoutes",
					ObservedGeneration: gateway.Generation,
				})
				allListenerStatuses[listener.Name] = listenerStatus
				continue // Stop processing this invalid listener
			}

			virtualHost := &routev3.VirtualHost{
				Name:    fmt.Sprintf("%s-%s", gateway.Name, listener.Name),
				Domains: []string{"*"},
			}
			if listener.Hostname != nil && *listener.Hostname != "" {
				virtualHost.Domains = []string{string(*listener.Hostname)}
			}

			virtualHosts := make(map[string]*routev3.VirtualHost)

			switch listener.Protocol {
			case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
				// Process HTTPRoutes
				httpRoutes := c.getHTTPRoutesForListener(gateway, listener)
				processedHTTPRoutes = append(processedHTTPRoutes, httpRoutes...)
				for _, httpRoute := range httpRoutes {
					hostnames := getRouteHostnames(httpRoute.Spec.Hostnames, listener)
					for _, hostname := range hostnames {
						vh, ok := virtualHosts[hostname]
						if !ok {
							vh = &routev3.VirtualHost{
								Name:    fmt.Sprintf("%s-%s-%s", gateway.Name, listener.Name, hostname),
								Domains: []string{hostname},
							}
							virtualHosts[hostname] = vh
						}
						// The translation function now populates the correct VirtualHost
						if err := translateHTTPRouteToEnvoyVirtualHost(httpRoute, vh); err == nil {
							// Only count attached routes once per route, not per hostname
							if len(vh.Routes) == len(httpRoute.Spec.Rules) {
								attachedRoutes++
							}
							for _, rule := range httpRoute.Spec.Rules {
								for _, backendRef := range rule.BackendRefs {
									cluster, err := c.translateBackendRefToCluster(httpRoute.Namespace, backendRef.BackendRef)
									if err == nil {
										envoyClusters[cluster.Name] = cluster
									}
								}
							}
						}
					}
				}

				// Process GRPCRoutes
				grpcRoutes := c.getGRPCRoutesForListener(gateway, listener)
				processedGRPCRoutes = append(processedGRPCRoutes, grpcRoutes...)
				for _, grpcRoute := range grpcRoutes {
					hostnames := getRouteHostnames(grpcRoute.Spec.Hostnames, listener)
					for _, hostname := range hostnames {
						vh, ok := virtualHosts[hostname]
						if !ok {
							vh = &routev3.VirtualHost{
								Name:    fmt.Sprintf("%s-%s-%s", gateway.Name, listener.Name, hostname),
								Domains: []string{hostname},
							}
							virtualHosts[hostname] = vh
						}
						if err := translateGRPCRouteToEnvoyVirtualHost(grpcRoute, vh); err == nil {
							if len(vh.Routes) == len(grpcRoute.Spec.Rules) {
								attachedRoutes++
							}
							for _, rule := range grpcRoute.Spec.Rules {
								for _, backendRef := range rule.BackendRefs {
									cluster, err := c.translateBackendRefToCluster(grpcRoute.Namespace, backendRef.BackendRef)
									if err == nil {
										envoyClusters[cluster.Name] = cluster
									}
								}
							}
						}
					}
				}
			default:
				klog.Warningf("Unsupported listener protocol for route processing: %s", listener.Protocol)
			}

			vhSlice := make([]*routev3.VirtualHost, 0, len(virtualHosts))
			for _, vh := range virtualHosts {
				vhSlice = append(vhSlice, vh)
			}

			filterChain, routeConfig, err := c.translateListenerToFilterChain(gateway, listener, vhSlice)
			if err != nil {
				// If translation fails, a reference is unresolved. Set both conditions to False.
				klog.Errorf("Error translating listener %s to filter chain: %v", listener.Name, err)
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionFalse,
					Reason:             string(gatewayv1.ListenerReasonInvalidCertificateRef),
					Message:            fmt.Sprintf("Failed to resolve references: %v", err),
					ObservedGeneration: gateway.Generation,
				})
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionFalse,
					Reason:             string(gatewayv1.ListenerReasonInvalid),
					Message:            fmt.Sprintf("Failed to program listener: %v", err),
					ObservedGeneration: gateway.Generation,
				})
			} else {
				// Only if ALL checks pass, set the conditions to True.
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
					Message:            "All references resolved",
					ObservedGeneration: gateway.Generation,
				})
				meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener is programmed",
					ObservedGeneration: gateway.Generation,
				})

				if routeConfig != nil {
					envoyRoutes = append(envoyRoutes, routeConfig)
				}
				filterChains = append(filterChains, filterChain)
			}

			listenerStatus.AttachedRoutes = attachedRoutes
			meta.SetStatusCondition(&listenerStatus.Conditions, metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionAccepted),
				Status:             metav1.ConditionTrue,
				Reason:             string(gatewayv1.ListenerReasonAccepted),
				Message:            "Listener is valid",
				ObservedGeneration: gateway.Generation,
			})
			allListenerStatuses[listener.Name] = listenerStatus
		}

		if len(filterChains) > 0 {
			envoyListener := &listenerv3.Listener{
				Name:            fmt.Sprintf("listener-%d", port),
				Address:         createEnvoyAddress(uint32(port)),
				FilterChains:    filterChains,
				ListenerFilters: createListenerFilters(),
			}
			finalEnvoyListeners = append(finalEnvoyListeners, envoyListener)
		}
	}

	clustersSlice := make([]envoyproxytypes.Resource, 0, len(envoyClusters))
	for _, cluster := range envoyClusters {
		clustersSlice = append(clustersSlice, cluster)
	}

	orderedStatuses := make([]gatewayv1.ListenerStatus, len(gateway.Spec.Listeners))
	for i, listener := range gateway.Spec.Listeners {
		orderedStatuses[i] = allListenerStatuses[listener.Name]
	}

	return map[resourcev3.Type][]envoyproxytypes.Resource{
			resourcev3.ListenerType: finalEnvoyListeners,
			resourcev3.RouteType:    envoyRoutes,
			resourcev3.ClusterType:  clustersSlice,
		}, orderedStatuses,
		processedHTTPRoutes,
		processedGRPCRoutes
}

func getSupportedKinds(listener gatewayv1.Listener) ([]gatewayv1.RouteGroupKind, bool) {
	supportedKinds := []gatewayv1.RouteGroupKind{}
	allKindsValid := true
	groupName := gatewayv1.Group(gatewayv1.GroupName)

	if listener.AllowedRoutes != nil && len(listener.AllowedRoutes.Kinds) > 0 {
		for _, kind := range listener.AllowedRoutes.Kinds {
			if (kind.Group == nil || *kind.Group == groupName) && (kind.Kind == "HTTPRoute" || kind.Kind == "GRPCRoute") {
				supportedKinds = append(supportedKinds, gatewayv1.RouteGroupKind{
					Group: &groupName,
					Kind:  kind.Kind,
				})
			} else {
				allKindsValid = false
			}
		}
	} else if listener.Protocol == gatewayv1.HTTPProtocolType || listener.Protocol == gatewayv1.HTTPSProtocolType {
		supportedKinds = append(supportedKinds,
			gatewayv1.RouteGroupKind{
				Group: &groupName,
				Kind:  "HTTPRoute",
			},
			gatewayv1.RouteGroupKind{
				Group: &groupName,
				Kind:  "GRPCRoute",
			})
	}

	return supportedKinds, allKindsValid
}

func (c *Controller) updateRouteStatuses(ctx context.Context, gw *gatewayv1.Gateway, programmedCondition metav1.Condition, httpRoutes []*gatewayv1.HTTPRoute, grpcRoutes []*gatewayv1.GRPCRoute) error {
	var errGroup []error
	for _, httpRoute := range httpRoutes {
		err := c.updateHTTPRouteStatus(ctx, httpRoute, gw, programmedCondition)
		if err != nil {
			errGroup = append(errGroup, err)
		}
	}
	for _, grpcRoute := range grpcRoutes {
		err := c.updateGRPCRouteStatus(ctx, grpcRoute, gw, programmedCondition)
		if err != nil {
			errGroup = append(errGroup, err)
		}
	}
	return errors.Join(errGroup...)
}

func (c *Controller) updateHTTPRouteStatus(ctx context.Context, route *gatewayv1.HTTPRoute, gw *gatewayv1.Gateway, programmed metav1.Condition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		newParentStatus := gatewayv1.RouteParentStatus{
			ParentRef:      gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gw.Name), Namespace: ptr.To(gatewayv1.Namespace(gw.Namespace))},
			ControllerName: controllerName,
			Conditions:     []metav1.Condition{},
		}

		// If the parent Gateway was successfully programmed, then the route is considered
		// Accepted and its references are Resolved.
		if programmed.Status == metav1.ConditionTrue {
			meta.SetStatusCondition(&newParentStatus.Conditions, metav1.Condition{
				Type:               string(gatewayv1.RouteConditionAccepted),
				Status:             metav1.ConditionTrue,
				Reason:             string(gatewayv1.RouteReasonAccepted),
				Message:            "Route is accepted",
				ObservedGeneration: route.Generation,
			})
			meta.SetStatusCondition(&newParentStatus.Conditions, metav1.Condition{
				Type:               string(gatewayv1.RouteConditionResolvedRefs),
				Status:             metav1.ConditionTrue,
				Reason:             string(gatewayv1.RouteReasonResolvedRefs),
				Message:            "All references resolved",
				ObservedGeneration: route.Generation,
			})
		} else {
			// If the parent Gateway was NOT successfully programmed, the route is not
			// considered Accepted and its references are not Resolved.
			meta.SetStatusCondition(&newParentStatus.Conditions, metav1.Condition{
				Type:               string(gatewayv1.RouteConditionAccepted),
				Status:             metav1.ConditionFalse,
				Reason:             "GatewayNotProgrammed",
				Message:            "Gateway failed to be programmed",
				ObservedGeneration: route.Generation,
			})
			meta.SetStatusCondition(&newParentStatus.Conditions, metav1.Condition{
				Type:               string(gatewayv1.RouteConditionResolvedRefs),
				Status:             metav1.ConditionFalse,
				Reason:             "GatewayNotReady",
				Message:            "Gateway is not ready, so references cannot be resolved",
				ObservedGeneration: route.Generation,
			})
		}

		newRoute := route.DeepCopy()
		found := false
		for i, parent := range newRoute.Status.Parents {
			if parent.ParentRef.Name == gatewayv1.ObjectName(gw.Name) && parent.ParentRef.Namespace != nil && *parent.ParentRef.Namespace == gatewayv1.Namespace(gw.Namespace) {
				newRoute.Status.Parents[i] = newParentStatus
				found = true
				break
			}
		}
		if !found {
			newRoute.Status.Parents = append(newRoute.Status.Parents, newParentStatus)
		}

		if !reflect.DeepEqual(route.Status, newRoute.Status) {
			if _, err := c.gwClient.GatewayV1().HTTPRoutes(route.Namespace).UpdateStatus(ctx, newRoute, metav1.UpdateOptions{}); err != nil {
				klog.Errorf("Failed to update HTTPRoute %s/%s status: %v", route.Namespace, route.Name, err)
				return err
			}
		}
		return nil
	})
}

func (c *Controller) updateGRPCRouteStatus(ctx context.Context, route *gatewayv1.GRPCRoute, gw *gatewayv1.Gateway, programmed metav1.Condition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		newParentStatus := gatewayv1.RouteParentStatus{
			ParentRef:      gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gw.Name), Namespace: ptr.To(gatewayv1.Namespace(gw.Namespace))},
			ControllerName: controllerName,
			Conditions:     []metav1.Condition{},
		}

		if programmed.Status == metav1.ConditionTrue {
			meta.SetStatusCondition(&newParentStatus.Conditions, metav1.Condition{
				Type:               string(gatewayv1.RouteConditionAccepted),
				Status:             metav1.ConditionTrue,
				Reason:             string(gatewayv1.RouteReasonAccepted),
				Message:            "Route is accepted",
				ObservedGeneration: route.Generation,
			})
		} else {
			meta.SetStatusCondition(&newParentStatus.Conditions, metav1.Condition{
				Type:               string(gatewayv1.RouteConditionAccepted),
				Status:             metav1.ConditionFalse,
				Reason:             "GatewayNotProgrammed",
				Message:            "Gateway failed to be programmed",
				ObservedGeneration: route.Generation,
			})
		}

		meta.SetStatusCondition(&newParentStatus.Conditions, metav1.Condition{
			Type:               string(gatewayv1.RouteConditionResolvedRefs),
			Status:             metav1.ConditionTrue,
			Reason:             string(gatewayv1.RouteReasonResolvedRefs),
			Message:            "All references resolved",
			ObservedGeneration: route.Generation,
		})

		newRoute := route.DeepCopy()
		found := false
		for i, parent := range newRoute.Status.Parents {
			if parent.ParentRef.Name == gatewayv1.ObjectName(gw.Name) && parent.ParentRef.Namespace != nil && *parent.ParentRef.Namespace == gatewayv1.Namespace(gw.Namespace) {
				newRoute.Status.Parents[i] = newParentStatus
				found = true
				break
			}
		}
		if !found {
			newRoute.Status.Parents = append(newRoute.Status.Parents, newParentStatus)
		}

		if !reflect.DeepEqual(route.Status, newRoute.Status) {
			if _, err := c.gwClient.GatewayV1().GRPCRoutes(route.Namespace).UpdateStatus(ctx, newRoute, metav1.UpdateOptions{}); err != nil {
				klog.Errorf("Failed to update GRPCRoute %s/%s status: %v", route.Namespace, route.Name, err)
				return err
			}
		}
		return nil
	})
}

func (c *Controller) getHTTPRoutesForListener(gw *gatewayv1.Gateway, listener gatewayv1.Listener) []*gatewayv1.HTTPRoute {
	var matchingRoutes []*gatewayv1.HTTPRoute
	httpRoutes, err := c.httprouteLister.List(labels.Everything())
	if err != nil {
		klog.Infof("failed to list HTTPRoutes: %v", err)
		return matchingRoutes
	}

	for _, route := range httpRoutes {
		if isRouteReferenced(gw, listener, route) && isRouteAllowed(gw, listener, route, c.namespaceLister) {
			matchingRoutes = append(matchingRoutes, route)
		}
	}
	return matchingRoutes
}

func (c *Controller) getGRPCRoutesForListener(gw *gatewayv1.Gateway, listener gatewayv1.Listener) []*gatewayv1.GRPCRoute {
	var matchingRoutes []*gatewayv1.GRPCRoute
	grpcRoutes, err := c.grpcrouteLister.List(labels.Everything())
	if err != nil {
		klog.Infof("failed to list GRPCRoutes: %v", err)
		return matchingRoutes
	}
	for _, route := range grpcRoutes {
		if isRouteReferenced(gw, listener, route) && isRouteAllowed(gw, listener, route, c.namespaceLister) {
			matchingRoutes = append(matchingRoutes, route)
		}
	}
	return matchingRoutes
}

func (c *Controller) translateBackendRefToCluster(defaultNamespace string, backendRef gatewayv1.BackendRef) (*clusterv3.Cluster, error) {
	ns := defaultNamespace
	if backendRef.Namespace != nil {
		ns = string(*backendRef.Namespace)
	}
	service, err := c.serviceLister.Services(ns).Get(string(backendRef.Name))
	if err != nil {
		return nil, fmt.Errorf("could not find service %s/%s: %w", ns, backendRef.Name, err)
	}

	clusterName, err := backendRefToClusterName(defaultNamespace, backendRef)
	if err != nil {
		return nil, err
	}
	return &clusterv3.Cluster{
		Name:                 clusterName,
		ConnectTimeout:       durationpb.New(5 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		LoadAssignment:       createClusterLoadAssignment(clusterName, service.Spec.ClusterIP, uint32(*backendRef.Port)),
	}, nil
}

func (c *Controller) deleteGatewayResources(ctx context.Context, name, namespace string) error {
	klog.Infof("Deleting resources for Gateway: %s/%s", namespace, name)
	containerName := gatewayName(c.clusterName, namespace, name)

	c.xdsVersion.Add(1)
	version := fmt.Sprintf("%d", c.xdsVersion.Load())

	snapshot, err := cachev3.NewSnapshot(version, map[resourcev3.Type][]envoyproxytypes.Resource{
		resourcev3.ListenerType: {},
		resourcev3.RouteType:    {},
		resourcev3.ClusterType:  {},
		resourcev3.EndpointType: {},
	})
	if err != nil {
		return fmt.Errorf("failed to create empty snapshot for deleted gateway %s: %w", name, err)
	}
	if err := c.xdscache.SetSnapshot(ctx, containerName, snapshot); err != nil {
		return fmt.Errorf("failed to set empty snapshot for deleted gateway %s: %w", name, err)
	}

	if err := container.Delete(containerName); err != nil {
		return fmt.Errorf("failed to delete container for gateway %s: %v", name, err)
	}

	klog.Infof("Successfully cleared resources for deleted Gateway: %s", name)
	return nil
}

func createClusterLoadAssignment(clusterName, serviceHost string, servicePort uint32) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{
				LbEndpoints: []*endpointv3.LbEndpoint{
					{
						HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
							Endpoint: &endpointv3.Endpoint{
								Address: &corev3.Address{
									Address: &corev3.Address_SocketAddress{
										SocketAddress: &corev3.SocketAddress{
											Address: serviceHost,
											PortSpecifier: &corev3.SocketAddress_PortValue{
												PortValue: servicePort,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// getRouteHostnames determines the effective hostnames for a route.
func getRouteHostnames(routeHostnames []gatewayv1.Hostname, listener gatewayv1.Listener) []string {
	if len(routeHostnames) > 0 {
		hostnames := make([]string, len(routeHostnames))
		for i, h := range routeHostnames {
			hostnames[i] = string(h)
		}
		return hostnames
	}
	if listener.Hostname != nil && *listener.Hostname != "" {
		return []string{string(*listener.Hostname)}
	}
	return []string{"*"}
}

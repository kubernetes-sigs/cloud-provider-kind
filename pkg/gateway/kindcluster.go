package gateway

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net/netip"
	"runtime"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

//go:embed routeadder/route-adder-amd64
var routeAdderAmd64 []byte

//go:embed routeadder/route-adder-arm64
var routeAdderArm64 []byte

func getRouteAdderBinaryForArch() ([]byte, error) {
	arch := runtime.GOARCH

	switch arch {
	case "amd64":
		return routeAdderAmd64, nil
	case "arm64":
		return routeAdderArm64, nil
	default:
		return nil, fmt.Errorf("unsupported architecture: %s", arch)
	}
}

// getClusterRoutingMap creates a unified map of all routes required for the container.
// It includes routes for each node's PodCIDRs and for the cluster's ServiceCIDRs.
func (c *Controller) getClusterRoutingMap(ctx context.Context) (map[string]string, error) {
	nodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list kubernetes nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("no kubernetes nodes found in the cluster")
	}

	routeMap := make(map[string]string)
	controlPlaneIPs := make(map[bool]string) // map[isIPv6]address

	// --- 1. Process all nodes for PodCIDR routes and find a control-plane IP ---
	for _, node := range nodes.Items {
		nodeIPs := make(map[bool]string) // map[isIPv6]address
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				ip, err := netip.ParseAddr(addr.Address)
				if err != nil {
					continue // Skip invalid IPs
				}
				nodeIPs[ip.Is6()] = addr.Address
			}
		}

		// If this is a control-plane node, save its IPs for service routes
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok && len(controlPlaneIPs) == 0 {
			controlPlaneIPs = nodeIPs
		}

		// Map PodCIDRs to this node's IPs, matching family
		for _, podCIDR := range node.Spec.PodCIDRs {
			prefix, err := netip.ParsePrefix(podCIDR)
			if err != nil {
				continue
			}
			if gatewayIP, found := nodeIPs[prefix.Addr().Is6()]; found {
				routeMap[podCIDR] = gatewayIP
			}
		}
	}

	if len(controlPlaneIPs) == 0 {
		return nil, fmt.Errorf("could not find any control-plane node to use as a gateway for services")
	}

	// --- 2. Get ServiceCIDRs and add them to the map ---
	serviceCIDRs, err := c.getServiceCIDRs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get service CIDRs: %w", err)
	}

	for _, serviceCIDR := range serviceCIDRs {
		l := c.logger.WithValues("serviceCIDR", serviceCIDR)
		prefix, err := netip.ParsePrefix(serviceCIDR)
		if err != nil {
			l.Info("Invalid ServiceCIDR, skipping")
			continue
		}
		// Match the service CIDR family to the control-plane IP family
		if gatewayIP, found := controlPlaneIPs[prefix.Addr().Is6()]; found {
			l.Info("Found route for services", "gatewayIP", gatewayIP)
			routeMap[serviceCIDR] = gatewayIP
		} else {
			l.Info("No matching control-plane IP found for ServiceCIDR family")
		}
	}

	if len(routeMap) == 0 {
		return nil, fmt.Errorf("could not construct any valid routes")
	}

	return routeMap, nil
}

func (c *Controller) getServiceCIDRs(ctx context.Context) ([]string, error) {
	serviceCIDRs, err := c.client.NetworkingV1().ServiceCIDRs().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list servicecidrs: %w. Is the ServiceCIDR feature gate enabled?", err)
	}
	if len(serviceCIDRs.Items) == 0 {
		return nil, fmt.Errorf("no servicecidrs found in the cluster")
	}

	cidrs := sets.Set[string]{}
	for _, serviceCIDRObject := range serviceCIDRs.Items {
		cidrs.Insert(serviceCIDRObject.Spec.CIDRs...)
	}
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("no CIDRs found in any ServiceCIDR object")
	}
	return cidrs.UnsortedList(), nil
}

func (c *Controller) configureContainerNetworking(ctx context.Context, containerName string) error {
	l := c.logger.WithValues("container", containerName)
	binaryData, err := getRouteAdderBinaryForArch()
	if err != nil {
		return err
	}

	containerBinaryPath := "/tmp/route-adder"

	// 2. Combine copy and chmod into a single Exec call.
	// The shell command 'cat > file && chmod +x file' does both steps sequentially.
	setupCmd := []string{"sh", "-c", fmt.Sprintf("cat > %s && chmod +x %s", containerBinaryPath, containerBinaryPath)}
	stdinReader := bytes.NewReader(binaryData)

	l.Info("Streaming and setting up route-adder utility")
	if err := container.Exec(containerName, setupCmd, stdinReader, nil, nil); err != nil {
		return fmt.Errorf("failed to setup route-adder binary in container %s: %w", containerName, err)
	}
	l.Info("Successfully installed route-adder utility")
	routeMap, err := c.getClusterRoutingMap(ctx)
	if err != nil {
		return fmt.Errorf("failed to get kubernetes cluster routing information: %w", err)
	}

	// 3. Iterate through the map and add a route for each entry.
	var routesAdded int
	var stdout, stderr bytes.Buffer
	for cidr, gatewayIP := range routeMap {
		cmd := []string{containerBinaryPath, cidr, gatewayIP}
		l.Info("Adding route to container", "routes", strings.Join(cmd, " "))

		stdout.Reset()
		stderr.Reset()
		if err := container.Exec(containerName, cmd, nil, &stdout, &stderr); err != nil {
			return fmt.Errorf("failed to add route '%s' via %s to container %s: %w, stderr: %s",
				cidr, gatewayIP, containerName, err, stderr.String())
		}
		routesAdded++
	}

	if routesAdded == 0 {
		return fmt.Errorf("no valid cluster routes were found to configure")
	}

	l.Info("Successfully added service routes to container", "routes", routesAdded)
	return nil
}

func (c *Controller) ensureGatewayContainer(ctx context.Context, gw *gatewayv1.Gateway) error {
	namespace := gw.Namespace
	name := gw.Name
	containerName := gatewayName(c.clusterName, namespace, name)
	l := c.logger.WithValues("container", containerName)

	if !container.IsRunning(containerName) {
		l.Info("container gateway is not running", "gateway", klog.KRef(namespace, name))
		if container.Exist(containerName) {
			if err := container.Delete(containerName); err != nil {
				return err
			}
		}
	}
	if !container.Exist(containerName) {
		l.V(2).Info("creating container for gateway", "gateway", klog.KRef(namespace, name), "cluster", c.clusterName)
		enableTunnels := c.tunnelManager != nil || config.DefaultConfig.LoadBalancerConnectivity == config.Portmap
		err := createGateway(l, c.clusterName, c.clusterNameserver, c.xdsLocalAddress, c.xdsLocalPort, gw, enableTunnels)
		if err != nil {
			return err
		}

		// TODO fix this hack
		time.Sleep(250 * time.Millisecond)

		if err := c.configureContainerNetworking(ctx, containerName); err != nil {
			if delErr := container.Delete(containerName); delErr != nil {
				l.Error(err, "Failed to delete container after networking setup failed")
			}
			return fmt.Errorf("failed to configure networking for new gateway container %s: %w", containerName, err)
		}
	}
	return nil
}

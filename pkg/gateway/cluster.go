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

func (c *Controller) getClusterNodeIP(ctx context.Context) (string, error) {
	nodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return "", fmt.Errorf("failed to list kubernetes nodes: %w", err)
	}

	var nodeIP string
	for _, node := range nodes.Items {
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			for _, addr := range node.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP {
					nodeIP = addr.Address
					break
				}
			}
		}
		if nodeIP != "" {
			break
		}
	}
	if nodeIP == "" {
		for _, addr := range nodes.Items[0].Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				nodeIP = addr.Address
				break
			}
		}
	}
	if nodeIP == "" {
		return "", fmt.Errorf("no internal IP found for any node to use as a gateway")
	}
	return nodeIP, nil
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
	binaryData, err := getRouteAdderBinaryForArch()
	if err != nil {
		return err
	}

	containerBinaryPath := "/tmp/route-adder"

	// 2. Combine copy and chmod into a single Exec call.
	// The shell command 'cat > file && chmod +x file' does both steps sequentially.
	setupCmd := []string{"sh", "-c", fmt.Sprintf("cat > %s && chmod +x %s", containerBinaryPath, containerBinaryPath)}
	stdinReader := bytes.NewReader(binaryData)

	klog.Infof("Streaming and setting up route-adder utility in %s", containerName)
	if err := container.Exec(containerName, setupCmd, stdinReader, nil, nil); err != nil {
		return fmt.Errorf("failed to setup route-adder binary in container %s: %w", containerName, err)
	}
	klog.Infof("Successfully installed route-adder utility in container %s", containerName)

	nodeIP, err := c.getClusterNodeIP(ctx)
	if err != nil {
		return err
	}
	nodeAddr, err := netip.ParseAddr(nodeIP)
	if err != nil {
		return err
	}

	serviceCIDRs, err := c.getServiceCIDRs(ctx)
	if err != nil {
		return fmt.Errorf("failed to list servicecidrs: %w", err)
	}
	if len(serviceCIDRs) == 0 {
		return fmt.Errorf("no servicecidrs found in the cluster")
	}

	var routesAdded int
	var stdout, stderr bytes.Buffer
	for _, cidr := range serviceCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return fmt.Errorf("failed to parse CIDR %s: %w", cidr, err)
		}
		if prefix.Addr().Is4() != nodeAddr.Is4() {
			continue
		}
		cmd := []string{containerBinaryPath, cidr, nodeIP}
		klog.Infof("Adding route to container %s: %s", containerName, strings.Join(cmd, " "))

		stdout.Reset()
		stderr.Reset()

		if err := container.Exec(containerName, cmd, nil, &stdout, &stderr); err != nil {
			return fmt.Errorf("failed to add route '%s' via %s to container %s: %w, stderr: %s",
				cidr, nodeIP, containerName, err, stderr.String())
		}
		routesAdded++
	}

	if routesAdded == 0 {
		return fmt.Errorf("no valid service CIDRs found to configure routing")
	}

	return nil
}

func (c *Controller) ensureGatewayContainer(ctx context.Context, gw *gatewayv1.Gateway) error {
	namespace := gw.Namespace
	name := gw.Name
	containerName := gatewayName(c.clusterName, namespace, name)

	if !container.IsRunning(containerName) {
		klog.Infof("container %s for gateway %s/%s is not running", containerName, namespace, name)
		if container.Exist(containerName) {
			if err := container.Delete(containerName); err != nil {
				return err
			}
		}
	}
	if !container.Exist(containerName) {
		klog.V(2).Infof("creating container %s for gateway  %s/%s on cluster %s", containerName, namespace, name, c.clusterName)
		enableTunnels := c.tunnelManager != nil || config.DefaultConfig.LoadBalancerConnectivity == config.Portmap
		err := createGateway(c.clusterName, c.xdsLocalAddress, c.xdsLocalPort, gw, enableTunnels)
		if err != nil {
			return err
		}

		// TODO fix this hack
		time.Sleep(250 * time.Millisecond)

		if err := c.configureContainerNetworking(ctx, containerName); err != nil {
			if delErr := container.Delete(containerName); delErr != nil {
				klog.Errorf("failed to delete container %s after networking setup failed: %v", containerName, delErr)
			}
			return fmt.Errorf("failed to configure networking for new gateway container %s: %w", containerName, err)
		}
	}
	return nil
}

package gateway

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	"sigs.k8s.io/cloud-provider-kind/pkg/images"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// gatewayName name is a unique name for the gateway container
func gatewayName(clusterName, namespace, name string) string {
	h := sha256.New()
	_, err := io.WriteString(h, gatewaySimpleName(clusterName, namespace, name))
	if err != nil {
		panic(err)
	}
	hash := h.Sum(nil)
	return fmt.Sprintf("%s-%x", constants.ContainerPrefix, hash[:6])
}

func gatewaySimpleName(clusterName, namespace, name string) string {
	return clusterName + "/" + namespace + "/" + name
}

// createGateway create a docker container with a gateway
func createGateway(clusterName string, localAddress string, localPort int, gateway *gatewayv1.Gateway, enableTunnel bool) error {
	name := gatewayName(clusterName, gateway.Namespace, gateway.Name)
	simpleName := gatewaySimpleName(clusterName, gateway.Namespace, gateway.Name)
	envoyConfig := &configData{
		Cluster:             simpleName,
		ID:                  name,
		AdminPort:           envoyAdminPort,
		ControlPlaneAddress: localAddress,
		ControlPlanePort:    localPort,
	}
	dynamicFilesystemConfig, err := generateEnvoyConfig(envoyConfig)
	if err != nil {
		return err
	}
	networkName := constants.FixedNetworkName
	if n := os.Getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK"); n != "" {
		networkName = n
	}

	args := []string{
		"--detach", // run the container detached
		"--tty",    // allocate a tty for entrypoint logs
		// label the node with the cluster ID
		"--label", fmt.Sprintf("%s=%s", constants.NodeCCMLabelKey, clusterName),
		// label the node with the load balancer name
		"--label", fmt.Sprintf("%s=%s", constants.GatewayNameLabelKey, simpleName),
		// user a user defined docker network so we get embedded DNS
		"--net", networkName,
		"--init=false",
		"--hostname", name, // make hostname match container name
		// label the node with the role ID
		// running containers in a container requires privileged
		// NOTE: we could try to replicate this with --cap-add, and use less
		// privileges, but this flag also changes some mounts that are necessary
		// including some ones docker would otherwise do by default.
		// for now this is what we want. in the future we may revisit this.
		"--privileged",
		"--restart=on-failure",                           // to deal with the crash casued by https://github.com/envoyproxy/envoy/issues/34195
		"--sysctl=net.ipv4.ip_forward=1",                 // allow ip forwarding
		"--sysctl=net.ipv4.conf.all.rp_filter=0",         // disable rp filter
		"--sysctl=net.ipv4.ip_unprivileged_port_start=1", // Allow lower port numbers for podman (see https://github.com/containers/podman/blob/main/rootless.md for more info)
	}

	// allow IPv6 always
	args = append(args, []string{
		"--sysctl=net.ipv6.conf.all.disable_ipv6=0", // enable IPv6
		"--sysctl=net.ipv6.conf.all.forwarding=1",   // allow ipv6 forwarding})
	}...)

	if enableTunnel {
		// Forward the Service Ports to the host so they are accessible on Mac and Windows
		for _, listener := range gateway.Spec.Listeners {
			if listener.Protocol == gatewayv1.UDPProtocolType {
				args = append(args, fmt.Sprintf("--publish=%d/%s", listener.Port, "udp"))
			} else {
				args = append(args, fmt.Sprintf("--publish=%d/%s", listener.Port, "tcp"))
			}
		}
	}
	// publish the admin endpoint
	args = append(args, fmt.Sprintf("--publish=%d/tcp", envoyAdminPort))
	// Publish all ports in the host in random ports
	args = append(args, "--publish-all")

	args = append(args, images.Images["proxy"])
	// we need to override the default envoy configuration
	// https://www.envoyproxy.io/docs/envoy/latest/start/quick-start/configuration-dynamic-filesystem
	// envoy crashes in some circumstances, causing the container to restart, the problem is that the container
	// may come with a different IP and we don't update the status, we may do it, but applications does not use
	// to handle that the assigned LoadBalancerIP changes.
	// https://github.com/envoyproxy/envoy/issues/34195
	cmd := []string{"bash", "-c",
		fmt.Sprintf(`echo -en '%s' > %s && while true; do envoy -c %s && break; sleep 1; done`,
			dynamicFilesystemConfig, proxyConfigPath, proxyConfigPath)}
	args = append(args, cmd...)
	klog.V(2).Infof("creating gateway with parameters: %v", args)
	err = container.Create(name, args)
	if err != nil {
		return fmt.Errorf("failed to create continers %s %v: %w", name, args, err)
	}

	return nil
}

func (c *Controller) processNextGatewayItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.gatewayqueue.Get()
	if quit {
		return false
	}
	defer c.gatewayqueue.Done(key)

	err := c.syncGateway(key)

	c.handleGatewayErr(err, key)
	return true
}

// syncToStdout is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *Controller) syncGateway(key string) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing gateway %q (%v)", key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	gw, err := c.gatewayLister.Gateways(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}
	containerName := gatewayName(c.clusterName, namespace, name)

	if apierrors.IsNotFound(err) {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		klog.Infof("Gateway %s does not exist anymore, deleting \n", key)
		err := container.Delete(containerName)
		if err != nil {
			return fmt.Errorf("can not delete container %s for gateway %s/%s on cluster %s : %v", containerName, namespace, name, c.clusterName, err)
		}
	} else {
		klog.Infof("Syncing Gateway %s\n", gw.GetName())
		if !container.IsRunning(containerName) {
			klog.Infof("container %s for gateway is not running", name)
			if container.Exist(containerName) {
				err := container.Delete(containerName)
				if err != nil {
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
		}

	}
	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleGatewayErr(err error, key string) {
	if err == nil {
		c.gatewayqueue.Forget(key)
		return
	}

	if c.gatewayqueue.NumRequeues(key) < maxRetries {
		klog.Infof("Error syncing Gateway %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.gatewayqueue.AddRateLimited(key)
		return
	}

	c.gatewayqueue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping Gateway %q out of the queue: %v", key, err)
}

func (c *Controller) runGatewayWorker(ctx context.Context) {
	for c.processNextGatewayItem() {
	}
}

package loadbalancer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	"sigs.k8s.io/cloud-provider-kind/pkg/tunnels"
)

type Server struct {
	tunnelManager *tunnels.TunnelManager
}

var _ cloudprovider.LoadBalancer = &Server{}

func NewServer() cloudprovider.LoadBalancer {
	s := &Server{}

	if config.DefaultConfig.LoadBalancerConnectivity == config.Tunnel {
		s.tunnelManager = tunnels.NewTunnelManager()
	}
	return s
}

func (s *Server) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	// report status
	name := loadBalancerName(clusterName, service)
	ipv4, ipv6, err := container.IPs(name)
	if err != nil {
		if strings.Contains(err.Error(), "failed to get container details") {
			return nil, false, nil
		}
		return nil, false, err
	}
	status := &v1.LoadBalancerStatus{}

	// process Ports
	portStatus := []v1.PortStatus{}
	for _, port := range service.Spec.Ports {
		portStatus = append(portStatus, v1.PortStatus{
			Port:     port.Port,
			Protocol: port.Protocol,
		})
	}

	// process IPs
	svcIPv4 := false
	svcIPv6 := false
	for _, family := range service.Spec.IPFamilies {
		if family == v1.IPv4Protocol {
			svcIPv4 = true
		}
		if family == v1.IPv6Protocol {
			svcIPv6 = true
		}
	}
	if ipv4 != "" && svcIPv4 {
		status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{
			IP:     ipv4,
			IPMode: ptr.To(v1.LoadBalancerIPModeProxy),
			Ports:  portStatus,
		})
	}
	if ipv6 != "" && svcIPv6 {
		status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{
			IP:     ipv6,
			IPMode: ptr.To(v1.LoadBalancerIPModeProxy),
			Ports:  portStatus,
		})
	}

	return status, true, nil
}

func (s *Server) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return loadBalancerName(clusterName, service)
}

func (s *Server) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	name := loadBalancerName(clusterName, service)
	if !container.IsRunning(name) {
		klog.Infof("container %s for loadbalancer is not running", name)
		if container.Exist(name) {
			err := container.Delete(name)
			if err != nil {
				return nil, err
			}
		}
	}
	if !container.Exist(name) {
		klog.V(2).Infof("creating container for loadbalancer")
		err := s.createLoadBalancer(clusterName, service, config.DefaultConfig.ProxyImage)
		if err != nil {
			return nil, err
		}
	}

	// update loadbalancer
	klog.V(2).Infof("updating loadbalancer")
	err := s.UpdateLoadBalancer(ctx, clusterName, service, nodes)
	if err != nil {
		return nil, err
	}

	// on some platforms that run containers in VMs forward from userspace
	if s.tunnelManager != nil {
		klog.V(2).Infof("updating loadbalancer tunnels on userspace")
		err = s.tunnelManager.SetupTunnels(loadBalancerName(clusterName, service))
		if err != nil {
			klog.ErrorS(err, "error setting up tunnels")
		}
	}

	// get loadbalancer Status
	klog.V(2).Infof("get loadbalancer status")
	status, ok, err := s.GetLoadBalancer(ctx, clusterName, service)
	if !ok {
		return nil, fmt.Errorf("loadbalancer %s not found", name)
	}
	if err != nil {
		return nil, err
	}
	return status, nil
}

func (s *Server) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	return proxyUpdateLoadBalancer(ctx, clusterName, service, nodes)
}

func (s *Server) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	containerName := loadBalancerName(clusterName, service)
	var err1, err2 error
	if s.tunnelManager != nil {
		err1 = s.tunnelManager.RemoveTunnels(containerName)
	}
	// Before deleting the load balancer store the logs if required
	if config.DefaultConfig.EnableLogDump {
		fileName := path.Join(config.DefaultConfig.LogDir, service.Namespace+"_"+service.Name+".log")
		klog.V(2).Infof("storing logs for loadbalancer %s on %s", containerName, fileName)
		if err := container.LogDump(containerName, fileName); err != nil {
			klog.Infof("error trying to store logs for load balancer %s : %v", containerName, err)
		}
	}
	err2 = container.Delete(containerName)
	return errors.Join(err1, err2)
}

// loadbalancer name is a unique name for the loadbalancer container
func loadBalancerName(clusterName string, service *v1.Service) string {
	h := sha256.New()
	_, err := io.WriteString(h, loadBalancerSimpleName(clusterName, service))
	if err != nil {
		panic(err)
	}
	hash := h.Sum(nil)
	return fmt.Sprintf("%s-%x", constants.ContainerPrefix, hash[:6])
}

func loadBalancerSimpleName(clusterName string, service *v1.Service) string {
	return clusterName + "/" + service.Namespace + "/" + service.Name
}

func ServiceFromLoadBalancerSimpleName(s string) (clusterName string, service *v1.Service) {
	slices := strings.Split(s, "/")
	if len(slices) != 3 {
		return
	}
	clusterName = slices[0]
	service = &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: slices[1], Name: slices[2]}}
	return
}

// createLoadBalancer create a docker container with a loadbalancer
func (s *Server) createLoadBalancer(clusterName string, service *v1.Service, image string) error {
	name := loadBalancerName(clusterName, service)

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
		"--label", fmt.Sprintf("%s=%s", constants.LoadBalancerNameLabelKey, loadBalancerSimpleName(clusterName, service)),
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

	if isIPv6Service(service) {
		args = append(args, []string{
			"--sysctl=net.ipv6.conf.all.disable_ipv6=0", // enable IPv6
			"--sysctl=net.ipv6.conf.all.forwarding=1",   // allow ipv6 forwarding})
		}...)
	}

	if s.tunnelManager != nil ||
		config.DefaultConfig.LoadBalancerConnectivity == config.Portmap {
		// Forward the Service Ports to the host so they are accessible on Mac and Windows
		for _, port := range service.Spec.Ports {
			if port.Protocol != v1.ProtocolTCP && port.Protocol != v1.ProtocolUDP {
				continue
			}
			args = append(args, fmt.Sprintf("--publish=%d/%s", port.Port, port.Protocol))
		}
	}
	// publish the admin endpoint
	args = append(args, fmt.Sprintf("--publish=%d/%s", envoyAdminPort, v1.ProtocolTCP))
	// Publish all ports in the host in random ports
	args = append(args, "--publish-all")

	if service.Spec.LoadBalancerIP != "" {
		args = append(args, "--ip", service.Spec.LoadBalancerIP)
	}

	args = append(args, image)
	// we need to override the default envoy configuration
	// https://www.envoyproxy.io/docs/envoy/latest/start/quick-start/configuration-dynamic-filesystem
	// envoy crashes in some circumstances, causing the container to restart, the problem is that the container
	// may come with a different IP and we don't update the status, we may do it, but applications does not use
	// to handle that the assigned LoadBalancerIP changes.
	// https://github.com/envoyproxy/envoy/issues/34195
	cmd := []string{"bash", "-c",
		fmt.Sprintf(`echo -en '%s' > %s && touch %s && touch %s && while true; do envoy -c %s && break; sleep 1; done`,
			dynamicFilesystemConfig, proxyConfigPath, proxyConfigPathCDS, proxyConfigPathLDS, proxyConfigPath)}
	args = append(args, cmd...)
	klog.V(2).Infof("creating loadbalancer with parameters: %v", args)
	err := container.Create(name, args)
	if err != nil {
		return fmt.Errorf("failed to create continers %s %v: %w", name, args, err)
	}

	return nil
}

func isIPv6Service(service *v1.Service) bool {
	if service == nil {
		return false
	}
	for _, family := range service.Spec.IPFamilies {
		if family == v1.IPv6Protocol {
			return true
		}
	}
	return false
}

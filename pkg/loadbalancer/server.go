package loadbalancer

import (
	"context"
	"fmt"
	"os"
	"strings"

	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	"sigs.k8s.io/kind/pkg/cluster/constants"
)

type Server struct {
	cache map[string]bool
}

var _ cloudprovider.LoadBalancer = &Server{}

func NewServer() cloudprovider.LoadBalancer {
	return &Server{
		cache: map[string]bool{},
	}
}

func (s *Server) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	// report status
	name := loadBalancerName(clusterName, service)
	ipv4, ipv6, err := containerIPs(name)
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
		status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{IP: ipv4, Ports: portStatus})
	}
	if ipv6 != "" && svcIPv6 {
		status.Ingress = append(status.Ingress, v1.LoadBalancerIngress{IP: ipv6, Ports: portStatus})
	}

	return status, true, nil
}

func (s *Server) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return loadBalancerName(clusterName, service)
}

func (s *Server) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	name := loadBalancerName(clusterName, service)
	if !containerIsRunning(name) {
		if containerExist(name) {
			err := deleteContainer(name)
			if err != nil {
				return nil, err
			}
		}
	}
	if !containerExist(name) {
		klog.V(2).Infof("creating container for loadbalancer")
		err := createLoadBalancer(clusterName, service, proxyImage)
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
	return deleteContainer(loadBalancerName(clusterName, service))
}

// loadbalancer name = cluster-name + service.namespace + service.name
func loadBalancerName(clusterName string, service *v1.Service) string {
	return "kindlb-" + clusterName + "-" + service.Namespace + "-" + service.Name
}

// createLoadBalancer create a docker container with a loadbalancer
func createLoadBalancer(clusterName string, service *v1.Service, image string) error {
	name := loadBalancerName(clusterName, service)

	networkName := fixedNetworkName
	if n := os.Getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK"); n != "" {
		networkName = n
	}

	args := []string{
		"--detach", // run the container detached
		"--tty",    // allocate a tty for entrypoint logs
		// label the node with the cluster ID
		"--label", fmt.Sprintf("%s=%s", clusterLabelKey, clusterName),
		"--label", fmt.Sprintf("%s=%s", nodeRoleLabelKey, constants.ExternalLoadBalancerNodeRoleValue),
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
		"--restart=on-failure:1",                    // to allow to change the configuration
		"--sysctl=net.ipv4.ip_forward=1",            // allow ip forwarding
		"--sysctl=net.ipv6.conf.all.disable_ipv6=0", // enable IPv6
		"--sysctl=net.ipv6.conf.all.forwarding=1",   // allow ipv6 forwarding
		"--sysctl=net.ipv4.conf.all.rp_filter=0",    // disable rp filter
		image,
	}

	err := createContainer(name, args)
	if err != nil {
		return fmt.Errorf("failed to create continers %s %v: %w", name, args, err)
	}

	return nil
}

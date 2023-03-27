package provider

import (
	"context"

	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

var _ cloudprovider.LoadBalancer = &cloud{}

// GetLoadBalancer returns whether the specified load balancer exists, and if so, what its status is.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (c *cloud) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	klog.V(2).Infof("Get LoadBalancer cluster: %s service: %s", clusterName, service.Name)
	return c.lbController.GetLoadBalancer(ctx, clusterName, service)
}

// GetLoadBalancerName returns the name of the load balancer.
func (c *cloud) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	klog.V(2).Infof("Get LoadBalancerNmae cluster: %s service: %s", clusterName, service.Name)
	return c.lbController.GetLoadBalancerName(ctx, clusterName, service)
}

// EnsureLoadBalancer creates a new load balancer 'name', or updates the existing one. Returns the status of the balancer
func (c *cloud) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	klog.V(2).Infof("Ensure LoadBalancer cluster: %s service: %s", clusterName, service.Name)
	return c.lbController.EnsureLoadBalancer(ctx, clusterName, service, nodes)
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
func (c *cloud) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	klog.V(2).Infof("Update LoadBalancer cluster: %s service: %s", clusterName, service.Name)
	return c.lbController.UpdateLoadBalancer(ctx, clusterName, service, nodes)
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it
// exists, returning nil if the load balancer specified either didn't exist or
// was successfully deleted.
func (c *cloud) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	klog.V(2).Infof("Ensure LoadBalancer deleted cluster: %s service: %s", clusterName, service.Name)
	return c.lbController.EnsureLoadBalancerDeleted(ctx, clusterName, service)
}

package provider

import (
	"io"

	"sigs.k8s.io/cloud-provider-kind/cmd/app"
	"sigs.k8s.io/cloud-provider-kind/pkg/loadbalancer"

	cloudprovider "k8s.io/cloud-provider"

	"sigs.k8s.io/kind/pkg/cluster"
	kindcmd "sigs.k8s.io/kind/pkg/cmd"
	"sigs.k8s.io/kind/pkg/log"
)

const (
	ProviderName = "kind"
)

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		// TODO get this from the flags
		logger := kindcmd.NewLogger()
		type verboser interface {
			SetVerbosity(log.Level)
		}
		v, ok := logger.(verboser)
		if ok {
			v.SetVerbosity(5)
		}

		provider := cluster.NewProvider(
			cluster.ProviderWithLogger(logger),
		)
		return &cloud{
			kindClient:  provider,
			clusterName: app.ClusterName,
		}, nil
	})
}

var _ cloudprovider.Interface = (*cloud)(nil)

// controller is the KIND implementation of the cloud provider interface
type cloud struct {
	clusterName  string // name of the kind cluster
	kindClient   *cluster.Provider
	lbController cloudprovider.LoadBalancer
}

// Initialize passes a Kubernetes clientBuilder interface to the cloud provider
func (c *cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stopCh <-chan struct{}) {
	c.clusterName = app.ClusterName
	c.lbController = loadbalancer.NewServer()
}

// Clusters returns the list of clusters.
func (c *cloud) Clusters() (cloudprovider.Clusters, bool) {
	return c, true
}

// ProviderName returns the cloud provider ID.
func (c *cloud) ProviderName() string {
	return ProviderName
}

func (c *cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return c, true
}

func (c *cloud) Instances() (cloudprovider.Instances, bool) {
	return nil, false
}

func (c *cloud) Zones() (cloudprovider.Zones, bool) {
	return nil, false
}

func (c *cloud) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

func (c *cloud) HasClusterID() bool {
	return len(c.clusterName) > 0
}

func (c *cloud) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return c, true
}

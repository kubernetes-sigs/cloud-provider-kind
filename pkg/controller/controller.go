package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	cloudprovider "k8s.io/cloud-provider"
	nodecontroller "k8s.io/cloud-provider/controllers/node"
	servicecontroller "k8s.io/cloud-provider/controllers/service"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	ccmfeatures "k8s.io/controller-manager/pkg/features"
	"k8s.io/klog/v2"
	cpkconfig "sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	"sigs.k8s.io/cloud-provider-kind/pkg/loadbalancer"
	"sigs.k8s.io/cloud-provider-kind/pkg/provider"
	"sigs.k8s.io/kind/pkg/cluster"
)

var once sync.Once

type Controller struct {
	kind     *cluster.Provider
	clusters map[string]*ccm
}

type ccm struct {
	factory           informers.SharedInformerFactory
	serviceController *servicecontroller.Controller
	nodeController    *nodecontroller.CloudNodeController
	cancelFn          context.CancelFunc
}

func New(provider *cluster.Provider) *Controller {
	controllersmetrics.Register()
	return &Controller{
		kind:     provider,
		clusters: make(map[string]*ccm),
	}
}

func (c *Controller) Run(ctx context.Context) {
	defer c.cleanup()
	for {
		// get existing kind clusters
		clusters, err := c.kind.List()
		if err != nil {
			klog.Infof("error listing clusters, retrying ...: %v", err)
		}

		// add new ones
		for _, cluster := range clusters {
			select {
			case <-ctx.Done():
				return
			default:
			}

			klog.V(3).Infof("processing cluster %s", cluster)
			_, ok := c.clusters[cluster]
			if ok {
				klog.V(3).Infof("cluster %s already exist", cluster)
				continue
			}

			kubeClient, err := c.getKubeClient(ctx, cluster)
			if err != nil {
				klog.Errorf("Failed to create kubeClient for cluster %s: %v", cluster, err)
				continue
			}

			klog.V(2).Infof("Creating new cloud provider for cluster %s", cluster)
			cloud := provider.New(cluster, c.kind)
			ccm, err := startCloudControllerManager(ctx, cluster, kubeClient, cloud)
			if err != nil {
				klog.Errorf("Failed to start cloud controller for cluster %s: %v", cluster, err)
				continue
			}
			klog.Infof("Starting cloud controller for cluster %s", cluster)
			c.clusters[cluster] = ccm
		}
		// remove expired ones
		clusterSet := sets.New(clusters...)
		for cluster, ccm := range c.clusters {
			_, ok := clusterSet[cluster]
			if !ok {
				klog.Infof("Deleting resources for cluster %s", cluster)
				ccm.cancelFn()
				delete(c.clusters, cluster)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// getKubeClient returns a kubeclient for the cluster passed as argument
// It tries first to connect to the internal endpoint.
func (c *Controller) getKubeClient(ctx context.Context, cluster string) (kubernetes.Interface, error) {
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	// prefer internal (direct connectivity) over no-internal (commonly portmap)
	for _, internal := range []bool{true, false} {
		kconfig, err := c.kind.KubeConfig(cluster, internal)
		if err != nil {
			klog.Errorf("Failed to get kubeconfig for cluster %s: %v", cluster, err)
			continue
		}

		config, err := clientcmd.RESTConfigFromKubeConfig([]byte(kconfig))
		if err != nil {
			klog.Errorf("Failed to convert kubeconfig for cluster %s: %v", cluster, err)
			continue
		}

		// check that the apiserver is reachable before continue
		// to fail fast and avoid waiting until the client operations timeout
		var ok bool
		for i := 0; i < 5; i++ {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			if probeHTTP(ctx, httpClient, config.Host) {
				ok = true
				break
			}
			time.Sleep(time.Second * time.Duration(i))
		}
		if !ok {
			klog.Errorf("Failed to connect to apiserver %s: %v", cluster, err)
			continue
		}

		kubeClient, err := kubernetes.NewForConfig(config)
		if err != nil {
			klog.Errorf("Failed to create kubeClient for cluster %s: %v", cluster, err)
			continue
		}
		// the first cluster will give us the type of connectivity between
		// cloud-provider-kind and the clusters and load balancer containers.
		// In Linux or containerized cloud-provider-kind this will be direct.
		once.Do(func() {
			if internal {
				cpkconfig.DefaultConfig.ControlPlaneConnectivity = cpkconfig.Direct
			}
		})
		return kubeClient, err
	}
	return nil, fmt.Errorf("can not find a working kubernetes clientset")
}

func probeHTTP(ctx context.Context, client *http.Client, address string) bool {
	klog.Infof("probe HTTP address %s", address)
	req, err := http.NewRequest("GET", address, nil)
	if err != nil {
		return false
	}
	req = req.WithContext(ctx)
	resp, err := client.Do(req)
	if err != nil {
		klog.Infof("Failed to connect to HTTP address %s: %v", address, err)
		return false
	}
	defer resp.Body.Close()
	// drain the body
	io.ReadAll(resp.Body) // nolint:errcheck
	// we only want to verify connectivity so don't need to check the http status code
	// as the apiserver may not be ready
	return true
}

// TODO: implement leader election to not have problems with  multiple providers
// ref: https://github.com/kubernetes/kubernetes/blob/d97ea0f705847f90740cac3bc3dd8f6a4026d0b5/cmd/kube-scheduler/app/server.go#L211
func startCloudControllerManager(ctx context.Context, clusterName string, kubeClient kubernetes.Interface, cloud cloudprovider.Interface) (*ccm, error) {
	// TODO: we need to set up the ccm specific feature gates
	// but try to avoid to expose this to users
	featureGates := utilfeature.DefaultMutableFeatureGate
	err := ccmfeatures.SetupCurrentKubernetesSpecificFeatureGates(featureGates)
	if err != nil {
		return nil, err
	}

	client := kubeClient.Discovery().RESTClient()
	// wait for health
	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		healthStatus := 0
		client.Get().AbsPath("/healthz").Do(ctx).StatusCode(&healthStatus)
		if healthStatus != http.StatusOK {
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		klog.Errorf("Failed waiting for apiserver to be ready: %v", err)
		return nil, err
	}

	sharedInformers := informers.NewSharedInformerFactory(kubeClient, 60*time.Second)

	ccmMetrics := controllersmetrics.NewControllerManagerMetrics(clusterName)
	// Start the service controller
	serviceController, err := servicecontroller.New(
		cloud,
		kubeClient,
		sharedInformers.Core().V1().Services(),
		sharedInformers.Core().V1().Nodes(),
		clusterName,
		featureGates,
	)
	if err != nil {
		// This error shouldn't fail. It lives like this as a legacy.
		klog.Errorf("Failed to start service controller: %v", err)
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	go serviceController.Run(ctx, 5, ccmMetrics)

	// Start the node controller
	nodeController, err := nodecontroller.NewCloudNodeController(
		sharedInformers.Core().V1().Nodes(),
		kubeClient,
		cloud,
		30*time.Second,
		5, // workers
	)
	if err != nil {
		// This error shouldn't fail. It lives like this as a legacy.
		klog.Errorf("Failed to start node controller: %v", err)
		cancel()
		return nil, err
	}
	go nodeController.Run(ctx.Done(), ccmMetrics)

	sharedInformers.Start(ctx.Done())

	// This has to cleanup all the resources allocated by the cloud provider in this cluster
	// - containers as loadbalancers
	// - in windows and darwin ip addresses on the loopback interface
	// Find all the containers associated to the cluster and then use the cloud provider methods to delete
	// the loadbalancer, we can extract the service name from the container labels.
	cancelFn := func() {
		cancel()

		containers, err := container.ListByLabel(fmt.Sprintf("%s=%s", constants.NodeCCMLabelKey, clusterName))
		if err != nil {
			klog.Errorf("can't list containers: %v", err)
			return
		}

		lbController, ok := cloud.LoadBalancer()
		// this can not happen
		if !ok {
			return
		}

		for _, name := range containers {
			// create fake service to pass to the cloud provider method
			v, err := container.GetLabelValue(name, constants.LoadBalancerNameLabelKey)
			if err != nil {
				klog.Infof("could not get the label for the loadbalancer on container %s on cluster %s : %v", name, clusterName, err)
				continue
			}
			clusterName, service := loadbalancer.ServiceFromLoadBalancerSimpleName(v)
			if service == nil {
				klog.Infof("invalid format for loadbalancer on cluster %s: %s", clusterName, v)
				continue
			}
			err = lbController.EnsureLoadBalancerDeleted(context.Background(), clusterName, service)
			if err != nil {
				klog.Infof("error deleting loadbalancer %s/%s on cluster %s : %v", service.Namespace, service.Name, clusterName, err)
				continue
			}
		}
	}

	return &ccm{
		factory:           sharedInformers,
		serviceController: serviceController,
		nodeController:    nodeController,
		cancelFn:          cancelFn}, nil
}

// TODO cleanup alias ip on mac
func (c *Controller) cleanup() {
	for cluster, ccm := range c.clusters {
		klog.Infof("Cleaning resources for cluster %s", cluster)
		ccm.cancelFn()
		delete(c.clusters, cluster)
	}
}

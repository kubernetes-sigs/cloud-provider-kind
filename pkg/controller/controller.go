package controller

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cloudprovider "k8s.io/cloud-provider"
	cloudproviderapi "k8s.io/cloud-provider/api"

	nodecontroller "k8s.io/cloud-provider/controllers/node"
	servicecontroller "k8s.io/cloud-provider/controllers/service"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	ccmfeatures "k8s.io/controller-manager/pkg/features"
	"k8s.io/klog/v2"

	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"

	cpkconfig "sigs.k8s.io/cloud-provider-kind/pkg/config"
	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	"sigs.k8s.io/cloud-provider-kind/pkg/gateway"
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
	gatewayController *gateway.Controller
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

			restConfig, err := c.getRestConfig(ctx, cluster)
			if err != nil {
				klog.Errorf("Failed to create kubeClient for cluster %s: %v", cluster, err)
				continue
			}

			klog.V(2).Infof("Creating new cloud provider for cluster %s", cluster)
			cloud := provider.New(cluster, c.kind)
			ccm, err := startCloudControllerManager(ctx, cluster, restConfig, cloud)
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

func (c *Controller) getKubeConfig(cluster string, internal bool) (*rest.Config, error) {
	kconfig, err := c.kind.KubeConfig(cluster, internal)
	if err != nil {
		klog.Errorf("Failed to get kubeconfig for cluster %s: %v", cluster, err)
		return nil, err
	}

	config, err := clientcmd.RESTConfigFromKubeConfig([]byte(kconfig))
	if err != nil {
		klog.Errorf("Failed to convert kubeconfig for cluster %s: %v", cluster, err)
		return nil, err
	}
	return config, nil
}

// getRestConfig returns a valid rest.Config for the cluster passed as argument
// It tries first to connect to the internal endpoint.
func (c *Controller) getRestConfig(ctx context.Context, cluster string) (*rest.Config, error) {
	addresses := []string{}
	internalConfig, err := c.getKubeConfig(cluster, true)
	if err != nil {
		klog.Errorf("Failed to get internal kubeconfig for cluster %s: %v", cluster, err)
	} else {
		addresses = append(addresses, internalConfig.Host)
	}
	externalConfig, err := c.getKubeConfig(cluster, false)
	if err != nil {
		klog.Errorf("Failed to get external kubeconfig for cluster %s: %v", cluster, err)
	} else {
		addresses = append(addresses, externalConfig.Host)
	}

	if len(addresses) == 0 {
		return nil, fmt.Errorf("could not find kubeconfig for cluster %s", cluster)
	}

	var host string
	for i := 0; i < 5; i++ {
		host, err = firstSuccessfulProbe(ctx, addresses)
		if err != nil {
			klog.Errorf("Failed to connect to any address in %v: %v", addresses, err)
			time.Sleep(time.Second * time.Duration(i))
		} else {
			klog.Infof("Connected succesfully to %s", host)
			break
		}
	}

	var config *rest.Config
	switch host {
	case internalConfig.Host:
		config = internalConfig
		// the first cluster will give us the type of connectivity between
		// cloud-provider-kind and the clusters and load balancer containers.
		// In Linux or containerized cloud-provider-kind this will be direct.
		once.Do(func() {
			cpkconfig.DefaultConfig.ControlPlaneConnectivity = cpkconfig.Direct
		})
	case externalConfig.Host:
		config = externalConfig
	default:
		return nil, fmt.Errorf("restConfig for host %s not avaliable", host)
	}

	return config, nil
}

// TODO: implement leader election to not have problems with  multiple providers
// ref: https://github.com/kubernetes/kubernetes/blob/d97ea0f705847f90740cac3bc3dd8f6a4026d0b5/cmd/kube-scheduler/app/server.go#L211
func startCloudControllerManager(ctx context.Context, clusterName string, config *rest.Config, cloud cloudprovider.Interface) (*ccm, error) {
	// TODO: we need to set up the ccm specific feature gates
	// but try to avoid to expose this to users
	featureGates := utilfeature.DefaultMutableFeatureGate
	err := ccmfeatures.SetupCurrentKubernetesSpecificFeatureGates(featureGates)
	if err != nil {
		return nil, err
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Errorf("Failed to create kubeClient for cluster %s: %v", clusterName, err)
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

	nodeController := &nodecontroller.CloudNodeController{}

	hasCloudProviderTaint, err := getCloudProviderTaint(ctx, clusterName, kubeClient)
	if err != nil {
		klog.Errorf("Failed get cluster nodes: %v", err)
		cancel()
		return nil, err
	}

	if hasCloudProviderTaint {
		// Start the node controller
		nodeController, err = nodecontroller.NewCloudNodeController(
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
	}
	sharedInformers.Start(ctx.Done())

	// Gateway setup
	crdManager, err := gateway.NewCRDManager(config)
	if err != nil {
		klog.Errorf("Failed to create Gateway API CRD manager: %v", err)
		cancel()
		return nil, err
	}

	err = crdManager.InstallCRDs(ctx, cpkconfig.DefaultConfig.GatewayReleaseChannel)
	if err != nil {
		klog.Errorf("Failed to install Gateway API CRDs: %v", err)
		cancel()
		return nil, err
	}

	gwClient, err := gatewayclient.NewForConfig(config)
	if err != nil {
		// This error shouldn't fail. It lives like this as a legacy.
		klog.Errorf("Failed to create Gateway API client: %v", err)
		cancel()
		return nil, err
	}

	sharedGwInformers := gatewayinformers.NewSharedInformerFactory(gwClient, 60*time.Second)

	gatewayController, err := gateway.New(
		clusterName,
		gwClient,
		sharedInformers.Core().V1().Namespaces(),
		sharedInformers.Core().V1().Services(),
		sharedGwInformers.Gateway().V1().Gateways(),
		sharedGwInformers.Gateway().V1().HTTPRoutes(),
		sharedGwInformers.Gateway().V1().GRPCRoutes(),
	)
	if err != nil {
		klog.Errorf("Failed to start gateway controller: %v", err)
		cancel()
		return nil, err
	}

	err = gatewayController.Init(ctx)
	if err != nil {
		klog.Errorf("Failed to initialize gateway controller: %v", err)
		cancel()
		return nil, err
	}

	go func() {
		_ = gatewayController.Run(ctx)
	}()

	sharedInformers.Start(ctx.Done())
	sharedGwInformers.Start(ctx.Done())

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
			cleanupLoadBalancer(lbController, name)
			cleanupGateway(name)
		}
	}

	return &ccm{
		factory:           sharedInformers,
		serviceController: serviceController,
		nodeController:    nodeController,
		gatewayController: gatewayController,
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

func getCloudProviderTaint(ctx context.Context, clusterName string, kubeClient kubernetes.Interface) (bool, error) {
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list nodes for cluster %s: %w", clusterName, err)
	}
	for _, node := range nodes.Items {
		for _, taint := range node.Spec.Taints {
			if taint.Key == cloudproviderapi.TaintExternalCloudProvider {
				return true, nil
			}
		}
	}
	return false, nil
}

func cleanupLoadBalancer(lbController cloudprovider.LoadBalancer, name string) {
	// create fake service to pass to the cloud provider method
	v, err := container.GetLabelValue(name, constants.LoadBalancerNameLabelKey)
	if err != nil || v == "" {
		klog.Infof("could not get the label for the loadbalancer on container %s : %v", name, err)
		return
	}
	clusterName, service := loadbalancer.ServiceFromLoadBalancerSimpleName(v)
	if service == nil {
		klog.Infof("invalid format for loadbalancer on cluster %s: %s", clusterName, v)
		return
	}
	err = lbController.EnsureLoadBalancerDeleted(context.Background(), clusterName, service)
	if err != nil {
		klog.Infof("error deleting loadbalancer %s/%s on cluster %s : %v", service.Namespace, service.Name, clusterName, err)
		return
	}
}

func cleanupGateway(name string) {
	// create fake service to pass to the cloud provider method
	v, err := container.GetLabelValue(name, constants.GatewayNameLabelKey)
	if err != nil || v == "" {
		klog.Infof("could not get the label for the loadbalancer on container %s : %v", name, err)
		return
	}
	err = container.Delete(name)
	if err != nil {
		klog.Infof("error deleting container %s gateway %s : %v", name, v, err)
		return
	}
}

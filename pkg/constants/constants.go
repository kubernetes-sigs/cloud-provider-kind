package constants

const (
	ProviderName = "kind"
	// cloud-provider-kind
	ContainerPrefix = "kindccm"
	// DefaultProxyImage is the default Envoy proxy image
	DefaultProxyImage = "docker.io/envoyproxy/envoy:v1.33.2"
	// KIND constants
	FixedNetworkName = "kind"
	// NodeCCMLabelKey
	NodeCCMLabelKey = "io.x-k8s.cloud-provider-kind.cluster"
	// LoadBalancerNameLabelKey clustername/serviceNamespace/serviceName
	LoadBalancerNameLabelKey = "io.x-k8s.cloud-provider-kind.loadbalancer.name"
	// GatewayNameLabelKey clustername/gatewayNamespace/gatewayName
	GatewayNameLabelKey = "io.x-k8s.cloud-provider-kind.gateway.name"
)

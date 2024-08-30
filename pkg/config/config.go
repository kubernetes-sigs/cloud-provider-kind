package config

// DefaultConfig is a global variable that is initialized at startup with the flags options.
// It can not be modified after that.
var DefaultConfig = &Config{}

type Config struct {
	EnableLogDump bool
	LogDir        string
	// Platforms like Mac or Windows can not access the containers directly
	// so we do a double hop, enable container portmapping for the LoadBalancer containter
	// and do userspace proxying from the original port to the portmaps.
	// If the cloud-provider-kind runs in a container on these platforms only enables portmapping.
	LoadBalancerConnectivity Connectivity
	// Type of connectivity between the cloud-provider-kind and the clusters
	ControlPlaneConnectivity Connectivity
}

type Connectivity int

const (
	Unknown Connectivity = iota
	Direct
	Portmap
	Tunnel
)

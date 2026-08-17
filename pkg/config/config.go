package config

import (
	"os"

	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
)

// DefaultConfig is a global variable that is initialized at startup.
// Note: Its fields are modified by command-line flags in cmd/app.go.
var DefaultConfig = newDefaultConfig()

func newDefaultConfig() *Config {
	c := &Config{
		GatewayReleaseChannel: Standard,
		IngressDefault:        true,
		ProxyImage:            constants.DefaultProxyImageRegistry + "/" + constants.DefaultProxyImageName,
	}

	if registry := os.Getenv("CLOUD_PROVIDER_KIND_REGISTRY_URL"); registry != "" {
		c.ProxyImage = registry + "/" + constants.DefaultProxyImageName
	}

	return c
}



type Config struct {
	EnableLogDump bool
	LogDir        string
	// Platforms like Mac or Windows can not access the containers directly
	// so we do a double hop, enable container portmapping for the LoadBalancer containter
	// and do userspace proxying from the original port to the portmaps.
	// If the cloud-provider-kind runs in a container on these platforms only enables portmapping.
	LoadBalancerConnectivity Connectivity
	// Host port used when LoadBalancerConnectivity is Portmap.
	// 0 means the OS assigns an ephemeral port; any other value is used as-is.
	LoadBalancerPortMappingHostPort int32
	// Type of connectivity between the cloud-provider-kind and the clusters
	ControlPlaneConnectivity Connectivity
	// Gateway API Release channel (default stable)
	// https://gateway-api.sigs.k8s.io/concepts/versioning/
	GatewayReleaseChannel GatewayReleaseChannel
	IngressDefault        bool
	ProxyImage            string
}

type Connectivity int

const (
	Unknown Connectivity = iota
	Direct
	Portmap
	Tunnel
)

type GatewayReleaseChannel string

const (
	Standard     GatewayReleaseChannel = "standard"
	Experimental GatewayReleaseChannel = "experimental"
	Disabled     GatewayReleaseChannel = "disabled"
)

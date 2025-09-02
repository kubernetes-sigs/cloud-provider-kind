// main.go
package main

import (
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"
)

// usage: ./route-adder <cidr> <gateway_ip>
func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <destination_cidr> <gateway_ip>\n", os.Args[0])
		os.Exit(1)
	}

	cidr := os.Args[1]
	gatewayIP := os.Args[2]

	// Parse the destination CIDR. This gives us the destination network.
	_, dstNet, err := net.ParseCIDR(cidr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing destination CIDR %s: %v\n", cidr, err)
		os.Exit(1)
	}

	// Parse the gateway IP address.
	gw := net.ParseIP(gatewayIP)
	if gw == nil {
		fmt.Fprintf(os.Stderr, "Error parsing gateway IP %s\n", gatewayIP)
		os.Exit(1)
	}

	// Create the route object
	route := &netlink.Route{
		Dst: dstNet,
		Gw:  gw,
	}

	// Use RouteReplace, which is equivalent to `ip route replace`.
	// It will add the route if it doesn't exist or update it if it does.
	if err := netlink.RouteReplace(route); err != nil {
		fmt.Fprintf(os.Stderr, "Error replacing route: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Successfully replaced route: %s via %s\n", cidr, gatewayIP)
}

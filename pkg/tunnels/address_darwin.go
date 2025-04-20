//go:build darwin

package tunnels

import (
	"os/exec"
)

func AddIPToLocalInterface(ip string) (string, error) {
	// TODO: IPv6
	output, err := exec.Command("ifconfig", "lo0", "alias", ip, "netmask", "255.255.255.255").CombinedOutput()
	return string(output), err
}

func RemoveIPFromLocalInterface(ip string) (string, error) {
	// delete the IP address
	output, err := exec.Command("ifconfig", "lo0", "-alias", ip).CombinedOutput()
	return string(output), err
}

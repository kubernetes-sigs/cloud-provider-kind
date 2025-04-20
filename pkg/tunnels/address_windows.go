//go:build windows

package tunnels

import (
	"os/exec"
)

func AddIPToLocalInterface(ip string) (string, error) {
	output, err := exec.Command("netsh", "interface", "ip", "add", "address", "loopback", ip, "255.255.255.255").CombinedOutput()
	return string(output), err
}

func RemoveIPFromLocalInterface(ip string) (string, error) {
	output, err := exec.Command("netsh", "interface", "ip", "delete", "address", "loopback", ip, "255.255.255.255").CombinedOutput()
	return string(output), err
}

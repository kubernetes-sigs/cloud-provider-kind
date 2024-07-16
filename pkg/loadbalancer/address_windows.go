//go:build windows

package loadbalancer

import (
	"os/exec"
)

func AddIPToLocalInterface(ip string) error {
	err := exec.Command("netsh", "interface", "ip", "add", "address", "loopback", ip, "255.255.255.255").Run()
	if err != nil {
		return err
	}
	return nil
}

func RemoveIPFromLocalInterface(ip string) error {
	err := exec.Command("netsh", "interface", "ip", "delete", "address", "loopback", ip, "255.255.255.255").Run()
	if err != nil {
		return err
	}
	return nil
}

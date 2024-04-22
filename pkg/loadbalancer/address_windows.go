//go:build windows

package loadbalancer

import (
	"os/exec"
)

func AddIPToInterface(ifaceName string, ip string) error {
	err := exec.Command("netsh", "interface", "ip", "add", "address", "loopback", ip, "255.255.255.255").Run()
	if err != nil {
		return err
	}
	return nil
}

func RemoveIPToInterface(ifaceName string, ip string) error {
	err := exec.Command("netsh", "interface", "ip", "delete", "address", "loopback", ip, "255.255.255.255").Run()
	if err != nil {
		return err
	}
	return nil
}

//go:build darwin

package loadbalancer

import (
	"os/exec"
)

func AddIPToLocalInterface(ip string) error {
	// TODO: IPv6
	err := exec.Command("ifconfig", "lo0", "alias", ip, "netmask", "255.255.255.255").Run()
	if err != nil {
		return err
	}
	return nil
}

func RemoveIPFromLocalInterface(ip string) error {
	// delete the IP address
	err := exec.Command("ifconfig", "lo0", "-alias", ip).Run()
	if err != nil {
		return err
	}
	return nil

}

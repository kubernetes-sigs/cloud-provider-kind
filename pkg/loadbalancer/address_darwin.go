//go:build darwin

package loadbalancer

import (
	"os/exec"
)

func AddIPToInterface(ifaceName string, ip string) error {
	// TODO: IPv6
	err := exec.Command("ifconfig", ifaceName, "alias", ip, "netmask", "255.255.255.255").Run()
	if err != nil {
		return err
	}
	return nil
}

func RemoveIPToInterface(ifaceName string, ip string) error {
	// delete the IP address
	err := exec.Command("ifconfig", ifaceName, "-alias", ip).Run()
	if err != nil {
		return err
	}
	return nil

}

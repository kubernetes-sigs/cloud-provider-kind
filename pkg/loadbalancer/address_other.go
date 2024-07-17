//go:build !windows && !darwin

package loadbalancer

import "os/exec"

func AddIPToLocalInterface(ip string) error {
	err := exec.Command("ip", "addr", "add", ip, "dev", "lo").Run()
	if err != nil {
		return err
	}
	return nil
}

func RemoveIPFromLocalInterface(ip string) error {
	err := exec.Command("ip", "addr", "del", ip, "dev", "lo").Run()
	if err != nil {
		return err
	}
	return nil
}

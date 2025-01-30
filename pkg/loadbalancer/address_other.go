//go:build !windows && !darwin

package loadbalancer

import (
	"os/exec"
)

func AddIPToLocalInterface(ip string) (string, error) {
	output, err := exec.Command("ip", "addr", "add", ip, "dev", "lo").CombinedOutput()
	return string(output), err
}

func RemoveIPFromLocalInterface(ip string) (string, error) {
	output, err := exec.Command("ip", "addr", "del", ip, "dev", "lo").CombinedOutput()
	return string(output), err
}

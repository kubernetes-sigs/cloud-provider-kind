//go:build !windows && !darwin

package loadbalancer

import (
	"bytes"
	"errors"
	"os/exec"
)

func AddIPToLocalInterface(ip string) error {
	outBytes, err := exec.Command("ip", "addr", "add", ip, "dev", "lo").CombinedOutput()

	var exerr *exec.ExitError
	if errors.As(err, &exerr) && bytes.ContainsAny(outBytes, "File exists") {
		// unsure why but `Run()` doesn't capture stderr properly
		// if 'File exists' is in the output, then the lo device has the IP already
		return nil
	} else if err != nil {
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

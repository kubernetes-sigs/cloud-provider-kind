//go:build !windows && !darwin

package loadbalancer

func AddIPToInterface(ifaceName string, ip string) error {
	return nil
}

func RemoveIPToInterface(ifaceName string, ip string) error {
	return nil
}

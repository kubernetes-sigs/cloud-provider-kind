//go:build windows

package loadbalancer

import "fmt"

func AddIPToInterface(ifaceName string, ip string) error {
	return fmt.Errorf(("not implemented"))
}

func RemoveIPToInterface(ifaceName string, ip string) error {
	return fmt.Errorf(("not implemented"))
}

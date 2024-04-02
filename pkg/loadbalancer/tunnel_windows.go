//go:build windows

package loadbalancer

import "fmt"

func addIP(ifaceName string, ip string) error {
	return fmt.Errorf(("not implemented"))
}

func removeIP(ifaceName string, ip string) error {
	return fmt.Errorf(("not implemented"))
}

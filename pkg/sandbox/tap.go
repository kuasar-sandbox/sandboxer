package sandbox

import (
	"fmt"
	"net"
)

// VerifyTAP confirms that the named TAP/network interface exists. We do
// not create or modify the interface; the host platform is expected to
// provision TAPs ahead of sandbox launch.
func VerifyTAP(name string) error {
	if name == "" {
		return fmt.Errorf("tap: name is empty")
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("tap: interface %s not found: %w", name, err)
	}
	if iface.Flags&net.FlagUp == 0 {
		return fmt.Errorf("tap: interface %s is down", name)
	}
	return nil
}

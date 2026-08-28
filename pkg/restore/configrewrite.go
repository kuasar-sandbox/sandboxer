package restore

import (
	"encoding/json"
	"fmt"
)

// pathRewrite gathers the runtime paths that supersede the values
// captured in the snapshotted config.json.
type pathRewrite struct {
	UffdSocket   string
	DiskSocks    []string // vhost sockets in CH --disk (device) order
	DiskReadOnly []bool   // expected readonly role for every device slot
	APISock      string
	VsockSock    string
}

// rewriteConfigPaths takes a config.json blob, parses as a generic map,
// and patches the runtime-bound paths to point at this restore session's
// sockets. Only the paths change; the rest of the VM config is
// preserved bit-for-bit (CH validates fields like memory.size against
// state.json — keep them).
func rewriteConfigPaths(in []byte, p pathRewrite) ([]byte, error) {
	var cfg map[string]any
	if err := json.Unmarshal(in, &cfg); err != nil {
		return nil, err
	}

	// memory.zones[*].uffd_socket
	if mem, ok := cfg["memory"].(map[string]any); ok {
		if zones, ok := mem["zones"].([]any); ok {
			for _, z := range zones {
				if zm, ok := z.(map[string]any); ok {
					if _, has := zm["uffd_socket"]; has || p.UffdSocket != "" {
						zm["uffd_socket"] = p.UffdSocket
					}
				}
			}
		}
	}

	// disks[*].vhost_socket — slot i → this run's i-th device socket (device
	// order: root lower/upper first, then each data disk lower/upper). E and S
	// are independent roots, so accepting either a surplus or missing device
	// would bind E's disk provenance to a different captured VM topology.
	rawDisks, exists := cfg["disks"]
	disks, array := rawDisks.([]any)
	if !exists || !array {
		return nil, fmt.Errorf("rewriteConfigPaths: disk topology mismatch: snapshot config.json.disks must be an array with %d devices", len(p.DiskSocks))
	}
	if len(disks) != len(p.DiskSocks) {
		return nil, fmt.Errorf("rewriteConfigPaths: disk topology mismatch: snapshot has %d devices, Sandbox E requires %d", len(disks), len(p.DiskSocks))
	}
	if len(p.DiskReadOnly) != len(p.DiskSocks) {
		return nil, fmt.Errorf("rewriteConfigPaths: disk topology mismatch: got %d readonly roles for %d Sandbox E devices", len(p.DiskReadOnly), len(p.DiskSocks))
	}
	for i, d := range disks {
		dm, ok := d.(map[string]any)
		if !ok || dm == nil {
			return nil, fmt.Errorf("rewriteConfigPaths: config.json.disks[%d] must be an object", i)
		}
		if p.DiskSocks[i] == "" {
			return nil, fmt.Errorf("rewriteConfigPaths: empty socket for disk slot %d", i)
		}
		readOnly := false
		if raw, exists := dm["readonly"]; exists {
			var boolean bool
			boolean, ok = raw.(bool)
			if !ok {
				return nil, fmt.Errorf("rewriteConfigPaths: config.json.disks[%d].readonly must be a boolean", i)
			}
			readOnly = boolean
		}
		if readOnly != p.DiskReadOnly[i] {
			return nil, fmt.Errorf("rewriteConfigPaths: disk topology mismatch: config.json.disks[%d] readonly=%t conflicts with Sandbox E readonly=%t", i, readOnly, p.DiskReadOnly[i])
		}
		dm["vhost_socket"] = p.DiskSocks[i]
	}

	// vsock socket path. CH 51 stores it under "vsock.socket".
	if vsock, ok := cfg["vsock"].(map[string]any); ok {
		if p.VsockSock != "" {
			vsock["socket"] = p.VsockSock
		}
	}

	return json.Marshal(cfg)
}

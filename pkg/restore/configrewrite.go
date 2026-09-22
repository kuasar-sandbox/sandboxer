package restore

import (
	"encoding/json"
	"fmt"
)

// pathRewrite gathers the runtime paths and network backend that supersede
// the values captured in the snapshotted config.json.
type pathRewrite struct {
	UffdSocket   string
	DiskSocks    []string // vhost sockets in CH --disk (device) order
	DiskReadOnly []bool   // expected readonly role for every device slot
	APISock      string
	VsockSock    string
	TargetTap    string
	IsTapFD      bool
}

// rewriteConfigPaths takes a config.json blob, parses as a generic map,
// and patches the runtime-bound paths and virtio-net backend to point at this
// restore session's resources. Only the host backend paths/selectors change;
// the rest of the VM config is preserved bit-for-bit (CH validates fields like
// memory.size and device state against state.json).
//
// Returns the rewritten config JSON and the captured network device ID.
func rewriteConfigPaths(in []byte, p pathRewrite) ([]byte, string, error) {
	if p.TargetTap != "" && p.IsTapFD {
		return nil, "", fmt.Errorf("rewriteConfigPaths: cannot specify both TargetTap and IsTapFD")
	}

	var cfg map[string]any
	if err := json.Unmarshal(in, &cfg); err != nil {
		return nil, "", err
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

	// net[0]: virtio-net host backend rebinding.
	// Cloud Hypervisor v51.1 evaluates net device backends as follows:
	// - If net.tap is set, it attempts to open the named tap directly.
	// - If net.tap is unset and net.fds is present, it validates and rebinds
	//   descriptors supplied via --restore net_fds=[<id>@[...]].
	// - If neither is present or both are present, tap takes precedence.
	//
	// To safely restore a snapshot across different host backends:
	// - Target named TAP: set net[0].tap to the target TAP and remove net[0].fds.
	// - Target TapFD: remove net[0].tap and set net[0].fds to a placeholder array
	//   deserialized by CH as -1 placeholders, which are subsequently rebound to
	//   the inherited tapfd via net_fds=[<id>@[...]].
	// Device identity (id, MAC, queue count, offloads, IOMMU) is strictly preserved.
	var netDeviceID string
	if rawNets, ok := cfg["net"]; ok && rawNets != nil {
		nets, isArray := rawNets.([]any)
		if !isArray {
			return nil, "", fmt.Errorf("rewriteConfigPaths: config.json.net must be an array or null")
		}
		if len(nets) > 1 {
			return nil, "", fmt.Errorf("rewriteConfigPaths: config.json.net: expected at most one device, got %d", len(nets))
		}
		if len(nets) == 1 {
			nm, ok := nets[0].(map[string]any)
			if !ok || nm == nil {
				return nil, "", fmt.Errorf("rewriteConfigPaths: config.json.net[0] must be an object")
			}
			if rawID, exists := nm["id"]; exists && rawID != nil {
				if idStr, ok := rawID.(string); ok && idStr != "" {
					netDeviceID = idStr
				}
			}
			if p.TargetTap != "" {
				nm["tap"] = p.TargetTap
				delete(nm, "fds")
			} else if p.IsTapFD {
				if netDeviceID == "" {
					return nil, "", fmt.Errorf("rewriteConfigPaths: captured network device has missing or empty id for fd-backed restore")
				}
				// Preflight: sandboxer's tapfd handoff currently supplies a single queue FD.
				// If the snapshot captured a multi-queue device (>1 queue pair / >2 virtqueues),
				// fail closed explicitly rather than failing downstream in Cloud Hypervisor validation.
				if existingFDs, ok := nm["fds"].([]any); ok && len(existingFDs) > 1 {
					return nil, "", fmt.Errorf("rewriteConfigPaths: multi-queue net device (%d queue fds) cannot be restored with single-queue tapfd", len(existingFDs))
				}
				if nq, ok := nm["num_queues"].(float64); ok && int(nq) > 2 {
					return nil, "", fmt.Errorf("rewriteConfigPaths: multi-queue net device (%d virtqueues) cannot be restored with single-queue tapfd", int(nq))
				}
				delete(nm, "tap")
				// Placeholder array of the required length (currently one queue FD, [-1]).
				nm["fds"] = []any{-1}
			}
		} else if len(nets) == 0 {
			if p.TargetTap != "" || p.IsTapFD {
				return nil, "", fmt.Errorf("rewriteConfigPaths: cannot rebind network to snapshot with no network device")
			}
		}
	} else {
		if p.TargetTap != "" || p.IsTapFD {
			return nil, "", fmt.Errorf("rewriteConfigPaths: cannot rebind network to snapshot with no network device")
		}
	}

	// disks[*].vhost_socket — slot i → this run's i-th device socket (device
	// order: root lower/upper first, then each data disk lower/upper). E and S
	// are independent roots, so accepting either a surplus or missing device
	// would bind E's disk provenance to a different captured VM topology.
	rawDisks, exists := cfg["disks"]
	disks, array := rawDisks.([]any)
	if !exists || !array {
		return nil, "", fmt.Errorf("rewriteConfigPaths: disk topology mismatch: snapshot config.json.disks must be an array with %d devices", len(p.DiskSocks))
	}
	if len(disks) != len(p.DiskSocks) {
		return nil, "", fmt.Errorf("rewriteConfigPaths: disk topology mismatch: snapshot has %d devices, Sandbox E requires %d", len(disks), len(p.DiskSocks))
	}
	if len(p.DiskReadOnly) != len(p.DiskSocks) {
		return nil, "", fmt.Errorf("rewriteConfigPaths: disk topology mismatch: got %d readonly roles for %d Sandbox E devices", len(p.DiskReadOnly), len(p.DiskSocks))
	}
	for i, d := range disks {
		dm, ok := d.(map[string]any)
		if !ok || dm == nil {
			return nil, "", fmt.Errorf("rewriteConfigPaths: config.json.disks[%d] must be an object", i)
		}
		if p.DiskSocks[i] == "" {
			return nil, "", fmt.Errorf("rewriteConfigPaths: empty socket for disk slot %d", i)
		}
		readOnly := false
		if raw, exists := dm["readonly"]; exists {
			var boolean bool
			boolean, ok = raw.(bool)
			if !ok {
				return nil, "", fmt.Errorf("rewriteConfigPaths: config.json.disks[%d].readonly must be a boolean", i)
			}
			readOnly = boolean
		}
		if readOnly != p.DiskReadOnly[i] {
			return nil, "", fmt.Errorf("rewriteConfigPaths: disk topology mismatch: config.json.disks[%d] readonly=%t conflicts with Sandbox E readonly=%t", i, readOnly, p.DiskReadOnly[i])
		}
		dm["vhost_socket"] = p.DiskSocks[i]
	}

	// vsock socket path. CH 51 stores it under "vsock.socket".
	if vsock, ok := cfg["vsock"].(map[string]any); ok {
		if p.VsockSock != "" {
			vsock["socket"] = p.VsockSock
		}
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, "", err
	}
	return out, netDeviceID, nil
}

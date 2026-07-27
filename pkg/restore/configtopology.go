package restore

import (
	"encoding/json"
	"errors"
	"fmt"
)

// snapshotConfigHasNetwork reports whether Cloud Hypervisor's snapshotted
// config.json contains any virtio-net devices. VmConfig.net is serialized as
// null when no network device exists and as an array otherwise. A missing net
// field has the same no-device topology as null.
func snapshotConfigHasNetwork(configJSON []byte) (bool, error) {
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return false, fmt.Errorf("snapshot config.json: %w", err)
	}
	if cfg == nil {
		return false, errors.New("snapshot config.json: expected JSON object")
	}

	raw, ok := cfg["net"]
	if !ok {
		return false, nil
	}
	var devices []json.RawMessage
	if err := json.Unmarshal(raw, &devices); err != nil {
		return false, fmt.Errorf("snapshot config.json.net: expected array or null: %w", err)
	}
	switch len(devices) {
	case 0:
		return false, nil
	case 1:
		var device map[string]json.RawMessage
		if err := json.Unmarshal(devices[0], &device); err != nil || device == nil {
			return false, errors.New("snapshot config.json.net[0]: expected JSON object")
		}
		return true, nil
	default:
		return false, fmt.Errorf("snapshot config.json.net: expected at most one device, got %d", len(devices))
	}
}

// validateRestoreNetworkTopology prevents restore from adding or removing a
// virtio-net device. Cloud Hypervisor restores the device topology captured in
// config.json; host network configuration can only rebind that existing
// device.
func validateRestoreNetworkTopology(snapshotHasNetwork, hostHasNetwork bool) error {
	if snapshotHasNetwork == hostHasNetwork {
		return nil
	}
	if snapshotHasNetwork {
		return errors.New("network topology mismatch: snapshot has a network device but restore config has no network source")
	}
	return errors.New("network topology mismatch: snapshot has no network device but restore config provides a network source")
}

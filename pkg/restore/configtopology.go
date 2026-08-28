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

// validateSnapshotVCPUTopology requires the immutable Sandbox E capacity to
// describe the exact CPU topology Cloud Hypervisor captured in Snapshot S.
// sandboxer starts CH with --cpus boot=N and no separate max value, so both
// serialized values must remain N; restore does not support CPU hotplug.
func validateSnapshotVCPUTopology(configJSON []byte, sandboxCPU int) error {
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return fmt.Errorf("snapshot config.json: %w", err)
	}
	if cfg == nil {
		return errors.New("snapshot config.json: expected JSON object")
	}
	rawCPUs, exists := cfg["cpus"]
	if !exists {
		return errors.New("snapshot config.json.cpus is required")
	}
	var cpus map[string]json.RawMessage
	if err := json.Unmarshal(rawCPUs, &cpus); err != nil || cpus == nil {
		return errors.New("snapshot config.json.cpus must be an object")
	}
	readCount := func(field string) (uint32, error) {
		raw, ok := cpus[field]
		if !ok {
			return 0, fmt.Errorf("snapshot config.json.cpus.%s is required", field)
		}
		var count uint32
		if err := json.Unmarshal(raw, &count); err != nil {
			return 0, fmt.Errorf("snapshot config.json.cpus.%s must be a positive integer: %w", field, err)
		}
		if count == 0 {
			return 0, fmt.Errorf("snapshot config.json.cpus.%s must be positive", field)
		}
		return count, nil
	}
	boot, err := readCount("boot_vcpus")
	if err != nil {
		return err
	}
	maximum, err := readCount("max_vcpus")
	if err != nil {
		return err
	}
	if sandboxCPU <= 0 {
		return fmt.Errorf("referenced Sandbox CPU capacity=%d must be positive", sandboxCPU)
	}
	if uint64(boot) != uint64(sandboxCPU) || uint64(maximum) != uint64(sandboxCPU) {
		return fmt.Errorf("snapshot CPU topology mismatch: CH boot=%d max=%d Sandbox=%d", boot, maximum, sandboxCPU)
	}
	return nil
}

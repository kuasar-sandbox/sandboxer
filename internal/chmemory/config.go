// Package chmemory implements the Cloud Hypervisor memory-capacity contract
// shared by live vm.info observations and snapshot config.json validation.
package chmemory

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
)

// Config is the memory portion of Cloud Hypervisor's VmConfig. Cloud
// Hypervisor v51.1 uses zones instead of the top-level size when size is zero.
type Config struct {
	Size           uint64  `json:"size"`
	HotplugSize    *uint64 `json:"hotplug_size"`
	HotpluggedSize *uint64 `json:"hotplugged_size"`
	Zones          []Zone  `json:"zones"`
}

type Zone struct {
	Size           uint64  `json:"size"`
	HotplugSize    *uint64 `json:"hotplug_size"`
	HotpluggedSize *uint64 `json:"hotplugged_size"`
}

// TotalSize returns Cloud Hypervisor's exact configured boot-memory capacity.
// A non-zero top-level size and user-provided zones are mutually exclusive.
// Dynamic memory hotplug is outside the fixed-capacity Budget model and is
// rejected instead of being folded into Capacity.
func (c Config) TotalSize() (uint64, error) {
	if (c.HotplugSize != nil && *c.HotplugSize != 0) ||
		(c.HotpluggedSize != nil && *c.HotpluggedSize != 0) {
		return 0, errors.New("memory hotplug is unsupported by the fixed-capacity Budget model")
	}
	if c.Size != 0 {
		if c.Zones != nil {
			return 0, errors.New("memory zones require top-level size 0")
		}
		return c.Size, nil
	}
	if len(c.Zones) == 0 {
		return 0, errors.New("memory zones are required when top-level size is 0")
	}

	var total uint64
	add := func(value uint64) error {
		var carry uint64
		total, carry = bits.Add64(total, value, 0)
		if carry != 0 {
			return errors.New("memory capacity overflow")
		}
		return nil
	}
	for _, zone := range c.Zones {
		if (zone.HotplugSize != nil && *zone.HotplugSize != 0) ||
			(zone.HotpluggedSize != nil && *zone.HotpluggedSize != 0) {
			return 0, errors.New("memory-zone hotplug is unsupported by the fixed-capacity Budget model")
		}
		if err := add(zone.Size); err != nil {
			return 0, err
		}
	}
	if total == 0 {
		return 0, errors.New("memory capacity must be positive")
	}
	return total, nil
}

// CapacityFromVMConfig returns the exact memory total captured in a Cloud
// Hypervisor snapshot config.json.
func CapacityFromVMConfig(configJSON []byte) (uint64, error) {
	var vm struct {
		Memory *Config `json:"memory"`
	}
	if err := json.Unmarshal(configJSON, &vm); err != nil {
		return 0, fmt.Errorf("config.json: %w", err)
	}
	if vm.Memory == nil {
		return 0, errors.New("config.json: memory is required")
	}
	capacity, err := vm.Memory.TotalSize()
	if err != nil {
		return 0, fmt.Errorf("config.json memory: %w", err)
	}
	return capacity, nil
}

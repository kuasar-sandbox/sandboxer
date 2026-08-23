// Package chmemory implements the Cloud Hypervisor memory-capacity contract
// shared by live vm.info observations and snapshot config.json validation.
package chmemory

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
)

// Config is the memory portion of Cloud Hypervisor's VmConfig. TotalSize
// mirrors MemoryConfig::total_size() in Cloud Hypervisor v51.1.
type Config struct {
	Size           uint64  `json:"size"`
	HotpluggedSize *uint64 `json:"hotplugged_size"`
	Zones          []Zone  `json:"zones"`
}

type Zone struct {
	Size           uint64  `json:"size"`
	HotpluggedSize *uint64 `json:"hotplugged_size"`
}

// TotalSize returns base size and every configured zone size, including their
// hotplugged portions. Overflow and a zero total are errors because either
// makes the Budget domain ambiguous.
func (c Config) TotalSize() (uint64, error) {
	total := c.Size
	add := func(value uint64) error {
		var carry uint64
		total, carry = bits.Add64(total, value, 0)
		if carry != 0 {
			return errors.New("memory capacity overflow")
		}
		return nil
	}
	if c.HotpluggedSize != nil {
		if err := add(*c.HotpluggedSize); err != nil {
			return 0, err
		}
	}
	for _, zone := range c.Zones {
		if err := add(zone.Size); err != nil {
			return 0, err
		}
		if zone.HotpluggedSize != nil {
			if err := add(*zone.HotpluggedSize); err != nil {
				return 0, err
			}
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

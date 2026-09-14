package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
)

// readCurrentResourceStats checks runtime liveness on both sides of the read.
// The sole reaper marks an observed exit before native usage final sampling;
// that saved endpoint remains usable by usage without becoming a live snapshot.
func readCurrentResourceStats(sandboxID string, cfg *config.SandboxConfig, cgroupPath string, live func() bool) (ctl.ResourceStats, error) {
	stats, err := readResourceStats(sandboxID, cfg, cgroupPath, live())
	if !live() {
		stats.MemoryUsed, stats.CPUUsageUsec, stats.TimestampUnix = nil, nil, nil
	}
	return stats, err
}

// readResourceStats uses only the effective run configuration and the existing
// pinned VMM cgroup descriptor path. It neither samples native usage nor calls
// the guest, Cloud Hypervisor API, resource controller or balloon controller.
func readResourceStats(sandboxID string, cfg *config.SandboxConfig, cgroupPath string, live bool) (ctl.ResourceStats, error) {
	if cfg == nil {
		return ctl.ResourceStats{}, errors.New("resource stats: effective configuration unavailable")
	}
	capacity, err := cfg.CapacityMemoryBytes()
	if err != nil {
		return ctl.ResourceStats{}, fmt.Errorf("resource stats capacity: %w", err)
	}
	headroom, err := cfg.AllocatableMemoryBytes()
	if err != nil {
		return ctl.ResourceStats{}, fmt.Errorf("resource stats headroom: %w", err)
	}
	stats := ctl.ResourceStats{SandboxID: sandboxID, CPUCapacity: cfg.Resources.Capacity.CPU,
		CPUAllocatable: cfg.Resources.Allocatable.CPU, MemoryCapacity: capacity, MemoryHeadroom: headroom}
	if !live || cgroupPath == "" {
		return stats, nil
	}
	memory, err := readResourceFile(cgroupPath, "memory.current")
	if err != nil {
		return ctl.ResourceStats{}, err
	}
	if memory != nil {
		value, err := strconv.ParseUint(strings.TrimSpace(string(memory)), 10, 64)
		if err != nil {
			return ctl.ResourceStats{}, fmt.Errorf("resource stats memory.current: %w", err)
		}
		stats.MemoryUsed = &value
	}
	cpu, err := readResourceFile(cgroupPath, "cpu.stat")
	if err != nil {
		return ctl.ResourceStats{}, err
	}
	for _, line := range strings.Split(string(cpu), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "usage_usec" {
			continue
		}
		if len(fields) != 2 || stats.CPUUsageUsec != nil {
			return ctl.ResourceStats{}, errors.New("resource stats: invalid cpu.stat usage_usec")
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return ctl.ResourceStats{}, fmt.Errorf("resource stats cpu.stat: %w", err)
		}
		stats.CPUUsageUsec = &value
	}
	if stats.MemoryUsed != nil || stats.CPUUsageUsec != nil {
		timestamp := time.Now().Unix()
		stats.TimestampUnix = &timestamp
	}
	return stats, nil
}

func readResourceFile(cgroupPath, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(cgroupPath, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // A missing controller statistic is distinct from zero.
	}
	if err != nil {
		return nil, fmt.Errorf("resource stats %s: %w", name, err)
	}
	return data, nil
}

package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
)

func statsConfig() *config.SandboxConfig {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.CPU, cfg.Resources.Capacity.Memory = 4, "1MiB"
	cfg.Resources.Allocatable.CPU, cfg.Resources.Allocatable.Memory = 0.75, "256KiB"
	return cfg
}

func statsFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestResourceStatsScopeAndPinnedSource(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "vmm")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	cgroup, err := resctl.SetupCgroup(resctl.CgroupConfig{Path: path, MemoryMaxBytes: 8 << 20, CPUMaxQuotaUs: 400000, CPUWeight: 75})
	if err != nil {
		t.Fatal(err)
	}
	defer cgroup.Cleanup()
	statsFile(t, path, "memory.current", "3145728\n") // VMM charge can exceed Guest capacity.
	statsFile(t, path, "cpu.stat", "usage_usec 9007199254740993\nuser_usec 7\nsystem_usec 8\n")
	statsFile(t, path, "memory.stat", "inactive_file 2000000\n")
	if err := os.Rename(path, filepath.Join(directory, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	statsFile(t, path, "memory.current", "12\n")
	statsFile(t, path, "cpu.stat", "usage_usec 34\n")
	for _, dynamic := range []bool{false, true} {
		cfg := statsConfig()
		cfg.Resources.Control.CgroupPath = path
		if dynamic {
			cfg.Resources.Control.Controller = filepath.Join(directory, "absent-controller.sock")
		}
		before := *cfg
		start := time.Now().Unix()
		got, err := readResourceStats("sid", cfg, cgroup.LocalPath(), true)
		if err != nil {
			t.Fatal(err)
		}
		if got.SandboxID != "sid" || got.CPUCapacity != 4 || got.CPUAllocatable != .75 || got.MemoryCapacity != 1<<20 || got.MemoryHeadroom != 256<<10 {
			t.Fatalf("effective specification changed: %+v", got)
		}
		if got.MemoryUsed == nil || *got.MemoryUsed != 3<<20 || got.CPUUsageUsec == nil || *got.CPUUsageUsec != 9007199254740993 {
			t.Fatalf("wrong cgroup, clipped memory or lossy CPU: %+v", got)
		}
		if got.TimestampUnix == nil || *got.TimestampUnix < start || *got.TimestampUnix > time.Now().Unix() {
			t.Fatal("not the actual observation time", got.TimestampUnix)
		}
		if !reflect.DeepEqual(*cfg, before) || cfg.Usage.Enabled {
			t.Fatal("resource read changed policy or required usage")
		}
	}
	high, err := os.ReadFile(filepath.Join(directory, "original", "memory.high"))
	if err != nil || string(high) != "max" {
		t.Fatal("resource read changed memory control", string(high), err)
	}
}

func TestResourceStatsZeroMissingResetAndNoLive(t *testing.T) {
	dir, cfg := t.TempDir(), statsConfig()
	read := func(live bool) {
		t.Helper()
		got, err := readResourceStats("sid", cfg, dir, live)
		if err != nil {
			t.Fatal(err)
		}
		if !live && (got.MemoryUsed != nil || got.CPUUsageUsec != nil || got.TimestampUnix != nil) {
			t.Fatal("invented current host observation without live VMM")
		}
	}
	got, err := readResourceStats("sid", cfg, dir, true)
	if err != nil || got.MemoryUsed != nil || got.CPUUsageUsec != nil || got.TimestampUnix != nil {
		t.Fatal("missing counters were not omitted", got, err)
	}
	statsFile(t, dir, "memory.current", "0")
	got, err = readResourceStats("sid", cfg, dir, true)
	if err != nil || got.MemoryUsed == nil || *got.MemoryUsed != 0 || got.CPUUsageUsec != nil || got.TimestampUnix == nil {
		t.Fatal("valid memory zero lost or absent CPU fabricated", got, err)
	}
	statsFile(t, dir, "cpu.stat", "usage_usec 9000000\n")
	read(true)
	statsFile(t, dir, "cpu.stat", "usage_usec 0\n")
	got, err = readResourceStats("sid", cfg, dir, true)
	if err != nil || got.CPUUsageUsec == nil || *got.CPUUsageUsec != 0 {
		t.Fatal("current source reset accumulated or discarded", got, err)
	}
	if err := os.Remove(filepath.Join(dir, "memory.current")); err != nil {
		t.Fatal(err)
	}
	got, err = readResourceStats("sid", cfg, dir, true)
	if err != nil || got.MemoryUsed != nil || got.CPUUsageUsec == nil || *got.CPUUsageUsec != 0 || got.TimestampUnix == nil {
		t.Fatal("valid CPU zero lost or absent memory fabricated", got, err)
	}
	read(false)
	got, err = readResourceStats("sid", cfg, "", true)
	if err != nil || got.MemoryUsed != nil || got.CPUUsageUsec != nil || got.TimestampUnix != nil {
		t.Fatal("no-cgroup mode invented an observation", got, err)
	}
	for _, bad := range []string{"usage_usec NaN\n", "usage_usec 1\nusage_usec 2\n", "usage_usec -1\n", "usage_usec\n"} {
		statsFile(t, dir, "cpu.stat", bad)
		if _, err := readResourceStats("sid", cfg, dir, true); err == nil {
			t.Fatalf("invalid counter accepted: %q", bad)
		}
		read(false) // An unavailable runtime does not read even corrupt host files.
	}
}

func TestResourceStatsDropsObservationOnExitDuringRead(t *testing.T) {
	dir := t.TempDir()
	statsFile(t, dir, "memory.current", "123")
	statsFile(t, dir, "cpu.stat", "usage_usec 456\n")
	checks := 0
	stats, err := readCurrentResourceStats("sid", statsConfig(), dir, func() bool {
		checks++
		return checks == 1
	})
	if err != nil || checks != 2 || stats.MemoryUsed != nil || stats.CPUUsageUsec != nil || stats.TimestampUnix != nil || stats.MemoryHeadroom != 256<<10 {
		t.Fatal("read crossing exit retained host observation", stats, checks, err)
	}
}

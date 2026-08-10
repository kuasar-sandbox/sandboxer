package resctl

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSetupCgroup_EmptyPath_NoOp(t *testing.T) {
	cg, err := SetupCgroup(CgroupConfig{Path: ""})
	if err != nil {
		t.Fatalf("SetupCgroup with empty path: %v", err)
	}
	if cg == nil {
		t.Fatal("expected non-nil controller, got nil")
	}
	if cg.Path != "" {
		t.Errorf("Path = %q, want empty", cg.Path)
	}
	if cg.Active() {
		t.Error("Active() = true, want false (no-cgroup mode)")
	}
	// Cleanup must also be a no-op (no panic, no error).
	if err := cg.Cleanup(); err != nil {
		t.Errorf("Cleanup: %v", err)
	}
}

func TestSetupCgroup_PathMissing(t *testing.T) {
	_, err := SetupCgroup(CgroupConfig{Path: "/this/path/does/not/exist/abc"})
	if err == nil {
		t.Fatal("expected error for non-existent path, got nil")
	}
	if !strings.Contains(err.Error(), "open") {
		t.Errorf("error %q does not mention open", err.Error())
	}
}

func TestSetupCgroup_PathIsFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := SetupCgroup(CgroupConfig{Path: f})
	if err == nil {
		t.Fatal("expected error for non-directory path, got nil")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error %q does not mention 'not a directory'", err.Error())
	}
}

// TestSetupCgroupForConfig_DefersMemoryHigh verifies the regression fix
// for Issue 4: the convenience entry point used by restore.Run zeroes
// MemoryHighBytes so the boot/replay transient page-fault burst is not
// PSI-throttled. The actual memory.high write is deferred to
// SettledRestore.
func TestSetupCgroupForConfig_DefersMemoryHigh(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"memory.max", "memory.swap.max", "cpu.max", "cpu.weight"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := makeMinimalCfg()
	cfg.Resources.Control.CgroupPath = dir

	cg, err := SetupCgroupForConfig(cfg)
	if err != nil {
		t.Fatalf("SetupCgroupForConfig: %v", err)
	}
	defer cg.Cleanup()
	// memory.high file must NOT have been created. (We didn't pre-create
	// it, and SetupCgroup would error if it tried to write a missing file.)
	if _, err := os.Stat(filepath.Join(dir, "memory.high")); err == nil {
		t.Error("memory.high was written; SetupCgroupForConfig should defer it")
	}
}

func TestCgroupControllerConfiguresAtomicPlacement(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"memory.max", "memory.swap.max", "cpu.max"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cg, err := SetupCgroup(CgroupConfig{Path: dir, MemoryMaxBytes: 1, CPUMaxQuotaUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	attr := &syscall.SysProcAttr{Setpgid: true}
	if err := cg.ConfigureSysProcAttr(attr); err != nil {
		t.Fatal(err)
	}
	if !attr.UseCgroupFD || attr.CgroupFD < 0 || !attr.Setpgid {
		t.Fatalf("process attributes not preserved/configured: %+v", attr)
	}
	if err := cg.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := cg.ConfigureSysProcAttr(&syscall.SysProcAttr{}); err == nil {
		t.Fatal("closed controller accepted process configuration")
	}
}

func TestBuildCgroupConfig_ModeA(t *testing.T) {
	cfg := makeMinimalCfg()
	// makeMinimalCfg leaves CgroupPath empty.
	got, err := BuildCgroupConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "" {
		t.Errorf("no-cgroup mode should yield empty Path, got %q", got.Path)
	}
}

func TestBuildCgroupConfig_ModeB(t *testing.T) {
	dir := t.TempDir()
	cfg := makeMinimalCfg()
	cfg.Resources.Control.CgroupPath = dir
	cfg.Resources.Control.CgroupFD = 9
	// capacity=4GiB, allocatable.memory=2GiB, allocatable.cpu=1.5
	got, err := BuildCgroupConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != dir {
		t.Errorf("Path = %q, want %q", got.Path, dir)
	}
	if got.FD != 9 {
		t.Errorf("FD = %d, want 9", got.FD)
	}
	// memory.max = capacity + overhead (default 32 MiB)
	wantMax := uint64(4<<30) + (32 << 20)
	if got.MemoryMaxBytes != wantMax {
		t.Errorf("MemoryMaxBytes = %d, want %d", got.MemoryMaxBytes, wantMax)
	}
	// memory.high default = allocatable * 0.875
	wantHigh := uint64(float64(2<<30) * 0.875)
	if got.MemoryHighBytes != wantHigh {
		t.Errorf("MemoryHighBytes = %d, want %d", got.MemoryHighBytes, wantHigh)
	}
	// cpu.max = capacity.cpu * 100000us
	if got.CPUMaxQuotaUs != 2*100000 {
		t.Errorf("CPUMaxQuotaUs = %d, want %d", got.CPUMaxQuotaUs, 2*100000)
	}
	// cpu.weight = round(1.5 * 100) = 150
	if got.CPUWeight != 150 {
		t.Errorf("CPUWeight = %d, want 150", got.CPUWeight)
	}
}

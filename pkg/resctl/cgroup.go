package resctl

import (
	"fmt"
	"log"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"golang.org/x/sys/unix"
)

// IMPORTANT: sandbox-ctl NEVER joins the sandbox cgroup itself. Only the CH
// process is created there through CLONE_INTO_CGROUP. Reason: if sandbox-ctl
// were in the same cgroup as CH, hitting memory.high would throttle
// sandbox-ctl too — kernel mem_cgroup_handle_over_high puts the offender
// in TASK_KILLABLE D-state on return-to-user. The Go scheduler then can't
// run any goroutine on that thread, so sandbox-ctl can neither send
// SIGKILL to CH nor reap cmd.Wait — full deadlock observed in density-
// perf forensics. By keeping sandbox-ctl in its parent (host control)
// cgroup, it remains responsive to signals and Go scheduling regardless
// of guest memory pressure.

// CgroupConfig describes the VMM resource cgroup for a sandbox.
//
// Path must point at an EXISTING cgroup directory (e.g. created by an
// orchestrator, systemd unit, or operator script). sandbox-ctl never
// creates the directory and never rmdir on exit — ownership is
// external. Empty Path means "no cgroup operations" (no-cgroup mode in
// docs/sandbox.md §4.1).
//
// MemoryMaxBytes is written verbatim to memory.max. A zero MemoryHighBytes is
// the cold/restore deferred sentinel and writes "max" explicitly, so an
// externally owned cgroup cannot carry a stale throttle into a new VM. A
// positive value is written verbatim. The local Budget controller later
// computes memory.high from Budget, guest demand, overhead, and
// watermark_high.ratio. MemoryMax is
//
//	MemoryMaxBytes = capacity.memory + overhead.memory
//
// CPUMaxQuotaUs is the cpu.max quota; period is fixed 100000us. Set to
//
//	CPUMaxQuotaUs = capacity.cpu * 100000
//
// so the cgroup can burst to capacity when uncontended; cpu.weight does
// the floor enforcement under contention.
//
// CPUWeight is clamp(round(allocatable.cpu * 100), 1, 10000); see
// docs/sandbox.md §9.2.
type CgroupConfig struct {
	Path            string
	FD              int
	MemoryMaxBytes  uint64
	MemoryHighBytes uint64
	CPUMaxQuotaUs   int
	CPUWeight       uint64
}

// CgroupController owns a stable descriptor for the cgroup after limits have
// been written. The descriptor is used to create CH atomically in the cgroup;
// sandbox-ctl itself never enters it (see file header).
type CgroupController struct {
	Path   string
	active bool
	target *os.File
}

// SetupCgroup opens an existing cgroup and writes its resource limits without
// adding the current process. ConfigureSysProcAttr later places CH there at
// process creation time. See the file header for why sandbox-ctl never joins.
//
// Returns a controller with Path set and active=true on success. When
// cfg.Path is empty the function is a no-op and returns a zero-value
// controller (ConfigureSysProcAttr and Cleanup are also no-ops).
//
// The cgroup directory must exist. Failures (path missing, permission
// denied, controller not enabled in subtree_control) are propagated.
// memory.swap.max is best-effort — some kernels lack the swap controller
// and we warn rather than fail.
func SetupCgroup(cfg CgroupConfig) (*CgroupController, error) {
	if cfg.Path == "" {
		return &CgroupController{}, nil
	}

	var target *os.File
	if cfg.FD >= 3 {
		fd, err := unix.FcntlInt(uintptr(cfg.FD), unix.F_DUPFD_CLOEXEC, 3)
		if err != nil {
			return nil, fmt.Errorf("cgroup: duplicate inherited descriptor %d: %w", cfg.FD, err)
		}
		target = os.NewFile(uintptr(fd), "vmm-cgroup")
		if target == nil {
			unix.Close(fd)
			return nil, fmt.Errorf("cgroup: adopt duplicated descriptor %d", fd)
		}
	} else if cfg.FD == 0 {
		var err error
		target, err = os.Open(cfg.Path)
		if err != nil {
			return nil, fmt.Errorf("cgroup: open %q: %w", cfg.Path, err)
		}
	} else {
		return nil, fmt.Errorf("cgroup: inherited descriptor must be >= 3, got %d", cfg.FD)
	}
	fail := func(err error) (*CgroupController, error) {
		_ = target.Close()
		return nil, err
	}
	st, err := target.Stat()
	if err != nil {
		return fail(fmt.Errorf("cgroup: stat %q: %w", cfg.Path, err))
	}
	if !st.IsDir() {
		return fail(fmt.Errorf("cgroup: %q is not a directory", cfg.Path))
	}
	stablePath := fmt.Sprintf("/proc/self/fd/%d", target.Fd())

	if err := writeCgFile(stablePath, "memory.max", strconv.FormatUint(cfg.MemoryMaxBytes, 10)); err != nil {
		return fail(fmt.Errorf("cgroup: memory.max: %w", err))
	}
	memoryHigh := "max"
	if cfg.MemoryHighBytes > 0 {
		memoryHigh = strconv.FormatUint(cfg.MemoryHighBytes, 10)
	}
	if err := writeCgFile(stablePath, "memory.high", memoryHigh); err != nil {
		return fail(fmt.Errorf("cgroup: memory.high: %w", err))
	}
	// Disable swap so cgroup OOM signals are unambiguous. swap.max may
	// be unavailable on hosts compiled without the swap controller; that
	// is acceptable — we want zero swap and absence of the file means
	// effectively zero anyway.
	if err := writeCgFile(stablePath, "memory.swap.max", "0"); err != nil {
		log.Printf("[sandbox-ctl] cgroup: memory.swap.max write skipped: %v", err)
	}

	const period = 100000
	if err := writeCgFile(stablePath, "cpu.max", fmt.Sprintf("%d %d", cfg.CPUMaxQuotaUs, period)); err != nil {
		return fail(fmt.Errorf("cgroup: cpu.max: %w", err))
	}
	if cfg.CPUWeight > 0 {
		if err := writeCgFile(stablePath, "cpu.weight", strconv.FormatUint(cfg.CPUWeight, 10)); err != nil {
			return fail(fmt.Errorf("cgroup: cpu.weight: %w", err))
		}
	}

	return &CgroupController{Path: cfg.Path, active: true, target: target}, nil
}

// Active reports whether limits were written (i.e. cfg.Path was non-empty).
// Callers use it to decide whether to log "cgroup joined" / "no-cgroup mode".
func (c *CgroupController) Active() bool {
	return c != nil && c.active
}

// LocalPath returns a process-local stable path for cgroup file I/O. The path
// remains pinned to the opened cgroup even if its host pathname is renamed or
// replaced, and is valid until Cleanup.
func (c *CgroupController) LocalPath() string {
	if c == nil || !c.active || c.target == nil {
		return ""
	}
	return fmt.Sprintf("/proc/self/fd/%d", c.target.Fd())
}

// ConfigureSysProcAttr makes the child start in the target cgroup via
// clone3(CLONE_INTO_CGROUP). No-op in no-cgroup mode.
func (c *CgroupController) ConfigureSysProcAttr(attr *syscall.SysProcAttr) error {
	if c == nil || !c.active {
		return nil
	}
	if attr == nil {
		return fmt.Errorf("cgroup: nil process attributes")
	}
	if c.target == nil {
		return fmt.Errorf("cgroup: target descriptor is closed")
	}
	attr.UseCgroupFD = true
	attr.CgroupFD = int(c.target.Fd())
	return nil
}

func writeCgFile(dir, name, value string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Cleanup closes the stable target descriptor. The externally owned cgroup
// directory is intentionally left in place.
func (c *CgroupController) Cleanup() error {
	if c == nil || c.target == nil {
		return nil
	}
	err := c.target.Close()
	c.target = nil
	return err
}

// SetupCgroupForConfig is the convenience entry point used by restore.Run.
// It derives the externally owned VMM cgroup limits without joining
// sandbox-ctl. Deferred memory.high is reset to max until MemoryController has
// a fresh guest report and CH observation. Returns a zero-value controller
// (Cleanup no-op) when CgroupPath is unset (no-cgroup mode).
func SetupCgroupForConfig(cfg *config.SandboxConfig) (*CgroupController, error) {
	cgCfg, err := BuildCgroupConfig(cfg)
	if err != nil {
		return nil, err
	}
	cgCfg.MemoryHighBytes = 0
	return SetupCgroup(cgCfg)
}

// BuildCgroupConfig translates a config.SandboxConfig into a CgroupConfig.
// Returns a zero-Path config when CgroupPath is unset (no-cgroup mode); SetupCgroup
// will then no-op.
//
// Memory.max derives from capacity + overhead. A deferred memory.high is
// represented by zero here and written as max by SetupCgroup until the local
// Budget controller has a trusted guest/CH observation.
// cpu.max = capacity * 100000us per 100000us period; cpu.weight derives from
// allocatable.cpu.
func BuildCgroupConfig(cfg *config.SandboxConfig) (CgroupConfig, error) {
	if cfg.Resources.Control.CgroupPath == "" {
		return CgroupConfig{}, nil
	}
	capMem, err := cfg.CapacityMemoryBytes()
	if err != nil {
		return CgroupConfig{}, fmt.Errorf("capacity.memory: %w", err)
	}
	overhead, err := cfg.OverheadMemoryBytes()
	if err != nil {
		return CgroupConfig{}, fmt.Errorf("overhead.memory: %w", err)
	}
	if _, err := cfg.WatermarkHighRatio(); err != nil {
		return CgroupConfig{}, fmt.Errorf("watermark_high.ratio: %w", err)
	}
	memoryMax, carry := bits.Add64(capMem, overhead, 0)
	if carry != 0 {
		return CgroupConfig{}, fmt.Errorf("capacity.memory + overhead.memory overflows uint64")
	}
	return CgroupConfig{
		Path:            cfg.Resources.Control.CgroupPath,
		FD:              cfg.Resources.Control.CgroupFD,
		MemoryMaxBytes:  memoryMax,
		MemoryHighBytes: 0,
		CPUMaxQuotaUs:   cfg.Resources.Capacity.CPU * 100000,
		CPUWeight:       cfg.CPUWeight(),
	}, nil
}

package resctl

import (
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// IMPORTANT: sandbox-ctl NEVER joins the sandbox cgroup itself. Only the CH
// process is moved in (via AddPID after cmd.Start). Reason: if sandbox-ctl
// were in the same cgroup as CH, hitting memory.high would throttle
// sandbox-ctl too — kernel mem_cgroup_handle_over_high puts the offender
// in TASK_KILLABLE D-state on return-to-user. The Go scheduler then can't
// run any goroutine on that thread, so sandbox-ctl can neither send
// SIGKILL to CH nor reap cmd.Wait — full deadlock observed in density-
// perf forensics. By keeping sandbox-ctl in its parent (host control)
// cgroup, it remains responsive to signals and Go scheduling regardless
// of guest memory pressure.

// CgroupConfig describes the cgroup join target for a sandbox.
//
// Path must point at an EXISTING cgroup directory (e.g. created by an
// orchestrator, systemd unit, or operator script). sandbox-ctl never
// creates the directory and never rmdir on exit — ownership is
// external. Empty Path means "no cgroup operations" (no-cgroup mode in
// docs/sandbox.md §4.1).
//
// MemoryMaxBytes / MemoryHighBytes are written verbatim to memory.max /
// memory.high. The caller computes them from
//
//	MemoryMaxBytes  = capacity.memory + overhead.memory
//	MemoryHighBytes = watermark_high.memory   (default allocatable * 0.875)
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
	MemoryMaxBytes  uint64
	MemoryHighBytes uint64
	CPUMaxQuotaUs   int
	CPUWeight       uint64
	// Adopt: Path is the cgroup sandbox-ctl is already a member of (its
	// systemd unit's cgroup). Limits are written to it, but AddPID is a
	// no-op — CH inherits membership as a forked child. See cgroup.go header
	// for the throttle-deadlock this re-exposes; caller opts in via --cgroup-adopt.
	Adopt bool
}

// CgroupController holds the cgroup path after limits have been written.
// Callers use AddPID to move the CH process in after exec.Start.
// sandbox-ctl itself is never moved into the cgroup (see file header).
type CgroupController struct {
	Path   string
	active bool
	adopt  bool
}

// JoinCgroup writes resource limits to an existing cgroup but does NOT
// add the current process. The caller is responsible for moving CH
// (and only CH) into the cgroup via AddPID after exec.Start. See file
// header for why sandbox-ctl never joins.
//
// Name is preserved for compatibility — "Setup" might read more
// accurately given the new semantics, but renaming would churn callers
// without clarifying intent (the comment block does).
//
// Returns a controller with Path set and active=true on success. When
// cfg.Path is empty the function is a no-op and returns a zero-value
// controller (AddPID and Cleanup are also no-ops).
//
// The cgroup directory must exist. Failures (path missing, permission
// denied, controller not enabled in subtree_control) are propagated.
// memory.swap.max is best-effort — some kernels lack the swap controller
// and we warn rather than fail.
func JoinCgroup(cfg CgroupConfig) (*CgroupController, error) {
	if cfg.Path == "" {
		return &CgroupController{}, nil
	}

	st, err := os.Stat(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("cgroup: stat %q: %w", cfg.Path, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("cgroup: %q is not a directory", cfg.Path)
	}

	if err := writeCgFile(cfg.Path, "memory.max", strconv.FormatUint(cfg.MemoryMaxBytes, 10)); err != nil {
		return nil, fmt.Errorf("cgroup: memory.max: %w", err)
	}
	if cfg.MemoryHighBytes > 0 {
		if err := writeCgFile(cfg.Path, "memory.high", strconv.FormatUint(cfg.MemoryHighBytes, 10)); err != nil {
			return nil, fmt.Errorf("cgroup: memory.high: %w", err)
		}
	}
	// Disable swap so cgroup OOM signals are unambiguous. swap.max may
	// be unavailable on hosts compiled without the swap controller; that
	// is acceptable — we want zero swap and absence of the file means
	// effectively zero anyway.
	if err := writeCgFile(cfg.Path, "memory.swap.max", "0"); err != nil {
		log.Printf("[sandbox-ctl] cgroup: memory.swap.max write skipped: %v", err)
	}

	const period = 100000
	if err := writeCgFile(cfg.Path, "cpu.max", fmt.Sprintf("%d %d", cfg.CPUMaxQuotaUs, period)); err != nil {
		return nil, fmt.Errorf("cgroup: cpu.max: %w", err)
	}
	if cfg.CPUWeight > 0 {
		if err := writeCgFile(cfg.Path, "cpu.weight", strconv.FormatUint(cfg.CPUWeight, 10)); err != nil {
			return nil, fmt.Errorf("cgroup: cpu.weight: %w", err)
		}
	}

	return &CgroupController{Path: cfg.Path, active: true, adopt: cfg.Adopt}, nil
}

// SelfCgroupV2Path returns the absolute cgroup-v2 directory the current
// process is a member of, read from /proc/self/cgroup ("0::<path>"). Used by
// --cgroup-adopt to point CgroupPath at the launching systemd unit's cgroup.
func SelfCgroupV2Path() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("cgroup: read /proc/self/cgroup: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		// cgroup v2 unified hierarchy line is "0::<path>".
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", rest), nil
		}
	}
	return "", fmt.Errorf("cgroup: no cgroup-v2 (0::) entry in /proc/self/cgroup")
}

// Active reports whether limits were written (i.e. cfg.Path was non-empty).
// Callers use it to decide whether to log "cgroup joined" / "no-cgroup mode".
func (c *CgroupController) Active() bool {
	return c != nil && c.active
}

// AddPID moves the given process into the cgroup. Intended for CH right
// after exec.Start. No-op when the controller is inactive (no-cgroup mode).
//
// Race window: between exec.Start and AddPID the CH child runs briefly in
// sandbox-ctl's parent cgroup. CH at that point has only allocated tiny
// startup pages, so the window is harmless in practice.
func (c *CgroupController) AddPID(pid int) error {
	if c == nil || !c.active {
		return nil
	}
	if c.adopt {
		// Adopt mode: the cgroup is sandbox-ctl's own; CH is a forked child
		// and already a member. Moving it would be a no-op at best.
		return nil
	}
	if err := writeCgFile(c.Path, "cgroup.procs", strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("cgroup: add pid %d: %w", pid, err)
	}
	return nil
}

func writeCgFile(dir, name, value string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Cleanup is now a no-op. Previously moved sandbox-ctl back to the root
// cgroup, but sandbox-ctl never joins the per-sandbox cgroup any more
// (only CH does, via AddPID). CH itself either exits cleanly (kernel
// removes it from the cgroup) or is moved to root by external supervision.
// The cgroup directory is never rmdir'd here — ownership is external.
func (c *CgroupController) Cleanup() error {
	return nil
}

// JoinCgroupForConfig is the convenience entry point used by restore.Run.
// Derives a CgroupConfig from a config.SandboxConfig and joins, but with
// MemoryHighBytes zeroed so the boot-transient page-fault burst is not
// PSI-throttled — Settled/SettledRestore writes memory.high once the
// transient is over (Issue 4). Returns a zero-value controller (Cleanup
// no-op) when CgroupPath is unset (no-cgroup mode).
func JoinCgroupForConfig(cfg *config.SandboxConfig) (*CgroupController, error) {
	cgCfg, err := BuildCgroupConfig(cfg)
	if err != nil {
		return nil, err
	}
	cgCfg.MemoryHighBytes = 0
	return JoinCgroup(cgCfg)
}

// BuildCgroupConfig translates a config.SandboxConfig into a CgroupConfig.
// Returns a zero-Path config when CgroupPath is unset (no-cgroup mode); JoinCgroup
// will then no-op.
//
// Memory.max derives from capacity + overhead so guest legitimate use
// up to allocatable does not cgroup-OOM the CH process. Memory.high is
// the watermark (default allocatable * 0.875). cpu.max = capacity *
// 100000us per 100000us period; cpu.weight from allocatable.cpu.
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
	wm, err := cfg.WatermarkHighBytes()
	if err != nil {
		return CgroupConfig{}, fmt.Errorf("watermark_high.memory: %w", err)
	}
	return CgroupConfig{
		Path:            cfg.Resources.Control.CgroupPath,
		MemoryMaxBytes:  capMem + overhead,
		MemoryHighBytes: wm,
		CPUMaxQuotaUs:   cfg.Resources.Capacity.CPU * 100000,
		CPUWeight:       cfg.CPUWeight(),
		Adopt:           cfg.Resources.Control.Adopt,
	}, nil
}

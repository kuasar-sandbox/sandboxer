// cgroup v2 wiring for sandbox-init: application namespace/delegation and the
// snapshot freezer.
//
// The real /sys/fs/cgroup/app cgroup is always the application cgroup
// namespace root and the recursive snapshot freeze root. launch.cgroup_control
// selects the process layout below it:
//
//	false: application processes live directly in real /app and see 0::/
//	true:  real /app stays empty; sandbox-init-managed application processes
//	       live in real /app/init and see 0::/init
//
// sandbox-init stays in the guest-global cgroup root. The first primary child
// is atomically cloned into /app, creates the application cgroup namespace,
// mounts its scoped cgroupfs, and (true only) performs the design's sole
// cgroup.procs write to move itself into /init. sandbox-init pins that
// namespace and the cgroup directory FDs for every later primary restart,
// plugin, and native exec clone.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	cgroupMountPoint = "/sys/fs/cgroup"
	appCgroupDir     = cgroupMountPoint + "/app"
	appInitCgroupDir = appCgroupDir + "/init"
	cgroupFreezeFile = appCgroupDir + "/cgroup.freeze"
	cgroupEventsFile = appCgroupDir + "/cgroup.events"
	appCgroupProcs   = appCgroupDir + "/cgroup.procs"

	// This path is evaluated only inside the first primary's scoped cgroup
	// mount. It resolves to real /app/init/cgroup.procs and is the only
	// cgroup.procs file this implementation writes.
	scopedInitCgroupProcs = cgroupMountPoint + "/init/cgroup.procs"

	// freezeConfirmTimeout bounds the wait for cgroup.events:frozen 1 after
	// writing cgroup.freeze=1. It stays inside proto.DeadlineQuiesce so sync,
	// transport teardown, and the response retain time to complete.
	freezeConfirmTimeout = 3 * time.Second
	freezePollInterval   = 5 * time.Millisecond
)

// applicationCgroups owns the long-lived capabilities sandbox-init needs for
// atomic placement and namespace joining. Every descriptor is O_CLOEXEC and is
// passed to re-exec helpers only through an explicit ExtraFiles slot.
var applicationCgroups struct {
	sync.RWMutex
	control   bool
	root      *os.File // real /app (first-primary CLONE_INTO_CGROUP target)
	target    *os.File // real /app or /app/init (all later clone target)
	namespace *os.File // first primary's pinned cgroup namespace
}

// cgroupMount mounts the guest-global cgroup v2 hierarchy, propagates root
// controllers, creates /app and optional /app/init, and pins both the root and
// final-target directory FDs. Called once from phase 1 after switch-root.
//
// cgroup_control=true is a delegation contract: every controller advertised at
// the guest root must be enabled and observed in subtree_control. false keeps
// the historical best-effort root propagation because no application subtree
// is promised in that mode.
func cgroupMount(control bool) error {
	if err := unix.Mount("cgroup2", cgroupMountPoint, "cgroup2", 0, ""); err != nil {
		if !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("mount cgroup2 on %s: %w", cgroupMountPoint, err)
		}
	}
	if err := cgroupEnableAdvertisedControllers(cgroupMountPoint); err != nil {
		if control {
			return fmt.Errorf("enable guest-root controllers: %w", err)
		}
		logf("cgroup: guest-root controller delegation unavailable (continuing without application delegation): %v", err)
	}
	if err := os.Mkdir(appCgroupDir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("mkdir %s: %w", appCgroupDir, err)
	}
	if control {
		if err := os.Mkdir(appInitCgroupDir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("mkdir %s: %w", appInitCgroupDir, err)
		}
	}

	root, err := openCgroupDir(appCgroupDir)
	if err != nil {
		return fmt.Errorf("open application cgroup root: %w", err)
	}
	targetPath := appCgroupDir
	if control {
		targetPath = appInitCgroupDir
	}
	target, err := openCgroupDir(targetPath)
	if err != nil {
		_ = root.Close()
		return fmt.Errorf("open application cgroup target: %w", err)
	}

	applicationCgroups.Lock()
	defer applicationCgroups.Unlock()
	if applicationCgroups.root != nil || applicationCgroups.target != nil || applicationCgroups.namespace != nil {
		_ = root.Close()
		_ = target.Close()
		return errors.New("application cgroups already initialized")
	}
	applicationCgroups.control = control
	applicationCgroups.root = root
	applicationCgroups.target = target
	return nil
}

func openCgroupDir(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// cgroupConfigureClone adds CLONE_INTO_CGROUP to one os/exec launch. The first
// primary always starts in real /app and unshares a fresh cgroup namespace in
// the child side of ForkExec. Every later launch starts directly in the final
// target and joins the already-pinned namespace in its re-exec helper.
func cgroupConfigureClone(attr *syscall.SysProcAttr, firstPrimary bool) error {
	if attr == nil {
		return errors.New("nil SysProcAttr")
	}
	applicationCgroups.RLock()
	defer applicationCgroups.RUnlock()
	target := applicationCgroups.target
	if firstPrimary {
		target = applicationCgroups.root
	}
	if target == nil {
		return errors.New("application cgroup target is not initialized")
	}
	if !firstPrimary && applicationCgroups.namespace == nil {
		return errors.New("application cgroup namespace is not initialized")
	}
	attr.UseCgroupFD = true
	attr.CgroupFD = int(target.Fd())
	if firstPrimary {
		// Go performs this unshare in the single-threaded fork child, after
		// clone3(CLONE_INTO_CGROUP) and before re-execing sandbox-init. That
		// ordering makes real /app the namespace root without a helper anchor.
		attr.Unshareflags |= unix.CLONE_NEWCGROUP
	}
	return nil
}

// cgroupNamespaceFile returns sandbox-init's pinned application cgroup
// namespace capability for an explicit ExtraFiles handoff.
func cgroupNamespaceFile() (*os.File, error) {
	applicationCgroups.RLock()
	defer applicationCgroups.RUnlock()
	if applicationCgroups.namespace == nil {
		return nil, errors.New("application cgroup namespace is not initialized")
	}
	return applicationCgroups.namespace, nil
}

// cgroupControlEnabled reports the immutable launch topology selected at boot.
func cgroupControlEnabled() bool {
	applicationCgroups.RLock()
	defer applicationCgroups.RUnlock()
	return applicationCgroups.control
}

// cgroupFinalizeBootstrap pins the first primary's cgroup namespace while it
// is blocked in the internal handshake. In delegation mode it then proves
// real /app has no direct processes and strictly enables every controller that
// /app advertises before allowing the child to exec the final application.
func cgroupFinalizeBootstrap(pid int) error {
	path := fmt.Sprintf("/proc/%d/ns/cgroup", pid)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("pin %s: %w", path, err)
	}
	ns := os.NewFile(uintptr(fd), path)
	fail := func(err error) error {
		_ = ns.Close()
		return err
	}

	if cgroupControlEnabled() {
		procs, err := os.ReadFile(appCgroupProcs)
		if err != nil {
			return fail(fmt.Errorf("verify application root processes: %w", err))
		}
		if strings.TrimSpace(string(procs)) != "" {
			return fail(fmt.Errorf("verify application root processes: %s is not empty", appCgroupProcs))
		}
		if err := cgroupEnableAdvertisedControllers(appCgroupDir); err != nil {
			return fail(fmt.Errorf("enable application-root controllers: %w", err))
		}
	}

	applicationCgroups.Lock()
	defer applicationCgroups.Unlock()
	if applicationCgroups.namespace != nil {
		return fail(errors.New("application cgroup namespace already pinned"))
	}
	applicationCgroups.namespace = ns
	return nil
}

// cgroupEnableAdvertisedControllers enables every name currently listed in
// dir/cgroup.controllers and verifies each one appears in the readback of
// dir/cgroup.subtree_control. A delegation request never silently degrades.
func cgroupEnableAdvertisedControllers(dir string) error {
	return cgroupEnableAdvertisedControllersWith(dir, os.ReadFile, os.WriteFile)
}

func cgroupEnableAdvertisedControllersWith(
	dir string,
	read func(string) ([]byte, error),
	write func(string, []byte, os.FileMode) error,
) error {
	controllersPath := dir + "/cgroup.controllers"
	subtreePath := dir + "/cgroup.subtree_control"
	available, err := read(controllersPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", controllersPath, err)
	}
	controllers := strings.Fields(string(available))
	if len(controllers) == 0 {
		return nil
	}
	enable := "+" + strings.Join(controllers, " +")
	if err := write(subtreePath, []byte(enable), 0); err != nil {
		return fmt.Errorf("write %s %q: %w", subtreePath, enable, err)
	}
	enabled, err := read(subtreePath)
	if err != nil {
		return fmt.Errorf("read back %s: %w", subtreePath, err)
	}
	if missing := missingControllers(controllers, strings.Fields(string(enabled))); len(missing) > 0 {
		return fmt.Errorf("verify %s: controllers not enabled: %s", subtreePath, strings.Join(missing, ","))
	}
	return nil
}

func missingControllers(want, got []string) []string {
	set := make(map[string]struct{}, len(got))
	for _, controller := range got {
		controller = strings.TrimLeft(controller, "+-")
		set[controller] = struct{}{}
	}
	var missing []string
	for _, controller := range want {
		if _, ok := set[controller]; !ok {
			missing = append(missing, controller)
		}
	}
	return missing
}

// cgroupJoinNamespace joins the first primary's pinned cgroup namespace. It is
// called only by short-lived, OS-thread-locked re-exec helpers.
func cgroupJoinNamespace(fd int) error {
	return cgroupJoinNamespaceWith(fd, unix.Setns)
}

func cgroupJoinNamespaceWith(fd int, setns func(int, int) error) error {
	if fd < 0 {
		return fmt.Errorf("invalid cgroup namespace fd %d", fd)
	}
	if err := setns(fd, unix.CLONE_NEWCGROUP); err != nil {
		return fmt.Errorf("setns cgroup: %w", err)
	}
	return nil
}

// cgroupMountScoped replaces the inherited guest-global cgroup2 mount in a
// child's private, rslave mount namespace. Because the calling task is already
// in the application cgroup namespace, the new mount exposes real /app as /.
func cgroupMountScoped() error {
	return cgroupMountScopedWith(unix.Unmount, unix.Mount)
}

func cgroupMountScopedWith(
	unmount func(string, int) error,
	mount func(string, string, string, uintptr, string) error,
) error {
	if err := unmount(cgroupMountPoint, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount inherited cgroup2 from %s: %w", cgroupMountPoint, err)
	}
	if err := mount("cgroup2", cgroupMountPoint, "cgroup2", 0, ""); err != nil {
		return fmt.Errorf("mount scoped cgroup2 on %s: %w", cgroupMountPoint, err)
	}
	return nil
}

// cgroupMoveBootstrapToInit performs the one explicitly permitted
// cgroup.procs write: the first primary moves itself from virtual / to /init
// after its scoped cgroupfs is mounted. All later starts use
// CLONE_INTO_CGROUP on the final target and never migrate after birth.
func cgroupMoveBootstrapToInit() error {
	return cgroupMoveBootstrapToInitWith(os.WriteFile)
}

func cgroupMoveBootstrapToInitWith(write func(string, []byte, os.FileMode) error) error {
	if err := write(scopedInitCgroupProcs, []byte("0"), 0); err != nil {
		return fmt.Errorf("move bootstrap child to /init: %w", err)
	}
	return nil
}

// cgroupFreeze freezes the complete real /app subtree and blocks until the
// kernel confirms cgroup.events:frozen 1, or freezeConfirmTimeout elapses.
func cgroupFreeze() error {
	if err := os.WriteFile(cgroupFreezeFile, []byte("1"), 0); err != nil {
		return fmt.Errorf("write cgroup.freeze=1: %w", err)
	}
	deadline := time.Now().Add(freezeConfirmTimeout)
	for {
		frozen, err := cgroupFrozen()
		if err != nil {
			return err
		}
		if frozen {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cgroup.events frozen 1 not observed within %s", freezeConfirmTimeout)
		}
		time.Sleep(freezePollInterval)
	}
}

// cgroupThaw unfreezes the complete real /app subtree. Writing 0 is
// idempotent, so restore and attach can call it unconditionally.
func cgroupThaw() error {
	if err := os.WriteFile(cgroupFreezeFile, []byte("0"), 0); err != nil {
		return fmt.Errorf("write cgroup.freeze=0: %w", err)
	}
	return nil
}

// cgroupFrozen reports whether real /app currently reads frozen 1.
func cgroupFrozen() (bool, error) {
	b, err := os.ReadFile(cgroupEventsFile)
	if err != nil {
		return false, fmt.Errorf("read cgroup.events: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "frozen" {
			return f[1] == "1", nil
		}
	}
	return false, nil
}

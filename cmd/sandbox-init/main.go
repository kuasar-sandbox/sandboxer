// sandbox-init is the guest PID 1 binary inside the sandbox VM.
//
// It runs three phases (docs/sandbox-init.md §3):
//  1. Bring up loopback + bind AF_VSOCK :5000 (socket-only, before hello;
//     the host→guest reverse channel must exist before the host's ping
//     ticker fires). Then run the launch handshake (dial CID 2:5000 → hello
//     → recv launch spec) in a goroutine CONCURRENT with the spec-independent
//     overlay assembly (mount /proc /sys /dev, wait vda/vdb, mount overlay →
//     /sysroot). The handshake touches no path, so it is safe alongside
//     the mount chain and the later chroot. After the join: set up volume
//     (empty) mounts on the raw ext4 pre-switch, then MS_MOVE + chroot into
//     the overlay and mount the post-switch base filesystems (devpts, cgroup
//     v2, /run, /run/shm).
//  2. Apply the launch spec on the post-switch rootfs: network, tmpfs mounts,
//     file injection, one-shot init — then set up the app's stdio and send
//     launch_ack{stdio} (its connection becomes the stdio MUX). Fork the user
//     app with CLONE_NEWPID|CLONE_NEWNS, dropping to launch.user just before
//     execve, and send app_started{pid}.
//  3. Supervise: in parallel, the vsock listener goroutine dispatches
//     host-initiated ping / restore / attach / quiesce; the signal loop
//     reaps children. On user-app exit we drain the stdio MUX, send
//     app_exited{code,term_signal}, then reboot.
//
// All work is done via syscalls; no busybox or external tools are
// included in sandbox-runtime.bundle.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

const (
	devicePollInterval = 50 * time.Millisecond
	devicePollTimeout  = 10 * time.Second
	gracefulShutdown   = 10 * time.Second

	// memReportInterval is the period of the /proc/meminfo sampler
	// that feeds the host-side balloon controller. Matches the
	// host-side reconcile cadence; at this rate the worst-case
	// reclaim latency is ~2 × interval.
	memReportInterval = 5 * time.Second
)

func main() {
	// Re-entry as the user-app exec helper (after fork+clone in phase 2).
	// argv: [self, "exec-child", isolated("1"/"0"), cred, workdir, appPath, args...]
	// isolated reports whether the app got its own PID ns (→ remount /proc);
	// cred ("uid:gid:sg,..." or "-") is dropped just before execve.
	if len(os.Args) >= 6 && os.Args[1] == "exec-child" {
		runExecChild(os.Args[2] == "1", os.Args[3], os.Args[4], os.Args[5], os.Args[6:], false, false)
		return
	}
	// Placeholder app (launch.placeholder): same ns/cred setup as exec-child
	// but no program — it waits for SIGTERM/SIGINT instead of execve.
	// argv: [self, "exec-child-placeholder", isolated("1"/"0"), cred, workdir]
	if len(os.Args) >= 5 && os.Args[1] == "exec-child-placeholder" {
		runExecChild(os.Args[2] == "1", os.Args[3], os.Args[4], "", nil, false, true)
		return
	}
	// Joined exec command (sandbox-ctl exec): forked by the exec-join
	// helper after it has entered the app's mount + pid namespaces. cred
	// was resolved by the helper inside the app ns ("-" = keep root).
	if len(os.Args) >= 5 && os.Args[1] == "exec-child-joined" {
		runExecChild(false, os.Args[2], os.Args[3], os.Args[4], os.Args[5:], true, false)
		return
	}
	// nsenter helper for sandbox-ctl exec.
	// argv: [self, "exec-join", appPid, tty(0|1), user|-, cwd, argv0, args...]
	if len(os.Args) >= 7 && os.Args[1] == "exec-join" {
		runExecJoin(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6], os.Args[7:])
		return
	}

	logf("sandbox-init starting (pid=%d)", os.Getpid())
	if os.Getpid() != 1 {
		die("must run as PID 1, got %d", os.Getpid())
	}

	// The reverse-channel listener is socket-only (no rootfs dependency) and
	// MUST be bound before hello — the host starts its ping ticker the moment
	// it writes `launch`, so the bound socket has to exist or that ping hits
	// ECONNREFUSED. Bind it first so the launch handshake can run concurrently
	// with overlay assembly. (Loopback comes later — it reads sysfs.)
	revFD, err := bindVsockListener(proto.LaunchPort)
	if err != nil {
		die("vsock listen: %v", err)
	}

	// Launch handshake (dial → hello → recv launch) runs in a goroutine
	// concurrent with overlay assembly. It is pure-socket — it resolves no
	// filesystem path — so it is safe alongside the mount chain and the
	// later chroot (which runs single-threaded after the join). This hides
	// the hello→launch round-trip under the disk/overlay setup. The spec
	// drives the spec-dependent setup below; the conn is reused for
	// launch_ack and then the stdio MUX.
	hsCh := make(chan handshakeResult, 1)
	go runHandshake(hsCh)

	// Spec-independent root assembly (base mounts + overlay/single → sysroot).
	if err := phase1aAssembleRoot(); err != nil {
		die("phase 1a root assembly: %v", err)
	}

	hr := <-hsCh
	if hr.err != nil {
		die("launch handshake: %v", hr.err)
	}
	spec, conn := hr.spec, hr.conn
	logf("phase1: launch spec received; switching root")

	// Volume (empty) mounts are set up BEFORE switch-root: their source is a
	// fresh dir on the raw ext4 (/overlay/upper/volumes), bound onto the target
	// inside /sysroot so the switch-root MS_MOVE carries them into /.
	if err := applyVolumeMounts(spec.Mounts); err != nil {
		die("apply volume mounts: %v", err)
	}

	// switch-root into the assembled overlay + post-switch base mounts.
	if err := phase1bSwitchRoot(); err != nil {
		die("phase 1b switch-root: %v", err)
	}

	// Loopback reads /sys/class/net (sysfs), so it runs after switch-root.
	// Config-independent; only the app cares about localhost, so bringing it
	// up here (rather than before the handshake) is fine. Fatal: a guest that
	// can't bring up lo is broken.
	if err := bringUpLoopback(); err != nil {
		die("bring up loopback: %v", err)
	}

	// Apply the spec on the post-switch rootfs and finish the handshake
	// (launch_ack → MUX). applyNetwork needs /sys, available after chroot.
	cs, bridge, err := phase2Apply(spec, conn)
	if err != nil {
		die("phase 2 apply: %v", err)
	}
	logf("phase2: network + stdio done")

	appPid, err := phase2ForkApp(spec, cs)
	if err != nil {
		die("phase 2 fork: %v", err)
	}
	logf("phase2: app forked pid=%d", appPid)

	// Move the app tree into the `app` cgroup so quiesce can freeze it.
	// Fatal on failure: a pid left in the root cgroup would not freeze,
	// silently defeating the snapshot freeze (§3.4).
	if err := cgroupPlaceApp(appPid); err != nil {
		die("cgroup place app pid=%d: %v", appPid, err)
	}

	// Notify host before entering supervisor — best-effort short conn.
	if err := notifyAppStarted(appPid); err != nil {
		logf("warn: app_started notify failed (continuing): %v", err)
	}

	supervisor := &supervisorState{
		spec:       spec,
		stopSignal: syscall.Signal(spec.StopSignal), // 0 → SIGTERM (handled in phase3)
		stopGrace:  time.Duration(spec.StopGraceSec) * time.Second,
		execReg:    newExecRegistry(),
		connReg:    newConnRegistry(),
		acceptLn:   newAcceptListeners(),
		pluginReg:  newPluginRegistry(),
	}
	supervisor.appPid.Store(int64(appPid))
	supervisor.appBackoff.onStart(time.Now()) // app start instant for restart backoff reset

	// Launch companion plugins (launch.plugin[]) once the app is up + in its
	// cgroup; each is supervised independently and never reboots the sandbox.
	supervisor.pluginReg.start(spec.Plugins)

	// Reverse-channel dispatch goroutine. Lives until reboot. It carries
	// the consoleBridge so host-initiated restore / attach can swap a
	// fresh MUX session under the still-running app's stdio pumps, and
	// the supervisor so exec sessions can register their children.
	go serveReverseChannel(revFD, supervisor, bridge)

	// Memory reporter: feeds the host-side balloon controller with
	// /proc/meminfo snapshots so it can drive vm.resize. Replaces
	// virtio-balloon free-page-reporting (whose mmu_notifier traffic
	// starves this very vsock listener after ~16 s).
	go runMemReporter(memReportInterval)

	phase3Supervise(supervisor, bridge)
	// phase3Supervise does not return.
}

// singleDiskRoot records the disk mode resolved from /proc/cmdline in phase1a
// (single-disk vs two-disk overlay). Read later by applyVolumeMounts to place
// `empty` volume sources correctly. The guest is a single process, so a package
// var is adequate.
var singleDiskRoot bool

// singleDiskFromCmdline reports whether the kernel cmdline selects single-disk
// mode (sandbox.root.layout=single, set by the host's CHCommand). /proc must be
// mounted. Absent ⇒ two-disk overlay mode (the default).
func singleDiskFromCmdline() bool {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		logf("phase1a: read /proc/cmdline: %v (assuming overlay mode)", err)
		return false
	}
	for _, tok := range strings.Fields(string(b)) {
		if tok == "sandbox.root.layout=single" {
			return true
		}
	}
	return false
}

// phase1aAssembleRoot mounts /proc /sys /dev, then assembles the container root
// at /sysroot per disk mode (read from /proc/cmdline — the mode must be known
// before the launch spec arrives, so it rides the kernel cmdline):
//
//   - overlay mode (default): wait vda+vdb, mount the erofs base (vda, ro) as
//     the overlayfs lower over the ext4 upper (vdb, rw), overlay → /sysroot.
//   - single-disk mode: wait vda only, mount it (ext4, rw) directly as /sysroot
//     — no overlayfs, no vdb.
//
// Finally it binds the guest-side runtime payload (/opt/sandbox-runtime) into
// /sysroot. Spec-independent, so it runs concurrently with the launch
// handshake; the chroot is deferred to phase1bSwitchRoot (after the join),
// where it can run single-threaded.
func phase1aAssembleRoot() error {
	for _, m := range []struct {
		source, target, fstype string
		flags                  uintptr
	}{
		{"proc", "/proc", "proc", 0},
		{"sysfs", "/sys", "sysfs", 0},
		{"devtmpfs", "/dev", "devtmpfs", 0},
	} {
		if err := unix.Mount(m.source, m.target, m.fstype, m.flags, ""); err != nil {
			if errors.Is(err, unix.EBUSY) {
				continue
			}
			return fmt.Errorf("mount %s on %s: %w", m.source, m.target, err)
		}
	}

	singleDiskRoot = singleDiskFromCmdline() // /proc is mounted now
	if singleDiskRoot {
		logf("phase1a: base mounts done; single-disk mode, waiting for vda")
		if err := waitForDevice("/dev/vda", devicePollTimeout); err != nil {
			return fmt.Errorf("wait /dev/vda: %w", err)
		}
		// The single root disk is a writable ext4 CoW; mount it directly.
		if err := unix.Mount("/dev/vda", "/sysroot", "ext4", 0, ""); err != nil {
			return fmt.Errorf("mount blk0 (ext4 rw) on /sysroot: %w", err)
		}
		logf("phase1a: single-disk root mounted at /sysroot")
	} else {
		logf("phase1a: base mounts done; waiting for vda/vdb")
		if err := waitForDevice("/dev/vda", devicePollTimeout); err != nil {
			return fmt.Errorf("wait /dev/vda: %w", err)
		}
		if err := waitForDevice("/dev/vdb", devicePollTimeout); err != nil {
			return fmt.Errorf("wait /dev/vdb: %w", err)
		}
		logf("phase1a: vda+vdb present")

		if err := unix.Mount("/dev/vda", "/overlay/lower", "erofs", unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("mount blk0 (erofs ro) on /overlay/lower: %w", err)
		}
		if err := unix.Mount("/dev/vdb", "/overlay/upper", "ext4", 0, ""); err != nil {
			return fmt.Errorf("mount blk1 (ext4 rw) on /overlay/upper: %w", err)
		}
		if err := os.MkdirAll("/overlay/upper/upperdir", 0o755); err != nil {
			return fmt.Errorf("mkdir upperdir: %w", err)
		}
		if err := os.MkdirAll("/overlay/upper/workdir", 0o755); err != nil {
			return fmt.Errorf("mkdir workdir: %w", err)
		}

		overlayOpts := "lowerdir=/overlay/lower,upperdir=/overlay/upper/upperdir,workdir=/overlay/upper/workdir"
		if err := unix.Mount("overlay", "/sysroot", "overlay", 0, overlayOpts); err != nil {
			return fmt.Errorf("mount overlay on /sysroot: %w", err)
		}
		logf("phase1a: overlay assembled at /sysroot")
	}

	for _, dir := range []string{"/sysroot/proc", "/sysroot/sys", "/sysroot/dev"} {
		_ = os.MkdirAll(dir, 0o755)
	}

	// Project the guest-side runtime payload (/opt/sandbox-runtime, shipped in
	// the pmem rootfs) into the new root. The source lives on the pmem EROFS,
	// which becomes unreachable after switch-root, so it must be bound into
	// /sysroot HERE — phase1b's MS_MOVE /sysroot → / then carries it into the
	// new / along the same subtree (exactly like the empty-volume binds). The
	// bind keeps the pmem inodes referenced after the original mount is hidden;
	// the source is a read-only EROFS, so the bind is inherently read-only.
	if err := os.MkdirAll("/sysroot/opt/sandbox-runtime", 0o755); err != nil {
		return fmt.Errorf("mkdir sysroot opt/sandbox-runtime: %w", err)
	}
	if err := unix.Mount("/opt/sandbox-runtime", "/sysroot/opt/sandbox-runtime", "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind /opt/sandbox-runtime into sysroot: %w", err)
	}

	logf("phase1a: /sysroot ready (runtime payload bound)")
	return nil
}

// phase1bSwitchRoot MS_MOVEs /proc /sys /dev (and any volume binds already
// placed under /sysroot) into the overlay, switches root into it, then
// mounts the post-switch base filesystems (devpts, cgroup v2,
// /run, /run/shm). Runs after the join, single-threaded — the chroot is a
// process-global path switch so nothing else may touch a path concurrently.
func phase1bSwitchRoot() error {
	for _, src := range []struct{ from, to string }{
		{"/proc", "/sysroot/proc"},
		{"/sys", "/sysroot/sys"},
		{"/dev", "/sysroot/dev"},
	} {
		if err := unix.Mount(src.from, src.to, "", unix.MS_MOVE, ""); err != nil {
			logf("warn: MS_MOVE %s -> %s: %v (continuing)", src.from, src.to, err)
		}
	}

	if err := unix.Chdir("/sysroot"); err != nil {
		return fmt.Errorf("chdir sysroot: %w", err)
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("MS_MOVE newroot -> /: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("chroot .: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}

	// devpts: tty mode (consoleBridge.openPTY) opens /dev/ptmx, whose
	// open() handler in the kernel resolves to a devpts mount in the
	// caller's namespace — no mount, no pty. Cheap to mount always;
	// pipe mode just doesn't use it. Mounted in the post-chroot root
	// so the MS_MOVE'd /dev stays simple (no child mounts to drag along).
	// ptmxmode=0666 lets a non-root app open /dev/ptmx if needed.
	if err := os.MkdirAll("/dev/pts", 0o755); err != nil {
		return fmt.Errorf("mkdir /dev/pts: %w", err)
	}
	if err := unix.Mount("devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666"); err != nil {
		return fmt.Errorf("mount devpts on /dev/pts: %w", err)
	}

	// cgroup v2: the freeze domain for the user-app process tree
	// (snapshot freeze/thaw, §3.4) + resource-controller delegation so
	// envd's in-guest cgroups get cpu/memory/io/pids (cgroup.go).
	// Mounted post-chroot so the path is stable.
	if err := cgroupMount(); err != nil {
		return fmt.Errorf("cgroup setup: %w", err)
	}

	// /run + /run/shm: auto-mounted tmpfs (like /proc), so apps and the
	// file-injection staging dir have them without an explicit mount entry.
	if err := mountRunDirs(); err != nil {
		return fmt.Errorf("mount /run: %w", err)
	}

	logf("phase1b: switch-root + devpts + cgroup + /run done")
	return nil
}

// mountRunDirs mounts the auto-provided tmpfs /run and /run/shm.
func mountRunDirs() error {
	if err := os.MkdirAll("/run", 0o755); err != nil {
		return fmt.Errorf("mkdir /run: %w", err)
	}
	if err := unix.Mount("tmpfs", "/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("mount tmpfs /run: %w", err)
	}
	if err := os.MkdirAll("/run/shm", 0o1777); err != nil {
		return fmt.Errorf("mkdir /run/shm: %w", err)
	}
	if err := unix.Mount("tmpfs", "/run/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777"); err != nil {
		return fmt.Errorf("mount tmpfs /run/shm: %w", err)
	}
	return nil
}

// handshakeResult carries the outcome of the concurrent launch handshake
// back to main(). On success conn is the still-open vsock connection (reused
// for launch_ack and then the stdio MUX); on error conn is already closed.
type handshakeResult struct {
	spec *proto.LaunchSpec
	conn *vsockConn
	err  error
}

// runHandshake dials the host launch server, sends hello, and receives the
// launch spec, handing the still-open connection back via ch. It touches no
// filesystem path (pure socket I/O), so it is safe to run concurrently with
// overlay assembly and the subsequent chroot. On any error it closes the
// connection and reports the error; the conn is never closed on success.
func runHandshake(ch chan<- handshakeResult) {
	conn, err := dialVsock(proto.VsockHostCID, proto.LaunchPort)
	if err != nil {
		ch <- handshakeResult{err: fmt.Errorf("vsock dial host:%d: %w", proto.LaunchPort, err)}
		return
	}
	fail := func(err error) {
		_ = conn.Close()
		ch <- handshakeResult{err: err}
	}
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeHello, Phase: "ready"}); err != nil {
		fail(fmt.Errorf("send hello: %w", err))
		return
	}
	msg, err := proto.ReadMessage(conn)
	if err != nil {
		fail(fmt.Errorf("read launch: %w", err))
		return
	}
	if msg.Type != proto.TypeLaunch || msg.Launch == nil {
		fail(fmt.Errorf("expected launch message, got %q", msg.Type))
		return
	}
	if msg.Launch.Exec == "" && !msg.Launch.Placeholder {
		fail(errors.New("launch spec missing exec"))
		return
	}
	ch <- handshakeResult{spec: msg.Launch, conn: conn}
}

// phase2Apply applies the launch spec on the (post-switch-root) rootfs, then
// finishes the handshake on conn and turns it into the stdio MUX:
//
//	guest applies network / mounts / files / init; sets up app stdio
//	guest → host: launch_ack{stdio = what we established}  ← settled signal
//	host  → guest: ack
//	... connection now speaks the framed MUX sub-protocol ...
//
// launch_ack is sent only after the whole spec is applied (incl. init), so
// the host treats it as "environment ready, about to fork". Returns the
// child's 0/1/2 fds and the consoleBridge (attached + pumping; its pump
// goroutines park until the app is forked).
func phase2Apply(spec *proto.LaunchSpec, conn *vsockConn) (childStdio, *consoleBridge, error) {
	fail := func(err error) (childStdio, *consoleBridge, error) {
		_ = conn.Close()
		return childStdio{}, nil, err
	}

	if spec.Network != nil {
		if err := applyNetwork(spec.Network); err != nil {
			return fail(fmt.Errorf("apply network: %w", err))
		}
	}
	if err := applyFsMounts(spec.Mounts); err != nil {
		return fail(fmt.Errorf("apply fs mounts: %w", err))
	}
	if err := applyFiles(spec.Files); err != nil {
		return fail(fmt.Errorf("apply files: %w", err))
	}
	if err := runInit(spec.Init); err != nil {
		return fail(fmt.Errorf("run init: %w", err))
	}

	cs, bridge, err := setupAppStdio(spec.Stdio)
	if err != nil {
		return fail(fmt.Errorf("setup app stdio: %w", err))
	}
	// v1: the guest honors whatever the host asked for, so the established
	// spec is the requested one verbatim.
	established := spec.Stdio

	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeLaunchAck, Stdio: &established}); err != nil {
		return fail(fmt.Errorf("send launch_ack: %w", err))
	}
	ack, err := proto.ReadMessage(conn)
	if err != nil {
		return fail(fmt.Errorf("read ack: %w", err))
	}
	if ack.Type != proto.TypeAck {
		return fail(fmt.Errorf("expected ack, got %q", ack.Type))
	}

	// Hand the connection to the MUX. Clear any handshake deadline first —
	// mux.Session relies on Close (not a deadline) to unblock its read loop.
	_ = conn.SetDeadline(time.Time{})
	// Arm SO_LINGER so this MUX's close at quiesce blocks until the vsock
	// teardown completes (same rationale as reattach) — quiesce reaches a
	// confirmed steady state before acking.
	if err := conn.SetLinger(muxCloseLingerSec); err != nil {
		logf("launch: SO_LINGER: %v (continuing)", err)
	}
	sess := mux.NewSession(conn, streamSetFor(established), mux.Options{OnSetWinsize: bridge.onSetWinsize})
	bridge.attach(sess)
	bridge.start()
	return cs, bridge, nil
}

// phase2ForkApp re-execs ourselves with the "exec-child" sentinel argv in
// a new PID + mount namespace, wiring the app's 0/1/2 to the fds prepared
// by setupAppStdio. In tty mode the child also gets a fresh session with
// the pty slave (its fd 0) as controlling terminal, so the line discipline
// can deliver SIGINT / SIGWINCH to the app's process group. The re-exec'd
// child (runExecChild) remounts /proc and execs the user app.
func phase2ForkApp(spec *proto.LaunchSpec, cs childStdio) (int, error) {
	self := "/proc/self/exe"

	// Make the mount tree rshared BEFORE forking the app, so restore-time
	// file injection (binds created later in PID 1's namespace) propagates
	// into the app's private CLONE_NEWNS namespace. The app child marks its
	// own copy rslave (runExecChild) so it receives PID 1's mount events but
	// never leaks its own (e.g. /proc) back. Cold-start mounts/files are
	// already in place and become shared peers in the copy. Without this,
	// a restore-time bind stays invisible to the already-running app
	// (docs/sandbox-init.md §4.3).
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		return 0, fmt.Errorf("make-rshared /: %w", err)
	}

	// Resolve the run-as identity here (PID 1, post-chroot, /etc/passwd
	// readable) and pass it to the exec-child to drop just before execve —
	// never on the outer clone, which would run the child's `mount /proc`
	// (new PID ns) as non-root and fail.
	credStr := "-"
	if spec.User != "" {
		c, err := resolveCred(spec.User)
		if err != nil {
			return 0, fmt.Errorf("resolve user %q: %w", spec.User, err)
		}
		credStr = c.encode()
	}
	// PID namespace: isolated (default) gives the app its own (CLONE_NEWPID,
	// app = PID 1 there); shared (spec.SharePID) keeps the app in sandbox-init's
	// PID ns so PID 1's reaper collects the app's orphaned descendants. Mount ns
	// is always private (file-injection rslave propagation + private /proc).
	isolated := !spec.SharePID
	isoArg := "0"
	if isolated {
		isoArg = "1"
	}
	// Placeholder: no program — the child sets up its ns/cred then waits.
	// Otherwise pass the resolved exec + args for execve.
	var args []string
	if spec.Placeholder {
		args = []string{self, "exec-child-placeholder", isoArg, credStr, spec.Workdir}
	} else {
		args = append([]string{self, "exec-child", isoArg, credStr, spec.Workdir, spec.Exec}, spec.Args...)
	}

	cloneflags := uintptr(syscall.CLONE_NEWNS)
	if isolated {
		cloneflags |= syscall.CLONE_NEWPID
	}
	sysAttr := &syscall.SysProcAttr{Cloneflags: cloneflags}
	if cs.tty {
		sysAttr.Setsid = true
		sysAttr.Setctty = true // Ctty defaults to 0 = Stdin = the pty slave
	}

	cmd := exec.Cmd{
		Path:        self,
		Args:        args,
		Env:         envSliceFromMap(spec.Env),
		Dir:         "/",
		Stdin:       cs.stdin,
		Stdout:      cs.stdout,
		Stderr:      cs.stderr,
		SysProcAttr: sysAttr,
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Drop our copies of the child ends so EOF propagates once the app
	// closes its fds (the consoleBridge keeps the opposite ends).
	_ = cs.stdin.Close()
	if cs.stdout != cs.stdin {
		_ = cs.stdout.Close()
	}
	if cs.stderr != cs.stdin && cs.stderr != cs.stdout {
		_ = cs.stderr.Close()
	}
	return cmd.Process.Pid, nil
}

// runExecChild runs in the forked child after CLONE_NEWNS (+ CLONE_NEWPID
// when isolated). For an isolated app it is pid 1 in the new pid ns and
// remounts /proc so it reflects that ns; a shared-PID app stays in
// sandbox-init's pid ns, where /proc is already correct, so no remount.
// Then chdir to workdir and exec the real app.
//
// If appPath has no '/', resolve via PATH lookup (image config Cmd
// often holds bare names like "python3" or "node", expecting standard
// PATH search semantics like sh/cmd would do).
func runExecChild(isolated bool, cred, workdir, appPath string, args []string, joined, placeholder bool) {
	// Independent children (the user app) get a private mount namespace. A
	// joined exec command already runs in the app's mount + pid namespace, so
	// it touches neither the rslave nor /proc (would disrupt the shared view).
	if !joined {
		// Make this app's private mount namespace an rslave of PID 1's
		// rshared tree: restore-time injections (binds in PID 1) propagate
		// IN, while the app's own mounts (the /proc below) stay local and
		// never leak back to PID 1. Done before the /proc mount so /proc is
		// a local, non-propagating mount.
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
			die("exec-child: make-rslave /: %v", err)
		}
		// Only an isolated app (its own pid ns) needs a fresh /proc; a
		// shared-PID app's /proc (sandbox-init's) already reflects its ns.
		if isolated {
			if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
				if errRemount := unix.Mount("none", "/proc", "", unix.MS_REMOUNT, ""); errRemount != nil {
					die("exec-child: remount /proc: %v / %v", err, errRemount)
				}
			}
		}
	}

	if workdir != "" && workdir != "/" {
		if err := unix.Chdir(workdir); err != nil {
			die("exec-child: chdir %s: %v", workdir, err)
		}
	}

	resolved := appPath
	if !placeholder && !strings.Contains(appPath, "/") {
		// PATH lookup uses os.Environ() (set by parent from spec.Env).
		p, err := exec.LookPath(appPath)
		if err != nil {
			die("exec-child: %s not found in PATH: %v", appPath, err)
		}
		resolved = p
	}

	// Tie the command's lifetime to the exec-join helper (our parent).
	// Done here, from inside the app's pid namespace, rather than via
	// syscall.SysProcAttr.Pdeathsig in runExecJoin — Go's ForkExec
	// child self-check (getppid vs pre-clone parent pid) misfires across
	// setns(CLONE_NEWPID) and would SIGKILL us at startup. The kernel
	// tracks the real parent task regardless of pid-ns visibility, and
	// PR_SET_PDEATHSIG survives a normal (non-setuid) execve, so it
	// still fires for the real command when the helper dies.
	if joined {
		if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0); err != nil {
			die("exec-child: set pdeathsig: %v", err)
		}
	}

	// Drop to the run-as identity LAST — after mount /proc, chdir and PATH
	// lookup, all of which need root — and right before execve.
	if err := applyCred(cred); err != nil {
		die("exec-child: drop privileges: %v", err)
	}

	// Placeholder: run no program. Wait for a stop signal and exit cleanly.
	// (Cannot block on `select{}` — Go's deadlock detector would panic with
	// no live goroutines; a signal-fed channel keeps the runtime alive.)
	if placeholder {
		logf("exec-child: placeholder app (no exec) — waiting for stop signal")
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		os.Exit(0)
	}

	if err := syscall.Exec(resolved, append([]string{appPath}, args...), os.Environ()); err != nil {
		die("exec-child: exec %s: %v", resolved, err)
	}
}

// supervisorState is shared between the signal loop and the reverse
// channel goroutine.
type supervisorState struct {
	appPid     atomic.Int64      // current app pid; updated on in-place restart
	spec       *proto.LaunchSpec // app launch spec (incl. Restart) for re-fork on restart
	appBackoff backoff           // app restart backoff (shared scheme, supervise.go)
	shutdown   atomic.Bool       // set on SIGTERM/SIGINT; gates app restart re-fork
	quiescing  atomic.Bool       // set across a snapshot quiesce; gates app restart re-fork
	stopSignal syscall.Signal    // signal forwarded to app on host SIGTERM/SIGINT; 0 → SIGTERM
	stopGrace  time.Duration     // grace before SIGKILL; 0 → gracefulShutdown default
	execReg    *execRegistry     // exec-session child reaping + quiesce gating
	connReg    *connRegistry     // port-forward session tracking + quiesce teardown
	acceptLn   *acceptListeners  // accept-mode (`LOCAL::TARGET`) guest listener cache
	pluginReg  *pluginRegistry   // companion-process (launch.plugin[]) supervision
}

// phase3Supervise reaps children. The user app's exit applies launch.restart
// (restart in place with backoff, or reboot); plugin exits route to the plugin
// supervisor; exec-session children route to their session; orphans (shared-PID
// apps' descendants) are reaped silently.
func phase3Supervise(s *supervisorState, b *consoleBridge) {
	sigCh := make(chan os.Signal, 16)
	signal.Notify(sigCh, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)

	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGCHLD:
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if err != nil || pid <= 0 {
					break
				}
				logf("reaped pid=%d exit=%d signal=%v", pid, status.ExitStatus(), status.Signal())
				if int64(pid) == s.appPid.Load() {
					if handleAppExit(status, s, b) {
						return // rebooting (doReboot does not return; this is a backstop)
					}
					continue // restarting in place — keep reaping
				}
				// A companion plugin? Route to its supervisor (backoff restart).
				if s.pluginReg.onExit(pid, status) {
					continue
				}
				// Otherwise an exec-session child (deliver to its goroutine) or
				// a shared-PID app orphan (no session → silently reaped).
				s.execReg.deliver(pid, status)
			}
		case syscall.SIGTERM, syscall.SIGINT:
			s.shutdown.Store(true) // stop any in-flight app restart from re-forking
			stopSig := s.stopSignal
			if stopSig == 0 {
				stopSig = syscall.SIGTERM
			}
			grace := s.stopGrace
			if grace <= 0 {
				grace = gracefulShutdown
			}
			appPid := int(s.appPid.Load())
			logf("received %v, sending %v to app pid=%d (grace %s)", sig, stopSig, appPid, grace)
			_ = syscall.Kill(appPid, stopSig)
			waitOrTimeout(appPid, grace)
			b.appExited()
			notifyAppExited(0, 0)
			doReboot()
			return
		}
	}
}

// handleAppExit applies the app's restart policy. It returns true if the
// sandbox is rebooting (so the reaper loop stops), false if the app is being
// restarted in place (the reaper keeps running). The stdio MUX survives an
// in-place restart (consoleBridge.rewireApp); only a reboot tears it down.
func handleAppExit(status syscall.WaitStatus, s *supervisorState, b *consoleBridge) (rebooting bool) {
	var code, sig int
	if status.Signaled() {
		sig = int(status.Signal())
		code = 128 + sig // shell 128+signo convention
	} else {
		code = status.ExitStatus()
	}
	if !s.shutdown.Load() && wantRestart(s.spec.Restart, status) {
		delay := s.appBackoff.next(time.Now())
		logf("app exited code=%d signal=%d (restart=%s) — restarting in %s", code, sig, s.spec.Restart, delay)
		go restartApp(s, b, delay)
		return false
	}
	logf("app exited code=%d signal=%d (restart=%s) — draining MUX, notifying host, rebooting", code, sig, s.spec.Restart)
	b.appExited() // close app fds, flush guest→host streams (bounded)
	notifyAppExited(code, sig)
	doReboot() // does not return
	return true
}

// restartApp re-forks the user app in place after the backoff delay, re-wiring
// fresh stdio onto the surviving MUX session. Runs on its own goroutine so the
// reaper keeps collecting plugins/exec children during the backoff. Skips the
// re-fork if the sandbox is shutting down; waits out a snapshot quiesce window
// so it never forks an app the freeze missed.
func restartApp(s *supervisorState, b *consoleBridge, delay time.Duration) {
	time.Sleep(delay)
	for {
		if s.shutdown.Load() {
			return
		}
		if !s.quiescing.Load() {
			break
		}
		time.Sleep(50 * time.Millisecond) // wait out the snapshot window
	}
	cs, err := b.rewireApp(s.spec.Stdio)
	if err != nil {
		if s.shutdown.Load() || errors.Is(err, errAppBridgeClosed) {
			return
		}
		logf("restart: re-wire stdio: %v (rebooting)", err)
		notifyAppExited(1, 0)
		doReboot()
		return
	}
	if s.shutdown.Load() {
		closeChildStdio(cs)
		return
	}
	s.appBackoff.onStart(time.Now())
	pid, err := phase2ForkApp(s.spec, cs)
	if err != nil {
		logf("restart: fork app: %v (rebooting)", err)
		notifyAppExited(1, 0)
		doReboot()
		return
	}
	s.appPid.Store(int64(pid))
	if err := cgroupPlaceApp(pid); err != nil {
		logf("restart: cgroup place pid=%d: %v", pid, err)
	}
	logf("app restarted pid=%d", pid)
	if err := notifyAppStarted(pid); err != nil {
		logf("restart: app_started notify failed (continuing): %v", err)
	}
}

func waitOrTimeout(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if err != nil || wpid == pid {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	logf("graceful shutdown timed out, sending SIGKILL")
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func doReboot() {
	// POWER_OFF (not RESTART): the sandbox model is one-shot — when
	// the user app exits, the sandbox is done and CH should exit too.
	// LINUX_REBOOT_CMD_RESTART triggers CH's "reboot in place" flow,
	// which tries to reconnect vhost-user-blk backends. Our backends
	// only accept one connection (sandbox = single VM lifetime), so
	// reconnect fails and CH exits non-zero. POWER_OFF cleanly signals
	// vCPU shutdown; CH exits 0.
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		die("reboot: %v", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func waitForDevice(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(devicePollInterval)
	}
	return fmt.Errorf("device %s did not appear within %s", path, timeout)
}

func envSliceFromMap(m map[string]string) []string {
	if len(m) == 0 {
		return []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// runMemReporter samples /proc/meminfo every interval and pushes a
// mem_report to the host. Best-effort: errors logged, ticker continues.
// First report fires immediately so the host's BalloonController gets
// a baseline before its first reconcile tick.
func runMemReporter(interval time.Duration) {
	push := func() {
		avail, total, err := readMemInfo()
		if err != nil {
			logf("mem_report: read /proc/meminfo: %v", err)
			return
		}
		if err := notifyMemReport(avail, total); err != nil {
			logf("mem_report: %v", err)
		}
	}
	push()
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		push()
	}
}

// readMemInfo parses /proc/meminfo's MemTotal and MemAvailable in
// bytes. Linux reports kB; we shift to bytes for the wire protocol.
func readMemInfo() (memAvailable, memTotal uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, err := f.Read(buf)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			memTotal = parseMemInfoKB(line) << 10
		case strings.HasPrefix(line, "MemAvailable:"):
			memAvailable = parseMemInfoKB(line) << 10
		}
		if memTotal != 0 && memAvailable != 0 {
			break
		}
	}
	if memTotal == 0 {
		return 0, 0, errors.New("MemTotal not found")
	}
	return memAvailable, memTotal, nil
}

// parseMemInfoKB pulls the kB-scale integer out of a /proc/meminfo
// line of shape "MemTotal:    8064212 kB". Returns 0 on malformed input.
func parseMemInfoKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// initStart is the wall clock at sandbox-init main() entry. Used to
// prefix every log line with elapsed milliseconds — visible from the
// host-side guest console capture, gives a millisecond-resolution view
// of guest cold-start phases that complements sandbox-ctl's
// host-side microsecond timestamps.
var initStart = time.Now()

func logf(format string, args ...any) {
	elapsedMs := time.Since(initStart).Milliseconds()
	fmt.Fprintf(os.Stderr, "[sandbox-init][T+%dms] "+format+"\n",
		append([]any{elapsedMs}, args...)...)
}

func die(format string, args ...any) {
	logf(format, args...)
	os.Exit(1)
}

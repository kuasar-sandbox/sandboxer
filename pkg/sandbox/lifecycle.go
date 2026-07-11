package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/pkg/chapi"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
	"github.com/kuasar-sandbox/sandboxer/pkg/tapfd"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

// RunOptions controls a single sandbox-ctl run invocation.
type RunOptions struct {
	Cfg           *config.SandboxConfig
	ManifestCfg   *config.ManifestConfig // for manifest:// resolution; may be nil if all file://
	SandboxID     string                 // generated if empty
	CHBinary      string                 // path to bin/cloud-hypervisor
	RuntimeRoot   string                 // tmpfs run root (sockets / snap staging); "/run/sandbox" by default
	BaseRoot      string                 // on-disk base root (overlay diff); "/var/lib/sandbox" by default
	StatsJSONPath string                 // if set, write vhost stats as JSON to this path on shutdown
	StatsInterval time.Duration          // if > 0, periodically log lazy-load stats; 0 = off
	StdioMode     stdio.Mode             // CH process stdio wiring; see pkg/stdio

	// PingFatalThreshold: after this many consecutive ping failures
	// the host SIGTERMs CH so cmd.Wait() returns. 0 = disabled
	// (default — wait for the user's signal). With the default 1 s
	// ping interval, 30 ≈ 30 s of unreachability. See pinger.go.
	PingFatalThreshold int

	// Forwards are parsed `--connect` port-forward directives (forward.go).
	// Each opens a host-local listener whose connections are spliced to a
	// guest-side target. Empty → no port forwarding.
	Forwards []ForwardSpec
}

// Run executes one sandbox lifecycle: prepare backends + launch server,
// spawn CH, wait for exit, cleanup. Returns CH's exit code or an error
// if setup failed.
func Run(ctx context.Context, opts RunOptions) (int, error) {
	startUnixNs := time.Now().UnixNano()
	if opts.Cfg == nil {
		return -1, fmt.Errorf("RunOptions.Cfg is nil")
	}
	if opts.SandboxID == "" {
		opts.SandboxID = generateSandboxID()
	}
	if opts.RuntimeRoot == "" {
		opts.RuntimeRoot = "/run/sandbox"
	}
	if opts.BaseRoot == "" {
		opts.BaseRoot = DefaultBaseRoot
	}
	if opts.CHBinary == "" {
		opts.CHBinary = "cloud-hypervisor"
	}

	if err := opts.Cfg.ValidateCold(); err != nil {
		return -1, fmt.Errorf("config: %w", err)
	}
	// tap-name mode: CH opens the host tap, so verify it exists now.
	// tapfd mode: the fd comes from the provider handoff below (no host tap
	// to verify here).
	if opts.Cfg.Network.TAP != "" {
		if err := VerifyTAP(opts.Cfg.Network.TAP); err != nil {
			return -1, err
		}
	}
	if err := populateSnapshotRefs(opts.Cfg); err != nil {
		return -1, fmt.Errorf("snapshot refs: %w", err)
	}

	runDir := filepath.Join(opts.RuntimeRoot, opts.SandboxID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return -1, fmt.Errorf("mkdir %s: %w", runDir, err)
	}
	defer os.RemoveAll(runDir)

	// chSock is needed here (front-half) because resctl.BalloonController is
	// constructed before ServeAndWait; the remaining socket paths are
	// owned by ServeAndWait, which derives them identically from runDir.
	chSock := filepath.Join(runDir, "ch.sock")

	logf := func(format string, a ...any) { log.Printf("[sandbox-ctl] "+format, a...) }

	// Resolve capacity / floor up front so we can build the resctl.BalloonController
	// before hooks (hooks owns "alloc change → balloon target" routing,
	// which needs balloonCtl in hand). Both are cheap yaml lookups; the
	// memfd-creation block below reuses capBytes.
	capBytes, err := opts.Cfg.CapacityMemoryBytes()
	if err != nil {
		return -1, err
	}
	allocBytes, _ := opts.Cfg.AllocatableMemoryBytes()

	// resctl.BalloonController is the sole writer of /api/v1/vm.resize. Created
	// only when alloc < cap (no balloon device when alloc == cap).
	// SetAllocatable(allocBytes) here matches the --balloon size= value
	// passed to CH in ch.go, so the in-memory target agrees with CH from
	// the start without an extra round-trip after launch.
	var balloonCtl *resctl.BalloonController
	initialAllocBytes := allocBytes
	if allocBytes < capBytes {
		balloonCtl = resctl.NewBalloonController(chSock, capBytes, logf)
		balloonCtl.SetAllocatable(allocBytes)
	}

	// Controller handshake (dynamic mode) — must happen before cgroup write
	// so that the controller-granted initial allocatable can override
	// the static startup-burst value when degraded. Balloon is injected
	// here so any later OnAllocatableChanged / SettledRestore call routes
	// through balloonCtl rather than opening its own HTTP path.
	hooks, err := resctl.NewControllerHooks(resctl.ControllerHookOptions{
		SocketPath: opts.Cfg.Resources.Control.Controller,
		CgroupPath: opts.Cfg.Resources.Control.CgroupPath,
		Logf:       logf,
		Balloon:    balloonCtl,
	}, opts.Cfg)
	if err != nil {
		return -1, fmt.Errorf("controller dial: %w", err)
	}
	if hooks.Enabled() {
		grantedInitial, err := hooks.Admit(opts.SandboxID, 0)
		if err != nil {
			return -1, err
		}
		if grantedInitial < allocBytes {
			return -1, fmt.Errorf("controller admit granted initial allocatable %d below floor %d", grantedInitial, allocBytes)
		}
		if grantedInitial > capBytes {
			return -1, fmt.Errorf("controller admit granted initial allocatable %d above capacity %d", grantedInitial, capBytes)
		}
		initialAllocBytes = grantedInitial
		if balloonCtl != nil {
			balloonCtl.SetAllocatable(initialAllocBytes)
		}
		logf("controller admit ok, initial allocatable=%d", grantedInitial)
	}
	defer hooks.Release("normal")

	// cgroup join. CgroupPath empty → no-cgroup mode, no cgroup operations.
	// CgroupPath set → join existing cgroup (must already exist; not
	// created by sandbox-ctl). See docs/sandbox.md §4.1.
	cgCfg, err := resctl.BuildCgroupConfig(opts.Cfg)
	if err != nil {
		return -1, err
	}
	// Defer memory.high write to Settled (launch hello). Cold boot's
	// uffd-driven page-fault burst can push the cgroup well past
	// allocatable*0.875; if memory.high is already in effect, every
	// UFFDIO_ZEROPAGE/COPY syscall returns through
	// mem_cgroup_handle_over_high reclaim, throttling the uffd handler
	// against the very faults it's trying to resolve (Issue 4 root cause).
	// memory.max remains the hard ceiling during boot; memory.high gets
	// written by Settled() once the boot transient is past.
	cgCfg.MemoryHighBytes = 0
	cg, err := resctl.JoinCgroup(cgCfg)
	if err != nil {
		return -1, fmt.Errorf("cgroup: %w", err)
	}
	if cg.Active() {
		logf("cgroup limits set: %s memory.max=%d memory.high=%d cpu.max=%dus/100000us cpu.weight=%d (CH joins on start; sandbox-ctl stays out)",
			cg.Path, cgCfg.MemoryMaxBytes, cgCfg.MemoryHighBytes,
			cgCfg.CPUMaxQuotaUs, cgCfg.CPUWeight)
	} else {
		logf("cgroup: no path configured, running without cgroup limits (no-cgroup mode)")
	}
	defer func() { _ = cg.Cleanup() }()

	// Open the read-side fetcher (cache + store clients + decryptor)
	// once per sandbox when any disk URI uses manifest://. file://-only
	// configs without a manifest config skip the dial. snapshot --upload
	// builds its own (write-side) Ingester at request time; the two
	// paths never share a connection pool.
	var fetcher manifest.FetcherCloser
	if needsManifestFetcher(opts.Cfg) {
		if opts.ManifestCfg == nil {
			return -1, fmt.Errorf("manifest:// disk requires --manifest-config or MANIFEST_CONFIG")
		}
		fetcher, err = opts.ManifestCfg.NewFetcher(opts.ManifestCfg.FetchKeyFunc())
		if err != nil {
			return -1, fmt.Errorf("manifest fetcher: %w", err)
		}
		defer fetcher.Close()
		logf("manifest fetcher: store=%s cache=%s crypto=%s/%s",
			opts.ManifestCfg.Store.Endpoint, opts.ManifestCfg.Cache.Endpoint,
			opts.ManifestCfg.Crypto.Chunk, opts.ManifestCfg.Crypto.Manifest)
	}

	// Resolve the root disk(s) and image config by mode (docs/sandbox-runtime
	// .md §3.1):
	//   - overlay mode: blk0 is the read-only erofs image (boot.root.base);
	//     the launch defaults (image config) are read from its appended ZIP.
	//   - single-disk mode: blk0 is the writable ext4 CoW built below; there
	//     is no erofs image and thus no image config, so launch.exec must be
	//     set (enforced by config.validate). blk0Reader stays nil.
	var blk0Reader vhost.BlockReader // overlay mode only (ro erofs base)
	var imageCfg *ImageConfig
	if opts.Cfg.SingleDisk() {
		imageCfg = &ImageConfig{}
	} else {
		r, _, err := OpenBlockReader(ctx, opts.Cfg.Boot.Root.Base, fetcher)
		if err != nil {
			return -1, fmt.Errorf("blk0 base: %w", err)
		}
		blk0Reader = r
		defer blk0Reader.Close()
		// Both file:// and manifest:// blk0 produce a vhost.BlockReader that is
		// also a concurrent-safe io.ReaderAt; LoadImageConfigFrom does the
		// ZIP-trailer scan over either source uniformly.
		imageCfg, err = LoadImageConfigFrom(blk0Reader, blk0Reader.Size())
		if err != nil {
			return -1, fmt.Errorf("load rootfs image config: %w", err)
		}
		logf("image config: cmd=%v entrypoint=%v workdir=%q env-keys=%d",
			imageCfg.Cmd, imageCfg.Entrypoint, imageCfg.WorkingDir, len(imageCfg.Env))
	}
	// Merge image config defaults with sandbox.yaml `launch:` overrides.
	launchSpec, err := MergeLaunch(imageCfg, opts.Cfg.Launch)
	if err != nil {
		return -1, fmt.Errorf("launch spec: %w", err)
	}

	// Network acquisition. tapfd mode (docs/tapfd.md §3) receives a tap queue
	// fd + metadata from either an exec helper or a persistent provider socket;
	// tap-name mode was verified above and CH opens it. The handoff metadata
	// overrides the static attrs (mac/ip/mtu). The resolved spec travels
	// through the launch handshake; nil → "no IP configuration" to sandbox-init.
	var tapFile, netnsFile *os.File
	var metaMAC, metaIP string
	var metaMTU int
	if opts.Cfg.Network.TapFD != nil {
		f, nsf, meta, err := tapfd.AcquireConfig(ctx, opts.Cfg.Network.TapFD)
		if err != nil {
			return -1, fmt.Errorf("tapfd handoff: %w", err)
		}
		tapFile = f
		defer tapFile.Close()
		netnsFile = nsf // non-nil only if the provider's tap is netns-isolated
		if netnsFile != nil {
			defer netnsFile.Close()
		}
		metaMAC, metaIP, metaMTU = meta.MAC, meta.IP, meta.MTU
		logf("tapfd: received tap fd (mac=%s ip=%s mtu=%d netns=%t)", meta.MAC, meta.IP, meta.MTU, netnsFile != nil)
	}
	netMAC, netSpec := opts.Cfg.Network.Effective(metaMAC, metaIP, metaMTU)
	launchSpec.Network = netSpec

	// Environment setup carried in the launch spec (applied guest-side
	// before the app forks): mounts (incl. image Volumes → empty mounts),
	// injected files, one-shot init, and the shutdown grace.
	launchSpec.Mounts = effectiveMounts(opts.Cfg.Mounts, imageCfg.Volumes)
	launchSpec.Files = opts.Cfg.ProtoFiles()
	launchSpec.Init = toProtoInit(opts.Cfg.Init)
	launchSpec.Plugins = toProtoPlugins(opts.Cfg.Launch.Plugin)
	launchSpec.SharePID = opts.Cfg.Launch.PIDNamespace == "shared"
	launchSpec.StopGraceSec = opts.Cfg.StopGraceSeconds()

	// App stdio: tell sandbox-init what to wire (pty vs pipe channels);
	// the launch-handshake connection becomes the stdio MUX after
	// launch_ack (guestlink.LaunchServer.OnMUXReady below).
	launchSpec.Stdio = opts.StdioMode.ProtoSpec()
	if cols, rows, ok := opts.StdioMode.InitialWinsize(); ok {
		launchSpec.Stdio.Winsize = &proto.Winsize{Cols: cols, Rows: rows}
	}

	// Build the writable CoW disk. In overlay mode it is blk1 (the ext4 upper
	// over the erofs blk0); in single-disk mode it IS blk0 (the root disk).
	// Either way: an optional read-only CoW base + a writable sparse diff.
	var cowBase vhost.BlockReader
	var diffURI, diffTemplate string
	if opts.Cfg.SingleDisk() {
		diffURI, diffTemplate = opts.Cfg.Boot.Root.Diff, opts.Cfg.Boot.Root.DiffTemplate
		if opts.Cfg.Boot.Root.Base != "" {
			r, _, err := OpenBlockReader(ctx, opts.Cfg.Boot.Root.Base, fetcher)
			if err != nil {
				return -1, fmt.Errorf("single-disk root base: %w", err)
			}
			cowBase = r
			defer cowBase.Close()
		}
	} else {
		diffURI, diffTemplate = opts.Cfg.Boot.Root.Overlay.Diff, opts.Cfg.Boot.Root.Overlay.DiffTemplate
		if opts.Cfg.Boot.Root.Overlay.Base != "" {
			r, _, err := OpenBlockReader(ctx, opts.Cfg.Boot.Root.Overlay.Base, fetcher)
			if err != nil {
				return -1, fmt.Errorf("blk1 overlay base: %w", err)
			}
			cowBase = r
			defer cowBase.Close()
		}
	}
	// Resolve the diff path. Empty → auto-default to the on-disk base dir (NOT
	// the tmpfs run dir — the writable layer must be on disk). An auto-defaulted
	// diff is ours: removed when the sandbox ends.
	ownedDiff := diffURI == ""
	if ownedDiff {
		baseDir := DefaultBaseDir(opts.BaseRoot, opts.SandboxID)
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			return -1, fmt.Errorf("mkdir base dir %s: %w", baseDir, err)
		}
		diffURI = DefaultDiffURI(opts.BaseRoot, opts.SandboxID)
		defer func() {
			_ = os.Remove(filepath.Join(baseDir, opts.SandboxID+".overlay.diff"))
			_ = os.Remove(baseDir)
		}()
	}
	_, diffPath, ok := config.SchemeAndPath(diffURI)
	if !ok {
		return -1, fmt.Errorf("boot.root diff invalid URI: %s", diffURI)
	}
	var baseSize int64
	if cowBase != nil {
		baseSize = cowBase.Size()
	}
	diffSize, err := opts.Cfg.DiffSizeBytes()
	if err != nil {
		return -1, err
	}
	createSize, err := PrepareDiff(diffPath, diffTemplate, baseSize, diffSize)
	if err != nil {
		return -1, fmt.Errorf("prepare diff: %w", err)
	}
	cow, err := vhost.OpenBlockCOW(diffPath, cowBase, createSize)
	if err != nil {
		return -1, fmt.Errorf("root COW: %w", err)
	}
	defer cow.Close()

	// Root logical disk (Disks[0]): single → one writable Cow (blk0); overlay →
	// ro erofs base (blk0) + writable Cow (blk1). Data disks (boot.disks[], in
	// order) append their device(s) after the root.
	disks := []DiskBackend{{
		Overlay:   !opts.Cfg.SingleDisk(),
		Reader:    blk0Reader, // nil in single-disk
		Cow:       cow,
		BasePath:  opts.Cfg.Boot.Root.Base, // overlay ro device stats path
		DiffPath:  diffPath,
		OwnedDiff: ownedDiff,
	}}
	for i := range opts.Cfg.Boot.Disks {
		db, dcleanup, derr := prepColdDataDisk(ctx, &opts.Cfg.Boot.Disks[i], i, opts.BaseRoot, opts.SandboxID, fetcher)
		if derr != nil {
			return -1, derr
		}
		defer dcleanup()
		disks = append(disks, db)
	}
	// Resolve mounts[].type=disk → guest device paths (name→ordinal→/dev/vdX);
	// clears the name (not sent to the guest).
	resolveDiskMounts(launchSpec.Mounts, opts.Cfg.Boot.Disks, opts.Cfg.SingleDisk())

	// The shared back-half (memfd, uffd va_report handler, vhost-blk
	// backends, launch server, pinger, ctl.sock, signal escalation,
	// stats) lives in ServeAndWait. Cold start supplies: ZeroSource
	// faults, the merged launch spec whose conn becomes the stdio MUX
	// (WireLaunchMUX), and a settle protocol gated on the guest's
	// hello / launch_ack handshake.
	return ServeAndWait(VMParams{
		Ctx:                ctx,
		SandboxID:          opts.SandboxID,
		RunDir:             runDir,
		Logf:               logf,
		StdioMode:          opts.StdioMode,
		PingFatalThreshold: opts.PingFatalThreshold,
		StartUnixNs:        startUnixNs,
		StatsJSONPath:      opts.StatsJSONPath,
		StatsInterval:      opts.StatsInterval,

		CapBytes:   int64(capBytes),
		UffdSource: uffd.ZeroSource{},
		Disks:      disks,

		LaunchSpec:        launchSpec,
		WireLaunchMUX:     true,
		StartTimeout:      opts.Cfg.StartTimeoutDuration(),
		VAReportDeadline:  opts.Cfg.VAReportDeadline(),
		PingTimeout:       opts.Cfg.PingDeadline(),
		AppNotifyDeadline: opts.Cfg.AppNotifyDeadline(),
		Balloon:           balloonCtl,
		Hooks:             hooks,

		TapFile:   tapFile, // nil in tap-name mode; CH inherits it at fd 4
		NetMAC:    netMAC,
		NetnsFile: netnsFile, // non-nil → launch CH inside the tap's netns

		SnapCfg:     opts.Cfg,
		ManifestCfg: opts.ManifestCfg,
		Forwards:    opts.Forwards,
		Cgroup:      cg,

		BuildCmd: func(e CmdEnv) (*exec.Cmd, func(), error) {
			_, kernelPath, _ := config.SchemeAndPath(opts.Cfg.Boot.Kernel)
			_, runtimePath, _ := config.SchemeAndPath(opts.Cfg.Boot.Runtime)
			// CH process stdio (docs/sandbox.md §5.2): stdin = /dev/null
			// (so CH's `--console tty` never raw-izes a terminal), stderr
			// = our stderr (CH WARN), stdout = the kernel-dmesg sink per
			// --console. The returned consoleArg goes into the cmdline.
			cmd := exec.Command(opts.CHBinary)
			consoleArg, cleanup, err := opts.StdioMode.SetupCHStdio(cmd)
			if err != nil {
				return nil, nil, fmt.Errorf("stdio: %w", err)
			}
			args, err := CHCommandWithInitialAllocatable(opts.Cfg, initialAllocBytes, e.Disks, e.CHSock, e.VsockBase, kernelPath, runtimePath, e.UffdSock, consoleArg, e.TapFDNum, e.NetMAC)
			if err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("CH cmdline: %w", err)
			}
			cmd.Args = append(cmd.Args, args...)
			logf("CH args: %s", strings.Join(args, " "))
			return cmd, cleanup, nil
		},

		// Cold-start settle: ping ticker starts as soon as the launch
		// handshake completes (HelloDone = LaunchSpec sent); Settled +
		// balloon reconcile + controller heartbeat/sensor gate on the
		// guest's launch_ack (post-boot transient over). Fire-and-forget
		// so ServeAndWait proceeds to cmd.Wait.
		PostSpawn: func(pc PostSpawnCtx) error {
			go func() {
				select {
				case <-pc.Launch.HelloDone():
					pc.Pinger.Start(pc.Ctx)
				case <-pc.Ctx.Done():
					return
				}
				select {
				case <-pc.Launch.LaunchAckDone():
					if err := pc.Hooks.Settled(); err != nil {
						pc.Logf("settled: %v", err)
					}
					if pc.Balloon != nil {
						if err := pc.Balloon.Start(pc.Ctx); err != nil {
							pc.Logf("balloon: start: %v", err)
						}
					}
					if pc.Hooks.Enabled() {
						pc.Hooks.StartHeartbeat(pc.Ctx, 5*time.Second)
						pc.Hooks.StartSensor(pc.Ctx, 64<<20)
					}
				case <-pc.Ctx.Done():
				}
			}()
			return nil
		},
	})
}

// chShutdownGrace bounds how long we wait for CH to exit cleanly after
// forwarding the first SIGTERM. Cold-target hangs in production observed
// CH not draining the signal for >50 minutes because vCPU was stuck;
// without this escalation, sandbox-ctl waits indefinitely on cmd.Wait.
const chShutdownGrace = 5 * time.Second

// processSignaler is the subset of *os.Process needed by
// waitForCHWithSignalEscalation, exposed for testability.
type processSignaler interface {
	Signal(sig os.Signal) error
}

// waitForCHWithSignalEscalation blocks until doneCh fires, driving CH
// shutdown when host SIGTERM/SIGINT arrives. Shutdown path (in order):
//
//  1. PUT /api/v1/vmm.shutdown via chapi — CH performs an ordered
//     internal cleanup (stop vCPU → destroy devices → release memory
//     zones → close sockets → exit), avoiding the slow Linux reaper
//     unmap-on-SIGKILL path that hurts host-oversubscribe density
//     scenarios (see density-perf forensics: 8 GiB zone × 8 sandbox
//     teardown spent ~31 s in kernel mm-lock contention after SIGKILL).
//  2. If the API call fails (CH dead, socket closed, etc.) fall back
//     to forwarding SIGTERM to the CH process.
//  3. Arm a grace timer; if CH still hasn't exited, escalate to SIGKILL.
//  4. A second SIGTERM/INT from the user escalates immediately.
//
// chSock is empty for tests that exercise only the signal path; in that
// case we skip step 1 and go straight to SIGTERM (matches the old
// behavior).
func waitForCHWithSignalEscalation(
	doneCh <-chan error,
	sigCh <-chan os.Signal,
	proc processSignaler,
	chPid int,
	chSock string,
	chRespDeadline time.Duration,
	grace time.Duration,
	logf func(format string, args ...any),
) error {
	var killTimer *time.Timer
	var killCh <-chan time.Time
	shutdownInitiated := false
	for {
		select {
		case sig := <-sigCh:
			if !shutdownInitiated {
				usedAPI := false
				if chSock != "" {
					if err := (chapi.Client{Sock: chSock, RespDeadline: chRespDeadline}).ShutdownVMM(); err == nil {
						logf("received %v, requested vmm.shutdown via API (will SIGKILL after %s if CH still alive)", sig, grace)
						usedAPI = true
					} else {
						logf("received %v, vmm.shutdown API failed (%v) — falling back to SIGTERM", sig, err)
					}
				}
				if !usedAPI {
					logf("received %v, forwarding SIGTERM to CH (will SIGKILL after %s)", sig, grace)
					_ = proc.Signal(syscall.SIGTERM)
				}
				shutdownInitiated = true
				killTimer = time.NewTimer(grace)
				killCh = killTimer.C
			} else {
				logf("received %v while shutdown in progress, sending SIGKILL now", sig)
				_ = proc.Signal(syscall.SIGKILL)
				if killTimer != nil {
					killTimer.Stop()
				}
				killCh = nil
			}
		case <-killCh:
			logf("CH didn't exit within %s of shutdown request, sending SIGKILL pid=%d", grace, chPid)
			_ = proc.Signal(syscall.SIGKILL)
			killCh = nil
		case waitErr := <-doneCh:
			if killTimer != nil {
				killTimer.Stop()
			}
			return waitErr
		}
	}
}

// writeUffdStats emits a one-line-per-counter human readable summary
// of uffd handler counters to w. Designed to be quick to eyeball when
// e2e_sandbox_cold dumps stderr.
func writeUffdStats(w io.Writer, s map[string]uint64) {
	fmt.Fprintln(w, "[uffd-stats]")
	keys := []string{
		"faults_absent",
		"faults_released",
		"faults_loaded",
		"zeropage_calls",
		"copy_calls",
		"pages_zeroed",
		"pages_copied",
		"wakes",
		"remove_events",
		"remove_q_dropped",
		"remove_events_batched",
		"madvise_calls",
		"madvise_bytes",
		"backend_lookup_miss",
		"errors",
		"batch_calls",
		"batch_pages_total",
		"batch_avg_pages",
		"batch_max_pages",
	}
	for _, k := range keys {
		fmt.Fprintf(w, "  %-20s %d\n", k, s[k])
	}
}

// guestDevPath maps a 0-based device index to its guest block device. virtio-blk
// names devices vda, vdb, … in CH --disk order; root takes the first 1-2, data
// disks the rest. Index stays < 26 (root ≤ 2 + 8 data disks × 2 = 18 max).
func guestDevPath(i int) string { return "/dev/vd" + string(rune('a'+i)) }

// resolveDiskMounts fills the type=disk MountSpecs with their resolved guest
// devices: it maps each mount's source (a boot.disks[] name) to that disk's
// ordinal and CH-order device path(s), and clears Source (the name is not sent
// to the guest — only the ordinal + devices). Device order is root first (1 or
// 2 devices), then each boot.disks[] entry in array order.
func resolveDiskMounts(mounts []proto.MountSpec, disks []config.DiskConfig, rootSingle bool) {
	first := make([]int, len(disks))
	next := 1
	if !rootSingle {
		next = 2
	}
	ord := make(map[string]int, len(disks))
	for i := range disks {
		ord[disks[i].Name] = i
		first[i] = next
		if disks[i].RootConfig.Single() {
			next++
		} else {
			next += 2
		}
	}
	for mi := range mounts {
		m := &mounts[mi]
		if m.Type != "disk" {
			continue
		}
		i := ord[m.Source] // validated to exist by config.validateDisks
		d := &disks[i]
		m.DiskIndex = i
		m.Source = ""
		if d.RootConfig.Single() {
			m.DiskOverlay = false
			m.DiskDevs = []string{guestDevPath(first[i])}
		} else {
			m.DiskOverlay = true
			m.DiskDevs = []string{guestDevPath(first[i]), guestDevPath(first[i] + 1)}
		}
	}
}

// prepColdDataDisk resolves one boot.disks[] data disk for cold start: opens its
// optional ro base(s) and builds its writable CoW diff (same machinery as the
// root). ordinal is its boot.disks[] index (used for the auto-default diff name).
// The returned cleanup closes the readers/CoW and removes an auto-created diff.
func prepColdDataDisk(ctx context.Context, d *config.DiskConfig, ordinal int, baseRoot, sandboxID string, fetcher fetch.Fetcher) (DiskBackend, func(), error) {
	var db DiskBackend
	var closers []func()
	cleanup := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	fail := func(e error) (DiskBackend, func(), error) { cleanup(); return DiskBackend{}, nil, e }

	single := d.RootConfig.Single()
	db.Overlay = !single
	field := fmt.Sprintf("boot.disks[%d]", ordinal)

	var cowBaseURI, diffURI, diffTemplate string
	if single {
		cowBaseURI, diffURI, diffTemplate = d.Base, d.Diff, d.DiffTemplate
	} else {
		r, _, err := OpenBlockReader(ctx, d.Base, fetcher)
		if err != nil {
			return fail(fmt.Errorf("%s erofs base: %w", field, err))
		}
		db.Reader, db.BasePath = r, d.Base
		closers = append(closers, func() { r.Close() })
		cowBaseURI, diffURI, diffTemplate = d.Overlay.Base, d.Overlay.Diff, d.Overlay.DiffTemplate
	}

	var cowBase vhost.BlockReader
	if cowBaseURI != "" {
		r, _, err := OpenBlockReader(ctx, cowBaseURI, fetcher)
		if err != nil {
			return fail(fmt.Errorf("%s cow base: %w", field, err))
		}
		cowBase = r
		closers = append(closers, func() { r.Close() })
	}

	if diffURI == "" { // auto-default a per-disk diff on the base dir; ours to remove
		baseDir := DefaultBaseDir(baseRoot, sandboxID)
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			return fail(fmt.Errorf("%s mkdir base dir: %w", field, err))
		}
		p := filepath.Join(baseDir, fmt.Sprintf("%s.disk%d.diff", sandboxID, ordinal))
		diffURI = "file://" + p
		db.OwnedDiff = true
		closers = append(closers, func() { _ = os.Remove(p) })
	}
	_, diffPath, ok := config.SchemeAndPath(diffURI)
	if !ok {
		return fail(fmt.Errorf("%s diff invalid URI: %s", field, diffURI))
	}
	var baseSize int64
	if cowBase != nil {
		baseSize = cowBase.Size()
	}
	diffSize, err := d.RootConfig.DiffSizeBytes(field)
	if err != nil {
		return fail(err)
	}
	createSize, err := PrepareDiff(diffPath, diffTemplate, baseSize, diffSize)
	if err != nil {
		return fail(fmt.Errorf("%s prepare diff: %w", field, err))
	}
	cow, err := vhost.OpenBlockCOW(diffPath, cowBase, createSize)
	if err != nil {
		return fail(fmt.Errorf("%s COW: %w", field, err))
	}
	closers = append(closers, func() { cow.Close() })
	db.Cow, db.DiffPath = cow, diffPath
	return db, cleanup, nil
}

// allQuiescer drives Quiesce/Resume on every vhost backend together (root +
// data-disk devices, in device order).
type allQuiescer struct{ servers []*vhost.Server }

func (q *allQuiescer) Quiesce() {
	for _, s := range q.servers {
		s.Quiesce()
	}
}
func (q *allQuiescer) Resume() {
	// LIFO so callers of Quiesce see fully-paused state before any
	// resume kicks the backends.
	for i := len(q.servers) - 1; i >= 0; i-- {
		q.servers[i].Resume()
	}
}

// SnapshotHandler bundles the live state needed to service one
// snapshot_request on ctl.sock. Both Run (cold-start) and restore.Run
// build one of these and pass it to ctl.sock listener; lifecycle code
// that owns the bundle holds references; the snapshot path consumes
// them when a request arrives.
type SnapshotHandler struct {
	Cfg         *config.SandboxConfig
	ManifestCfg *config.ManifestConfig // required for --upload; the Ingester is built lazily per snapshot
	SandboxID   string                 // required: snapshot.Take rejects empty
	Memfd       *memory.Memfd
	Disks       []SnapDiskRef   // writable diffs to capture, logical order (root, then data disks)
	Servers     []*vhost.Server // all vhost servers (quiesced together around the dump)
	CHSock      string
	RunDir      string
	Pinger      *guestlink.Pinger // optional; if non-nil, paused around quiesce/Take
	Forwarder   *Forwarder        // optional; if non-nil, paused + active relays collapsed around quiesce
	Reattach    func() error      // optional; re-establishes the stdio MUX after --resume
	Logf        func(string, ...any)
}

// Handle dispatches one ctl snapshot_request. Public for restore.Run.
func (h *SnapshotHandler) Handle(req ctl.Request) (ctl.Response, error) {
	opts := RunOptions{Cfg: h.Cfg, ManifestCfg: h.ManifestCfg, SandboxID: h.SandboxID}
	return handleSnapshotRequest(req, opts, h.Memfd, h.Disks, h.Servers, h.CHSock, h.RunDir, h.Pinger, h.Forwarder, h.Reattach, h.Logf)
}

// handleSnapshotRequest executes one snapshot_request received via
// ctl.sock. Wraps pkg/snapshot.Take with the runtime state
// owned by Run. Upload mode opens a fresh manifest.IngesterCloser
// here (per request) so the snapshot write path never shares a
// connection pool with the long-lived disk read path.
func handleSnapshotRequest(
	req ctl.Request,
	opts RunOptions,
	mfd *memory.Memfd,
	disks []SnapDiskRef, // writable diffs, logical order (root, then data disks)
	servers []*vhost.Server, // all vhost servers (quiesced together)
	chSock, runDir string,
	pinger *guestlink.Pinger,
	forwarder *Forwarder, // gates new forwards + collapses active relays around quiesce; may be nil
	reattachMUX func() error, // re-establishes the stdio MUX after --resume; may be nil
	logf func(string, ...any),
) (resp ctl.Response, err error) {
	// Quiesce sequence (docs/sandbox.md §6.2 T2a, sandbox-init.md §3.4):
	// pause the ping ticker, then `quiesce` → the guest does sync +
	// drop_caches, stops reading the app's stdout/stderr (pty), runs the
	// graceful MUX_CLOSE handshake on the stdio MUX (the host's mux.Session
	// auto-replies MUX_CLOSE_ACK), then replies `quiesced`. quiesced ⇒
	// the MUX is closed + the app is blocked + the guest is in a clean
	// state — safe to /vm.pause via snapshot.Take. Quiesce failure aborts
	// this snapshot rather than capturing a half-open MUX; sandbox keeps
	// running.
	//
	// On the --resume path, snapshot.Take resumes CH but the MUX stays
	// closed; reattachMUX (below, after Take) `attach`es a fresh one so the
	// running app's stdio flows again. On the destroy path (resume_after=
	// false) we instead tear the VMM down once this response has flushed —
	// that is what makes `sandbox-ctl run` return (docs/sandbox.md §6.2 T8).
	defer func() {
		if err == nil && !req.ResumeAfter {
			go destroyAfterSnapshot(chSock, opts.Cfg.CHApiDeadline(), logf)
		}
	}()
	// Gate new port-forward connects for the whole quiesce→snapshot window;
	// active relays are collapsed after the guest acks `quiesced` (it has
	// already torn down its ends, lingered). Resume mirrors the pinger:
	// stay paused only on the success + destroy path.
	if forwarder != nil {
		forwarder.Pause()
		defer func() {
			if err == nil && !req.ResumeAfter {
				return
			}
			forwarder.Resume()
		}()
	}
	if pinger != nil {
		pinger.Pause()
		// Resume on the way out UNLESS this is the success + destroy
		// path: there the VM stays paused while destroyAfterSnapshot
		// tears it down, so a resumed pinger only spews misleading
		// `ping ... i/o timeout` lines against a paused/dying VM. On
		// error (sandbox keeps running) or --resume (sandbox resumed),
		// the pinger must come back.
		defer func() {
			if err == nil && !req.ResumeAfter {
				return
			}
			pinger.Resume()
		}()
		client := pinger.Client
		if client == nil {
			client = &guestlink.HostClient{BasePath: filepath.Join(runDir, "vsock.sock"), Logf: logf}
		}
		// Close host forward halves before quiesce. Their vsock SHUTDOWNs must
		// reach the guest before the quiesced response becomes the transport
		// barrier; closing them after that response can race /vm.pause and leave
		// teardown packets in the snapshotted virtqueue.
		if forwarder != nil {
			forwarder.CloseActive()
		}
		if err := guestlink.SendQuiesce(client); err != nil {
			logf("quiesce: %v (aborting snapshot)", err)
			return ctl.Response{}, fmt.Errorf("quiesce: %w", err)
		}
		logf("quiesce: guest acked (MUX + forwards closed), proceeding to /vm.pause")
	}

	if req.Upload && (opts.ManifestCfg == nil || opts.ManifestCfg.Store.Endpoint == "") {
		return ctl.Response{}, fmt.Errorf("upload mode requires --manifest-config or MANIFEST_CONFIG (manifest store endpoints)")
	}
	// --output 与 --upload 互斥(docs/sandbox.md §13.3)。CLI 已经做过这道
	// 校验,但 ctl.sock 协议是开放的,run 进程也守在最后一关。
	if req.Upload && req.OutDir != "" {
		return ctl.Response{}, fmt.Errorf("--output and --upload are mutually exclusive")
	}
	if !req.Upload && req.OutDir == "" {
		return ctl.Response{}, fmt.Errorf("--output and --upload are mutually exclusive; one is required")
	}
	stagingDir := filepath.Join(runDir, "snap-stage")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return ctl.Response{}, err
	}
	defer os.RemoveAll(stagingDir)

	// Build snapshot.cfg builder closure. Take() invokes it with the final
	// overlay_ref the sink produced (file://<sha256>.overlay in --output mode,
	// manifest://<key> in --upload mode) so snapshot.cfg's overlay.base is
	// final on first write — no post-hoc ZIP rewrite.
	cfg := opts.Cfg
	snapCfgBuilder := func(overlayRefs []string) ([]byte, error) {
		return buildSnapshotCfg(cfg, overlayRefs)
	}

	// opts.SandboxID is always populated by the run path (generated when the
	// CLI didn't pass one); see Run() in this file.
	sandboxID := opts.SandboxID

	// Pick the sink. --output writes sparse, content-addressed local files;
	// --upload streams to a fresh Ingester (its own store client, never shared
	// with the long-lived read-side fetcher). Either way Take streams the
	// multi-GiB memory + overlay straight to the sink — only CH's tiny
	// config.json/state.json transit the /run tmpfs staging dir.
	var sink snapshot.SnapshotSink
	var ingestSink *snapshot.IngestSink
	if req.Upload {
		ing, ierr := opts.ManifestCfg.NewIngester(opts.ManifestCfg.IngestKeyFunc(), nil)
		if ierr != nil {
			return ctl.Response{}, fmt.Errorf("snapshot ingester: %w", ierr)
		}
		defer ing.Close()
		ingestSink = snapshot.NewIngestSink(ing, logf)
		sink = ingestSink
	} else {
		sink = snapshot.NewFileSink(req.OutDir, sandboxID, logf)
	}

	// Per-disk diff list (root, then data disks) for Take. Restored from a LOCAL
	// snapshot ⇒ MERGE this run's resident delta onto the parent local layer
	// (replace the next-newest layer, not stack) for BOTH --output and --upload;
	// buildSnapshotCfg drops the parent ref to match. Cold/manifest parents stack.
	prov := cfg.SnapshotProvenance
	localParent := strings.HasPrefix(prov.ParentSnapshotRef, "file://")
	diffs := make([]snapshot.DiskDiff, len(disks))
	for i, d := range disks {
		dd := snapshot.DiskDiff{Path: d.DiffPath, Owned: d.OwnedDiff}
		if localParent {
			if i == 0 {
				dd.MergeBase = prov.ParentOverlayPath // root
			} else if j := i - 1; j < len(prov.ParentDisks) {
				dd.MergeBase = prov.ParentDisks[j].OverlayPath
			}
		}
		diffs[i] = dd
	}
	src := snapshot.Sources{
		SandboxID:     sandboxID,
		APISock:       chSock,
		MemfdFD:       mfd.FD(),
		MemfdSize:     int64(mfd.Size()),
		Diffs:         diffs,
		StagingDir:    stagingDir,
		CHApiDeadline: cfg.CHApiDeadline(),
		SnapshotCfg:   snapCfgBuilder,
		Quiescer:      &allQuiescer{servers: servers},
		Logf:          logf,
	}
	if localParent {
		if prov.ParentSnapshotPath == "" || prov.ParentOverlayPath == "" {
			return ctl.Response{}, fmt.Errorf("snapshot: local parent %q lacks merge paths", prov.ParentSnapshotRef)
		}
		src.MergeBaseSnapshot = prov.ParentSnapshotPath
	}
	out, err := snapshot.Take(src, sink, req.ResumeAfter)
	if err != nil {
		return ctl.Response{}, err
	}

	// --resume: Take has resumed CH, but the guest closed the stdio MUX
	// during quiesce. Re-establish it (best-effort — the snapshot itself
	// succeeded; a reattach failure just leaves the resumed sandbox with
	// detached stdio). Do this before any upload work so the app, which is
	// already running again, isn't blocked on a full stdout pipe for long.
	if req.ResumeAfter && reattachMUX != nil {
		if err := reattachMUX(); err != nil {
			logf("stdio MUX re-attach after snapshot failed: %v (sandbox running, stdio detached)", err)
		} else {
			logf("stdio MUX re-attached after snapshot")
		}
	}

	resp = ctl.Response{
		MemorySize:       out.MemorySize,
		MemoryResident:   out.MemoryResident,
		WallclockPauseMs: out.WallclockPauseMs,
		WallclockDumpMs:  out.WallclockDumpMs,
	}
	// The response reports the ROOT overlay (out.*[0]); data-disk overlays live
	// in the bundle's snapshot.cfg (boot.disks[]) and, in --output mode, as
	// sibling files.
	if !req.Upload {
		resp.SnapshotPath = out.SnapshotPath
		resp.OverlayPath = out.OverlayPaths[0]
		resp.OverlayRef = out.OverlayRefs[0]
		return resp, nil
	}

	// Upload mode: the IngestSink already streamed the overlays + bundle to the
	// store during Take (resident pages only). Report the manifest keys it
	// produced (out.*Ref are manifest://<key>) plus per-artifact dedup stats.
	// OverlayRef stays scheme-tagged — it is the snapshot bundle's overlay.base,
	// used for from_refs chaining.
	overlayRes, bundleRes := ingestSink.Results()
	resp.SnapshotManifestKey = strings.TrimPrefix(out.SnapshotRef, "manifest://")
	resp.OverlayManifestKey = strings.TrimPrefix(out.OverlayRefs[0], "manifest://")
	resp.OverlayRef = out.OverlayRefs[0]
	resp.Msg = fmt.Sprintf("upload OK; overlay stored=%d dedup=%d, snapshot stored=%d dedup=%d",
		overlayRes.StoredChunks, overlayRes.DedupChunks, bundleRes.StoredChunks, bundleRes.DedupChunks)
	return resp, nil
}

// destroyAfterSnapshotDelay is how long destroyAfterSnapshot waits before
// tearing the VMM down — long enough for the snapshot_done response to
// flush over ctl.sock back to the `sandbox-ctl snapshot` CLI (a tiny JSON
// over a local UDS; this margin is generous).
const destroyAfterSnapshotDelay = 300 * time.Millisecond

// destroyAfterSnapshot tears the VMM down (PUT /api/v1/vmm.shutdown) so
// the `sandbox-ctl run` process owning this ctl.sock returns. Run on a
// goroutine on the resume_after=false ("destroy") path: by the time the
// delay elapses the snapshot_done response has been queued + sent. Errors
// are only logged — the sandbox is being torn down regardless.
func destroyAfterSnapshot(chSock string, respDeadline time.Duration, logf func(string, ...any)) {
	time.Sleep(destroyAfterSnapshotDelay)
	if err := (chapi.Client{Sock: chSock, RespDeadline: respDeadline}).ShutdownVMM(); err != nil {
		logf("snapshot: destroy mode — vmm.shutdown: %v", err)
		return
	}
	logf("snapshot: destroy mode — VMM shutdown requested")
}

// buildSnapshotCfg renders the snapshot.cfg YAML body per docs §3.4.
// runtime_ref / base_ref are pre-computed by sandbox-ctl at boot
// (file SHA256 is hashed once at startup; see config.SnapshotRefs in
// config.SandboxConfig). overlayRef is filled in by Take() after overlay
// digest is known, or by Upload() after overlay manifest key is known.
func buildSnapshotCfg(cfg *config.SandboxConfig, overlayRefs []string) ([]byte, error) {
	doc := snapshotCfgYAML{}
	doc.Resources.Capacity.CPU = cfg.Resources.Capacity.CPU
	doc.Resources.Capacity.Memory = cfg.Resources.Capacity.Memory
	doc.Metadata = cfg.Metadata
	doc.Boot.RuntimeRef = cfg.SnapshotRefs.RuntimeRef

	// Incremental layered chain (docs/sandbox.md §3.5), keyed on the parent's
	// scheme (same rule for memory, root, and each data disk):
	//   - LOCAL parent (file://): this run's resident delta was MERGED onto the
	//     parent local layer (snapshot.Take's mergeSparse), so the new top
	//     REPLACES the parent — inherit the parent's lower chain, drop the parent
	//     ref (local-layer depth stays 1).
	//   - REMOTE (manifest://) / cold start: prepend the parent ref to stack.
	prov := cfg.SnapshotProvenance
	localParent := strings.HasPrefix(prov.ParentSnapshotRef, "file://")
	coldStart := prov.ParentSnapshotRef == ""
	if localParent {
		doc.FromRefs = prov.ParentFromRefs
	} else {
		doc.FromRefs = prependRef(prov.ParentSnapshotRef, prov.ParentFromRefs)
	}

	// Root node. coldLower is the read-only lower the writable diff sits on: the
	// single-disk CoW base, or the overlay's lower (overlay.base) in two-disk
	// mode — both must be chained on cold start (see renderDiskNode).
	rootColdLower := cfg.Boot.Root.Base
	if cfg.Boot.Root.Overlay != nil {
		rootColdLower = cfg.Boot.Root.Overlay.Base
	}
	doc.Boot.Root = renderDiskNode(overlayRefs[0], cfg.SnapshotRefs.BaseRef,
		rootColdLower, cfg.SingleDisk(), localParent, coldStart,
		prov.ParentOverlayBase, prov.ParentBaseFromRefs)

	// Data-disk nodes (boot.disks[] order), each with its own parent chain.
	for i := range cfg.Boot.Disks {
		d := &cfg.Boot.Disks[i]
		var pTop string
		var pChain []string
		if i < len(prov.ParentDisks) {
			pTop, pChain = prov.ParentDisks[i].OverlayBase, prov.ParentDisks[i].BaseFromRefs
		}
		baseRef := ""
		if i < len(cfg.SnapshotRefs.DiskBaseRefs) {
			baseRef = cfg.SnapshotRefs.DiskBaseRefs[i]
		}
		diskColdLower := d.Base
		if d.Overlay != nil {
			diskColdLower = d.Overlay.Base
		}
		doc.Boot.Disks = append(doc.Boot.Disks, renderDiskNode(overlayRefs[1+i],
			baseRef, diskColdLower, d.RootConfig.Single(), localParent, coldStart, pTop, pChain))
	}
	return yaml.Marshal(&doc)
}

// renderDiskNode renders one disk's snapshot.cfg node (root or a data disk),
// computing its incremental chain. overlayRef is the captured top diff ref;
// baseRef the erofs image ref (overlay mode); coldLower the cold-start read-only
// lower the writable diff sits on — the single-disk CoW base, or the overlay's
// lower (overlay.base) in two-disk mode — to chain. parentTop/parentChain are
// this disk's parent overlay ref + chain — dropped (localParent: merged) or
// prepended (stacked).
func renderDiskNode(overlayRef, baseRef, coldLower string, single, localParent, coldStart bool, parentTop string, parentChain []string) diskNodeYAML {
	var chain []string
	if localParent {
		chain = parentChain
	} else {
		chain = prependRef(parentTop, parentChain)
	}
	// The captured diff is sparse (CoW writes only), so on COLD start the
	// read-only lower the writable diff sits on is not itself captured and must
	// be chained below the diff, else restore loses those base blocks. This is
	// the single-disk CoW base AND — symmetrically — the overlay's lower
	// (overlay.base), e.g. a fromTemplate build's inherited template overlay. A
	// self-contained diff (diff_template, no lower) has coldLower == "".
	if coldStart && coldLower != "" {
		chain = prependRef(coldLower, chain)
	}
	node := diskNodeYAML{}
	if single {
		node.Base = overlayRef
		node.BaseFromRefs = chain
	} else {
		node.BaseRef = baseRef
		node.Overlay = &overlayCfgYAML{Base: overlayRef, BaseFromRefs: chain}
	}
	return node
}

// prependRef returns [ref] ++ rest when ref is non-empty, else nil. Used to
// extend a layered chain by the parent's top layer.
func prependRef(ref string, rest []string) []string {
	if ref == "" {
		return nil
	}
	out := make([]string, 0, 1+len(rest))
	out = append(out, ref)
	out = append(out, rest...)
	return out
}

// diskNodeYAML is one disk's snapshot.cfg node (root or a boot.disks[] entry).
//   - overlay mode: base_ref (erofs image) + overlay{captured upper diff + chain}.
//   - single-disk mode: the captured diff is the child's read-only base; chain
//     below it. (base/base_from_refs mutually exclusive with base_ref/overlay.)
type diskNodeYAML struct {
	BaseRef      string          `yaml:"base_ref,omitempty"`
	Overlay      *overlayCfgYAML `yaml:"overlay,omitempty"`
	Base         string          `yaml:"base,omitempty"`
	BaseFromRefs []string        `yaml:"base_from_refs,omitempty"`
}

// snapshotCfgYAML mirrors the on-disk snapshot.cfg schema. Extracted
// type so buildSnapshotCfg + applyrules.SnapshotCfg share a definition.
type snapshotCfgYAML struct {
	Resources struct {
		Capacity struct {
			CPU    int    `yaml:"cpu"`
			Memory string `yaml:"memory"`
		} `yaml:"capacity"`
	} `yaml:"resources"`
	Metadata map[string]string `yaml:"metadata,omitempty"` // opaque platform passthrough
	FromRefs []string          `yaml:"from_refs,omitempty"`
	Boot     struct {
		RuntimeRef string         `yaml:"runtime_ref"`
		Root       diskNodeYAML   `yaml:"root"`
		Disks      []diskNodeYAML `yaml:"disks,omitempty"`
	} `yaml:"boot"`
}

// overlayCfgYAML is the overlay-mode disk sub-node of snapshotCfgYAML.
type overlayCfgYAML struct {
	Base         string   `yaml:"base"`
	BaseFromRefs []string `yaml:"base_from_refs,omitempty"`
}

// populateSnapshotRefs hashes boot.runtime + boot.root.base (when file://)
// and stores the canonical refs on cfg.SnapshotRefs so buildSnapshotCfg
// can render snapshot.cfg without re-hashing on each request. Called once
// during Run() startup; cost is one streamed read per artifact (typical
// runtime ≈ 5 MiB, base ≈ 100 MiB).
func populateSnapshotRefs(cfg *config.SandboxConfig) error {
	rRef, err := buildBootRef(cfg.Boot.Runtime, false /* fileOnly=false; runtime is file:// only but caller fields enforce */)
	if err != nil {
		return fmt.Errorf("boot.runtime: %w", err)
	}
	cfg.SnapshotRefs.RuntimeRef = rRef

	// Per-data-disk erofs base refs (overlay mode). Indexed by boot.disks[]
	// ordinal; empty entry for single-disk / no-base disks.
	if len(cfg.Boot.Disks) > 0 {
		cfg.SnapshotRefs.DiskBaseRefs = make([]string, len(cfg.Boot.Disks))
		for i := range cfg.Boot.Disks {
			d := &cfg.Boot.Disks[i]
			if d.Single() || d.Base == "" {
				continue
			}
			ref, err := buildBootRef(d.Base, true /* allowManifest */)
			if err != nil {
				return fmt.Errorf("boot.disks[%d].base: %w", i, err)
			}
			cfg.SnapshotRefs.DiskBaseRefs[i] = ref
		}
	}

	if cfg.Boot.Root.Base == "" {
		return nil
	}
	bRef, err := buildBootRef(cfg.Boot.Root.Base, true /* allowManifest */)
	if err != nil {
		return fmt.Errorf("boot.root.base: %w", err)
	}
	cfg.SnapshotRefs.BaseRef = bRef
	return nil
}

// buildBootRef produces the canonical snapshot.cfg ref for a host URL.
//   - file:///abs/path → "file://<basename>@sha256:<hex>"
//   - manifest://<key> → "manifest://<key>" (passthrough; only when
//     allowManifest is true)
func buildBootRef(uri string, allowManifest bool) (string, error) {
	if strings.HasPrefix(uri, "manifest://") {
		if !allowManifest {
			return "", fmt.Errorf("manifest:// not permitted here")
		}
		return uri, nil
	}
	if !strings.HasPrefix(uri, "file://") {
		return "", fmt.Errorf("expected file:// or manifest://, got %q", uri)
	}
	path := strings.TrimPrefix(uri, "file://")
	digest, err := streamFileSha256(path)
	if err != nil {
		return "", err
	}
	return "file://" + filepath.Base(path) + "@sha256:" + digest, nil
}

// streamFileSha256 returns hex(SHA256(file)). Sparse holes read as 0.
func streamFileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func generateSandboxID() string {
	b := make([]byte, 4)
	if f, err := os.Open("/dev/urandom"); err == nil {
		defer f.Close()
		_, _ = f.Read(b)
		return fmt.Sprintf("sb-%x", b)
	}
	return "sb-default"
}

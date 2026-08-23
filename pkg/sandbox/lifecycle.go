package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/runtimebundle"
	"github.com/kuasar-sandbox/sandboxer/pkg/chapi"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
	"github.com/kuasar-sandbox/sandboxer/pkg/tapfd"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
	"golang.org/x/sys/unix"
)

// RunOptions controls a single sandbox-ctl run invocation.
type RunOptions struct {
	Cfg           *config.SandboxConfig
	ManifestCfg   *config.ManifestConfig // for manifest:// resolution; may be nil if all file://
	Fetcher       fetch.Fetcher          // lazily network-backed; caller owns its lifetime
	RefLocations  config.RefLocations    // trusted logical location -> host directory mappings
	SandboxID     string                 // generated if empty
	CHBinary      string                 // path to bin/cloud-hypervisor
	RuntimeRoot   string                 // tmpfs run root (sockets / snap staging); "/run/sandbox" by default
	BaseRoot      string                 // on-disk base root (overlay diff); "/var/lib/sandbox" by default
	StatsJSONPath string                 // if set, write vhost stats as JSON to this path on shutdown
	StatsInterval time.Duration          // if > 0, periodically log lazy-load stats; 0 = off
	StdioMode     stdio.Mode             // CH process stdio wiring; see pkg/stdio
	CustomerKeyFn ingest.CustomerKeyFunc // process-fixed customer key resolver
	LocalCodec    tarstream.Codec        // nil when crypto.local=off
	LocalRequired bool                   // reject plaintext local artifacts and active diffs

	// PingFatalThreshold: after this many consecutive ping failures
	// the host SIGTERMs CH so cmd.Wait() returns. 0 = disabled
	// (default — wait for the user's signal). With the default 1 s
	// ping interval, 30 ≈ 30 s of unreachability. See pinger.go.
	PingFatalThreshold int

	// Forwards are parsed `--connect` port-forward directives (forward.go).
	// Each opens a host-local listener whose connections are spliced to a
	// guest-side target. Empty → no port forwarding.
	Forwards []ForwardSpec

	// NotifyReadiness receives the one-shot startup milestones for this run.
	// nil preserves the historical behavior exactly.
	NotifyReadiness ReadinessNotify
}

// Run executes one sandbox lifecycle: prepare backends + launch server,
// spawn CH, wait for exit, cleanup. Returns CH's exit code or an error
// if setup failed.
func Run(ctx context.Context, opts RunOptions) (int, error) {
	startUnixNs := time.Now().UnixNano()
	if opts.Cfg == nil {
		return -1, fmt.Errorf("RunOptions.Cfg is nil")
	}
	if opts.LocalRequired && opts.LocalCodec == nil {
		return -1, fmt.Errorf("RunOptions.LocalRequired requires LocalCodec")
	}
	if opts.LocalCodec != nil && opts.CustomerKeyFn == nil {
		return -1, fmt.Errorf("RunOptions.LocalCodec requires CustomerKeyFn")
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
	var diffCustomerKey [32]byte
	if opts.LocalCodec != nil {
		var err error
		diffCustomerKey, err = opts.CustomerKeyFn()
		if err != nil {
			return -1, fmt.Errorf("diff customer key: %w", err)
		}
		defer clear(diffCustomerKey[:])
	}
	// tap-name mode: CH opens the host tap, so verify it exists now.
	// tapfd mode: the fd comes from the provider handoff below (no host tap
	// to verify here).
	if opts.Cfg.Network.TAP != "" {
		if err := VerifyTAP(opts.Cfg.Network.TAP); err != nil {
			return -1, err
		}
	}
	if err := populateSnapshotRefs(opts.Cfg, opts.RefLocations, opts.LocalCodec, opts.LocalRequired); err != nil {
		return -1, fmt.Errorf("snapshot refs: %w", err)
	}
	needsManifest := needsManifestFetcher(opts.Cfg)
	if needsManifest {
		if opts.Fetcher == nil {
			return -1, fmt.Errorf("manifest:// disk requires --manifest-config or MANIFEST_CONFIG")
		}
		// The CLI's resolver is memoized. Resolve the key before controller,
		// cgroup, or run-directory side effects while leaving network clients
		// lazy until the first actual manifest open.
		if opts.CustomerKeyFn != nil {
			if _, err := opts.CustomerKeyFn(); err != nil {
				return -1, fmt.Errorf("manifest customer key: %w", err)
			}
		}
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

	// Resolve the exact memory domain before admission. The local controller,
	// CH command line, and node reservation all use these same byte values.
	capBytes, err := opts.Cfg.CapacityMemoryBytes()
	if err != nil {
		return -1, err
	}
	if capBytes > uint64(^uint(0)>>1) {
		return -1, fmt.Errorf("memory Capacity %d exceeds host addressable memory size", capBytes)
	}
	allocBytes, err := opts.Cfg.AllocatableMemoryBytes()
	if err != nil {
		return -1, err
	}
	startupHeadroom, err := opts.Cfg.StartupBytes()
	if err != nil {
		return -1, err
	}
	initialBudget := resctl.AlignedBudget(capBytes, startupHeadroom)
	// Reject a CH-inexpressible memory domain before creating a lease or
	// acquiring any node reservation. Validate both the cold command-line
	// target and the farthest target the settled policy can request.
	if err := resctl.ValidateBalloonSize(capBytes, resctl.TargetForBudget(capBytes, initialBudget)); err != nil {
		return -1, fmt.Errorf("initial memory domain: %w", err)
	}
	if err := resctl.ValidateBalloonSize(capBytes, resctl.TargetForBudget(capBytes, allocBytes)); err != nil {
		return -1, fmt.Errorf("settled memory domain: %w", err)
	}

	// Create the CH executor whenever either cold or settled policy can use a
	// balloon. InitialTarget=0 still emits --balloon size=0.
	var balloonCtl *resctl.BalloonController
	if allocBytes < capBytes || initialBudget < capBytes {
		balloonCtl = resctl.NewBalloonController(chSock, capBytes, opts.Cfg.CHApiDeadline(), logf)
	}
	// Register cgroup cleanup before ControllerHooks.Release so LIFO shutdown
	// stops every controller goroutine while its pinned cgroup FD is still live.
	var cg *resctl.CgroupController
	defer func() {
		if cg != nil {
			_ = cg.Cleanup()
		}
	}()

	// Controller handshake (dynamic mode) happens before cgroup writes so a
	// rejected admission leaves no local resource side effects. Admit carries
	// the resolved host cgroup path from cfg; local I/O is pinned to the stable
	// cgroup descriptor after SetupCgroup below.
	hooks, err := resctl.NewControllerHooks(resctl.ControllerHookOptions{
		SocketPath: opts.Cfg.Resources.Control.Controller,
		SandboxID:  opts.SandboxID,
		Context:    ControllerWorkContext(ctx),
		Logf:       logf,
	}, opts.Cfg)
	if err != nil {
		return -1, fmt.Errorf("controller dial: %w", err)
	}
	defer hooks.Release("normal")
	grantedInitial, err := hooks.Admit(opts.SandboxID, 0)
	if err != nil {
		return -1, err
	}
	initialBudget = grantedInitial
	logf("initial cold Budget reserved=%d", initialBudget)
	if balloonCtl != nil {
		if err := balloonCtl.SeedColdTarget(resctl.TargetForBudget(capBytes, initialBudget)); err != nil {
			return -1, fmt.Errorf("seed cold balloon target: %w", err)
		}
	}

	// CgroupPath empty → no-cgroup mode, no cgroup operations.
	// CgroupPath set → join existing cgroup (must already exist; not
	// created by sandbox-ctl). See docs/sandbox.md §4.1.
	cgCfg, err := resctl.BuildCgroupConfig(opts.Cfg)
	if err != nil {
		return -1, err
	}
	// Defer memory.high write until a fresh post-launch guest/CH observation.
	// Cold boot's uffd-driven page-fault burst can push the cgroup well past
	// its eventual steady high; if memory.high is already in effect, every
	// UFFDIO_ZEROPAGE/COPY syscall returns through
	// mem_cgroup_handle_over_high reclaim, throttling the uffd handler
	// against the very faults it's trying to resolve (Issue 4 root cause).
	// memory.max remains the hard ceiling during boot. Settled is only the
	// lifecycle barrier; MemoryController derives and writes the first high.
	cgCfg.MemoryHighBytes = 0
	cg, err = resctl.SetupCgroup(cgCfg)
	if err != nil {
		return -1, fmt.Errorf("cgroup: %w", err)
	}
	hooks.SetLocalCgroupPath(cg.LocalPath())
	memoryCtl, err := resctl.NewMemoryController(resctl.MemoryControllerOptions{
		Config: opts.Cfg, CgroupPath: cg.LocalPath(), Balloon: balloonCtl,
		Reservation: hooks, InitialBudget: initialBudget, Logf: logf,
	})
	if err != nil {
		return -1, fmt.Errorf("memory controller: %w", err)
	}
	if cg.Active() {
		logf("cgroup limits set: %s memory.max=%d memory.high=max(deferred) cpu.max=%dus/100000us cpu.weight=%d (CH starts in cgroup; sandbox-ctl stays out)",
			cg.Path, cgCfg.MemoryMaxBytes,
			cgCfg.CPUMaxQuotaUs, cgCfg.CPUWeight)
	} else {
		logf("cgroup: no path configured, running without cgroup limits (no-cgroup mode)")
	}

	// Use the caller-owned read-side fetcher when a disk uses manifest://.
	// The CLI wrapper creates cache/store clients only on its first actual
	// OpenManifest; file-only configs never dial. snapshot --upload builds its
	// own write-side Ingester at request time, so the two paths do not share a
	// connection pool.
	fetcher := opts.Fetcher
	if needsManifest {
		if opts.ManifestCfg != nil {
			logf("manifest fetcher: store=%s cache=%s crypto=%s/%s",
				opts.ManifestCfg.Store.Endpoint, opts.ManifestCfg.Cache.Endpoint,
				opts.ManifestCfg.Crypto.Chunk, opts.ManifestCfg.Crypto.Manifest)
		}
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
		r, _, err := OpenBlockReader(ctx, opts.Cfg.Boot.Root.Base, fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
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
	// overrides the static mac/ip attrs. The resolved spec travels
	// through the launch handshake; nil → "no IP configuration" to sandbox-init.
	var tapFile, netnsFile *os.File
	var metaMAC, metaIP string
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
		metaMAC, metaIP = meta.MAC, meta.IP
		logf("tapfd: received tap fd (mac=%s ip=%s netns=%t)", meta.MAC, meta.IP, netnsFile != nil)
	}
	netMAC, netSpec := opts.Cfg.Network.Effective(metaMAC, metaIP)
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
			refs := append([]string{opts.Cfg.Boot.Root.Base}, opts.Cfg.Boot.Root.BaseFromRefs...)
			r, _, err := OpenLayeredBlockReader(ctx, refs, fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
			if err != nil {
				return -1, fmt.Errorf("single-disk root base: %w", err)
			}
			cowBase = r
			defer cowBase.Close()
		}
	} else {
		diffURI, diffTemplate = opts.Cfg.Boot.Root.Overlay.Diff, opts.Cfg.Boot.Root.Overlay.DiffTemplate
		if opts.Cfg.Boot.Root.Overlay.Base != "" {
			refs := append([]string{opts.Cfg.Boot.Root.Overlay.Base}, opts.Cfg.Boot.Root.Overlay.BaseFromRefs...)
			r, _, err := OpenLayeredBlockReader(ctx, refs, fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
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
	diffInit, err := PrepareDiff(diffPath, diffTemplate, baseSize, diffSize)
	if err != nil {
		return -1, fmt.Errorf("prepare diff: %w", err)
	}
	var cowOptions []vhost.BlockCOWOption
	if opts.LocalCodec != nil {
		cowOptions = append(cowOptions, vhost.WithDiffEncryption(diffCustomerKey, opts.LocalRequired))
	}
	cow, err := vhost.OpenBlockCOW(diffPath, cowBase, diffInit, cowOptions...)
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
		db, dcleanup, derr := prepColdDataDisk(ctx, &opts.Cfg.Boot.Disks[i], i, opts.BaseRoot, opts.SandboxID, fetcher, opts.RefLocations, opts.LocalCodec, diffCustomerKey, opts.LocalRequired)
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
		Hooks:             hooks,
		Memory:            memoryCtl,

		TapFile:   tapFile, // nil in tap-name/no-network modes; non-nil tapfd is inherited at fd 4
		NetMAC:    netMAC,
		NetnsFile: netnsFile, // non-nil → launch CH inside the tap's netns

		SnapCfg:           opts.Cfg,
		ManifestCfg:       opts.ManifestCfg,
		CustomerKeyFn:     opts.CustomerKeyFn,
		LocalCodec:        opts.LocalCodec,
		LocalRequired:     opts.LocalRequired,
		Forwards:          opts.Forwards,
		Cgroup:            cg,
		NotifyReadiness:   opts.NotifyReadiness,
		ReadyOnAppStarted: true,

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
			args, err := CHCommandWithInitialBudget(opts.Cfg, initialBudget, e.Disks, e.CHSock, e.VsockBase, kernelPath, runtimePath, e.UffdSock, consoleArg, e.TapFDNum, e.NetMAC)
			if err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("CH cmdline: %w", err)
			}
			cmd.Args = append(cmd.Args, args...)
			logf("CH args: %s", strings.Join(args, " "))
			return cmd, cleanup, nil
		},

		// Cold-start settle: the command-line balloon target remains the sole
		// target until launch_ack. The ACK opens the local report barrier;
		// node Settled remains an independent lifecycle notification.
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
					if pc.Memory != nil {
						pc.Memory.StartSensor(pc.Ctx)
					}
					if err := pc.Hooks.Settled(); err != nil {
						pc.Logf("settled: %v", err)
					}
					if pc.Hooks.Enabled() {
						pc.Hooks.StartHeartbeat(pc.Ctx, 5*time.Second)
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

// MemoryController holds the memory.high lifecycle lock only around sandbox-local
// cgroupfs reads and writes. A brief retry window lets ordered shutdown wait out
// that commit without blocking the signal loop. Persistent contention is treated
// as an active lifecycle operation, where waiting for the lock could deadlock
// with snapshot destroy waiting for this same CH process to exit.
const (
	memoryHighLockRetryInterval = 10 * time.Millisecond
	memoryHighLockRetryWindow   = 100 * time.Millisecond
)

// liftVMMMemoryHigh temporarily removes memory.high throttling from the VMM
// cgroup. Lifecycle API calls need every CH thread to return to userspace; a
// thread sleeping in mem_cgroup_handle_over_high cannot acknowledge pause or
// ordered shutdown even though sandbox-ctl itself remains responsive outside
// the cgroup. memory.max remains in force while memory.high is lifted. The
// advisory lock is also taken by MemoryController, so a local Budget update
// cannot reinstate throttling inside the lifecycle critical section.
func liftVMMMemoryHigh(cgroupPath string) ([]byte, *os.File, error) {
	return liftVMMMemoryHighWithLock(cgroupPath, unix.LOCK_EX)
}

func tryLiftVMMMemoryHigh(cgroupPath string) ([]byte, *os.File, error) {
	return liftVMMMemoryHighWithLock(cgroupPath, unix.LOCK_EX|unix.LOCK_NB)
}

func liftVMMMemoryHighWithLock(cgroupPath string, lockOperation int) ([]byte, *os.File, error) {
	if cgroupPath == "" {
		return nil, nil, nil
	}
	lock, err := os.Open(cgroupPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open memory.high lifecycle lock: %w", err)
	}
	fail := func(err error) ([]byte, *os.File, error) {
		_ = lock.Close()
		return nil, nil, err
	}
	if err := unix.Flock(int(lock.Fd()), lockOperation); err != nil {
		return fail(fmt.Errorf("lock memory.high lifecycle: %w", err))
	}
	path := filepath.Join(cgroupPath, "memory.high")
	previous, err := os.ReadFile(path)
	if err != nil {
		return fail(fmt.Errorf("read %s: %w", path, err))
	}
	if strings.TrimSpace(string(previous)) == "max" {
		return previous, lock, nil
	}
	if err := os.WriteFile(path, []byte("max"), 0o644); err != nil {
		return fail(fmt.Errorf("write %s: %w", path, err))
	}
	return previous, lock, nil
}

// restoreVMMMemoryHigh restores a value saved by liftVMMMemoryHigh only while
// the file still contains "max". MemoryController updates are serialized by the
// lifecycle lock; the conditional write also avoids overwriting a change made by
// an external writer that does not participate in that lock.
func restoreVMMMemoryHigh(cgroupPath string, previous []byte) (bool, error) {
	if cgroupPath == "" || previous == nil || strings.TrimSpace(string(previous)) == "max" {
		return false, nil
	}
	path := filepath.Join(cgroupPath, "memory.high")
	current, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if strings.TrimSpace(string(current)) != "max" {
		return false, nil
	}
	if err := os.WriteFile(path, previous, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// memoryHighThrottleDrain covers Linux's maximum per-batch memory.high delay
// (2 seconds) plus a small scheduling margin. Raising memory.high stops new
// throttling, but it does not wake a task already sleeping in
// mem_cgroup_handle_over_high. CH deliberately keeps its vCPU kick signal
// blocked outside KVM_RUN, so snapshot and ordered shutdown must let that
// existing sleep expire before starting CH's one-second acknowledgement
// window. A zero memory.events.local high counter proves that this cgroup has
// never entered the throttle; otherwise a below-watermark point sample alone
// is insufficient and the path drains conservatively.
const memoryHighThrottleDrain = 2100 * time.Millisecond

func vmmMemoryHighThrottleDrainDelay(cgroupPath string, previous []byte) time.Duration {
	if cgroupPath == "" || previous == nil {
		return 0
	}
	high, err := strconv.ParseUint(strings.TrimSpace(string(previous)), 10, 64)
	if err != nil {
		return 0
	}
	currentRaw, err := os.ReadFile(filepath.Join(cgroupPath, "memory.current"))
	if err != nil {
		return memoryHighThrottleDrain
	}
	current, err := strconv.ParseUint(strings.TrimSpace(string(currentRaw)), 10, 64)
	if err != nil {
		return memoryHighThrottleDrain
	}
	if current > high {
		return memoryHighThrottleDrain
	}
	eventsRaw, err := os.ReadFile(filepath.Join(cgroupPath, "memory.events.local"))
	if err != nil {
		return memoryHighThrottleDrain
	}
	fields := strings.Fields(string(eventsRaw))
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] != "high" {
			continue
		}
		highEvents, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil || highEvents > 0 {
			return memoryHighThrottleDrain
		}
		return 0
	}
	// Without an authoritative zero high-event count, a below-watermark
	// sample cannot exclude a task that is still serving an earlier delay.
	return memoryHighThrottleDrain
}

func waitVMMMemoryHighThrottleDrain(delay time.Duration, chExited <-chan struct{}) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	if chExited == nil {
		<-timer.C
		return nil
	}
	select {
	case <-timer.C:
		return nil
	case <-chExited:
		return fmt.Errorf("Cloud Hypervisor exited while draining memory.high throttles")
	}
}

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
//  2. If lifting memory.high or the API call fails (CH dead, socket closed,
//     etc.) fall back to forwarding SIGTERM to the CH process.
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
	cgroupPath string,
	chRespDeadline time.Duration,
	grace time.Duration,
	logf func(format string, args ...any),
) error {
	var killTimer *time.Timer
	var killCh <-chan time.Time
	var drainTimer *time.Timer
	var drainCh <-chan time.Time
	var memoryHighLockRetryTimer *time.Timer
	var memoryHighLockRetryCh <-chan time.Time
	var memoryHighLockRetryDeadline time.Time
	var memoryHighLock *os.File
	var pendingShutdownSignal os.Signal
	shutdownInitiated := false
	finishShutdownRequest := func(sig os.Signal, useAPI bool) {
		usedAPI := false
		if useAPI {
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
		killTimer = time.NewTimer(grace)
		killCh = killTimer.C
	}
	attemptShutdownRequest := func(sig os.Signal) {
		useAPI := false
		if chSock != "" {
			previousHigh, lock, liftErr := tryLiftVMMMemoryHigh(cgroupPath)
			if liftErr != nil {
				if errors.Is(liftErr, unix.EWOULDBLOCK) {
					now := time.Now()
					if memoryHighLockRetryDeadline.IsZero() {
						memoryHighLockRetryDeadline = now.Add(memoryHighLockRetryWindow)
						logf("received %v, memory.high lifecycle lock busy; retrying for up to %s before SIGTERM fallback", sig, memoryHighLockRetryWindow)
					}
					remaining := time.Until(memoryHighLockRetryDeadline)
					if remaining > 0 {
						retryDelay := memoryHighLockRetryInterval
						if remaining < retryDelay {
							retryDelay = remaining
						}
						pendingShutdownSignal = sig
						memoryHighLockRetryTimer = time.NewTimer(retryDelay)
						memoryHighLockRetryCh = memoryHighLockRetryTimer.C
						return
					}
					memoryHighLockRetryDeadline = time.Time{}
					logf("received %v, memory.high lifecycle lock remained busy for %s; falling back to SIGTERM", sig, memoryHighLockRetryWindow)
				} else {
					memoryHighLockRetryDeadline = time.Time{}
					logf("received %v, could not lift VMM memory.high before shutdown: %v", sig, liftErr)
				}
			} else {
				memoryHighLockRetryDeadline = time.Time{}
				memoryHighLock = lock
				if previousHigh != nil && strings.TrimSpace(string(previousHigh)) != "max" {
					logf("received %v, lifted VMM memory.high for ordered shutdown", sig)
				}
				if drainDelay := vmmMemoryHighThrottleDrainDelay(cgroupPath, previousHigh); drainDelay > 0 {
					logf("received %v, waiting %s for existing VMM memory.high throttles to drain", sig, drainDelay)
					pendingShutdownSignal = sig
					drainTimer = time.NewTimer(drainDelay)
					drainCh = drainTimer.C
					return
				}
				useAPI = true
			}
		}
		finishShutdownRequest(sig, useAPI)
	}
	for {
		select {
		case sig := <-sigCh:
			if !shutdownInitiated {
				shutdownInitiated = true
				attemptShutdownRequest(sig)
			} else {
				logf("received %v while shutdown in progress, sending SIGKILL now", sig)
				_ = proc.Signal(syscall.SIGKILL)
				if memoryHighLockRetryTimer != nil {
					memoryHighLockRetryTimer.Stop()
					memoryHighLockRetryTimer = nil
				}
				memoryHighLockRetryCh = nil
				if drainTimer != nil {
					drainTimer.Stop()
					drainTimer = nil
				}
				drainCh = nil
				if killTimer != nil {
					killTimer.Stop()
				}
				killCh = nil
			}
		case <-memoryHighLockRetryCh:
			memoryHighLockRetryCh = nil
			memoryHighLockRetryTimer = nil
			attemptShutdownRequest(pendingShutdownSignal)
		case <-drainCh:
			drainCh = nil
			drainTimer = nil
			finishShutdownRequest(pendingShutdownSignal, true)
		case <-killCh:
			logf("CH didn't exit within %s of shutdown request, sending SIGKILL pid=%d", grace, chPid)
			_ = proc.Signal(syscall.SIGKILL)
			killCh = nil
		case waitErr := <-doneCh:
			if memoryHighLockRetryTimer != nil {
				memoryHighLockRetryTimer.Stop()
			}
			if drainTimer != nil {
				drainTimer.Stop()
			}
			if killTimer != nil {
				killTimer.Stop()
			}
			if memoryHighLock != nil {
				if err := memoryHighLock.Close(); err != nil {
					logf("release VMM memory.high lifecycle lock: %v", err)
				}
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
		"fault_queue_wait_ns",
		"fault_queue_wait_p50",
		"fault_queue_wait_p95",
		"fault_queue_wait_p99",
		"fault_queue_depth",
		"fault_queue_depth_hwm",
		"fault_inflight",
		"fault_inflight_hwm",
		"source_read_calls",
		"source_read_bytes",
		"source_read_ns",
		"urgent_copy_calls",
		"urgent_copy_ns",
		"urgent_zero_calls",
		"urgent_zero_ns",
		"tail_submitted",
		"tail_dropped_busy",
		"tail_canceled",
		"tail_buffered_data",
		"tail_deferred_data",
		"tail_zero",
		"tail_pages_planned",
		"tail_pages_completed",
		"tail_copy_ns",
		"tail_zero_ns",
		"tail_conflicts",
		"tail_partial",
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
func prepColdDataDisk(ctx context.Context, d *config.DiskConfig, ordinal int, baseRoot, sandboxID string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, diffCustomerKey [32]byte, required bool) (DiskBackend, func(), error) {
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
		r, _, err := OpenBlockReader(ctx, d.Base, fetcher, locations, codec, required)
		if err != nil {
			return fail(fmt.Errorf("%s erofs base: %w", field, err))
		}
		db.Reader, db.BasePath = r, d.Base
		closers = append(closers, func() { r.Close() })
		cowBaseURI, diffURI, diffTemplate = d.Overlay.Base, d.Overlay.Diff, d.Overlay.DiffTemplate
	}

	var cowBase vhost.BlockReader
	if cowBaseURI != "" {
		var fromRefs []string
		if single {
			fromRefs = d.BaseFromRefs
		} else {
			fromRefs = d.Overlay.BaseFromRefs
		}
		refs := append([]string{cowBaseURI}, fromRefs...)
		r, _, err := OpenLayeredBlockReader(ctx, refs, fetcher, locations, codec, required)
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
	diffInit, err := PrepareDiff(diffPath, diffTemplate, baseSize, diffSize)
	if err != nil {
		return fail(fmt.Errorf("%s prepare diff: %w", field, err))
	}
	var cowOptions []vhost.BlockCOWOption
	if codec != nil {
		cowOptions = append(cowOptions, vhost.WithDiffEncryption(diffCustomerKey, required))
	}
	cow, err := vhost.OpenBlockCOW(diffPath, cowBase, diffInit, cowOptions...)
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
	Cfg           *config.SandboxConfig
	ManifestCfg   *config.ManifestConfig // required for --upload; the Ingester is built lazily per snapshot
	CustomerKeyFn ingest.CustomerKeyFunc
	LocalCodec    tarstream.Codec
	LocalRequired bool
	SandboxID     string // required: snapshot.Take rejects empty
	Memfd         *memory.Memfd
	Disks         []SnapDiskRef   // writable diffs to capture, logical order (root, then data disks)
	Servers       []*vhost.Server // all vhost servers (quiesced together around the dump)
	CHSock        string
	RunDir        string
	Pinger        *guestlink.Pinger        // optional; if non-nil, paused around quiesce/Take
	Forwarder     *Forwarder               // optional; if non-nil, paused + active relays collapsed around quiesce
	Reattach      func() error             // optional; re-establishes the stdio MUX whenever a snapshot attempt resumes
	Memory        *resctl.MemoryController // optional; blocks local Budget mutation across capture
	Logf          func(string, ...any)
}

// Handle dispatches one ctl snapshot_request. Public for restore.Run.
func (h *SnapshotHandler) Handle(req ctl.Request) (ctl.Response, error) {
	return h.handle(req, "", nil)
}

func (h *SnapshotHandler) handle(req ctl.Request, cgroupPath string, chExited <-chan struct{}) (ctl.Response, error) {
	opts := RunOptions{
		Cfg: h.Cfg, ManifestCfg: h.ManifestCfg, SandboxID: h.SandboxID,
		CustomerKeyFn: h.CustomerKeyFn, LocalCodec: h.LocalCodec, LocalRequired: h.LocalRequired,
	}
	return handleSnapshotRequest(req, opts, h.Memfd, h.Disks, h.Servers, h.CHSock, h.RunDir, cgroupPath, chExited, h.Pinger, h.Forwarder, h.Reattach, h.Memory, h.Logf)
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
	cgroupPath string,
	chExited <-chan struct{},
	pinger *guestlink.Pinger,
	forwarder *Forwarder, // gates new forwards + collapses active relays around quiesce; may be nil
	reattachMUX func() error, // re-establishes the stdio MUX after a resumed snapshot attempt; may be nil
	memoryController *resctl.MemoryController,
	logf func(string, ...any),
) (resp ctl.Response, err error) {
	dropCaches := req.DropCachesEnabled()
	mergeRef := req.MergeRefEnabled()
	prov := opts.Cfg.SnapshotProvenance
	localParent := false
	if prov.ParentSnapshotRef != "" {
		parentRef, parseErr := manifest.ParseRef(prov.ParentSnapshotRef)
		if parseErr != nil {
			return ctl.Response{}, fmt.Errorf("parent snapshot ref: %w", parseErr)
		}
		localParent = !parentRef.Portable()
	}

	// Finish every predictable request/artifact/directory check before pausing
	// forwards or the pinger and, critically, before asking the guest to freeze.
	if opts.SandboxID == "" {
		return ctl.Response{}, fmt.Errorf("snapshot: empty sandbox id")
	}
	if len(disks) != 1+len(opts.Cfg.Boot.Disks) {
		return ctl.Response{}, fmt.Errorf("snapshot: runtime disk count %d does not match configured disk count %d",
			len(disks), 1+len(opts.Cfg.Boot.Disks))
	}
	if req.Upload && (opts.ManifestCfg == nil || opts.ManifestCfg.Store.Endpoint == "") {
		return ctl.Response{}, fmt.Errorf("upload mode requires --manifest-config or MANIFEST_CONFIG (manifest store endpoints)")
	}
	if req.Upload && req.OutDir != "" {
		return ctl.Response{}, fmt.Errorf("--output and --upload are mutually exclusive")
	}
	if !req.Upload && req.OutDir == "" {
		return ctl.Response{}, fmt.Errorf("--output and --upload are mutually exclusive; one is required")
	}
	if localParent && !mergeRef && req.Upload {
		return ctl.Response{}, fmt.Errorf("--merge-ref=false with a local memory parent requires --output; direct upload is not supported")
	}

	// Enter the memory lifecycle barrier immediately after request-shape
	// validation. Artifact/merge/sink preparation may perform I/O; allowing a
	// Budget shrink during that interval recreates the warm-up→freeze cache loss
	// from #114 even though the later quiesce itself is serialized.
	releaseMemoryBarrier := func() {}
	if memoryController != nil {
		var barrierErr error
		releaseMemoryBarrier, barrierErr = memoryController.BeginSnapshot(context.Background())
		if barrierErr != nil {
			return ctl.Response{}, fmt.Errorf("snapshot: begin memory Budget barrier: %w", barrierErr)
		}
	}
	defer func() {
		// Successful destroy mode transfers the barrier to
		// destroyAfterSnapshot, which retains it until CH exits.
		if err == nil && !req.ResumeAfter {
			return
		}
		releaseMemoryBarrier()
	}()

	stagingDir := filepath.Join(runDir, "snap-stage")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return ctl.Response{}, fmt.Errorf("snapshot staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDir)
	if !req.Upload {
		if err := ensureSnapshotDir(req.OutDir); err != nil {
			return ctl.Response{}, fmt.Errorf("snapshot output directory: %w", err)
		}
	}

	if prov.ParentSnapshotRef != "" && len(prov.ParentDisks) != max(0, len(disks)-1) {
		return ctl.Response{}, fmt.Errorf("snapshot: parent disk provenance count %d does not match runtime data disk count %d",
			len(prov.ParentDisks), max(0, len(disks)-1))
	}
	diffs := make([]snapshot.DiskDiff, len(disks))
	diskMerged := make([]bool, len(disks))
	for i, d := range disks {
		dd := snapshot.DiskDiff{
			Path:         d.DiffPath,
			Owned:        d.OwnedDiff,
			SnapshotView: d.SnapshotView,
		}
		if dd.SnapshotView == nil {
			return ctl.Response{}, fmt.Errorf("snapshot: disk %d diff %q has no snapshot view", i, dd.Path)
		}
		if d.Size <= 0 {
			return ctl.Response{}, fmt.Errorf("snapshot: disk %d diff %q has invalid logical size %d", i, dd.Path, d.Size)
		}
		diffSize := d.Size
		var parentDiskRef, parentDiskPath string
		if i == 0 {
			parentDiskRef = prov.ParentOverlayBase
			parentDiskPath = prov.ParentOverlayPath
		} else if i-1 < len(prov.ParentDisks) {
			parentDiskRef = prov.ParentDisks[i-1].OverlayBase
			parentDiskPath = prov.ParentDisks[i-1].OverlayPath
		}
		mergeDisk := false
		if parentDiskRef != "" {
			ref, parseErr := manifest.ParseRef(parentDiskRef)
			if parseErr != nil {
				return ctl.Response{}, fmt.Errorf("snapshot: disk %d parent ref: %w", i, parseErr)
			}
			mergeDisk = !ref.Portable()
		}
		if mergeDisk {
			if parentDiskPath == "" {
				return ctl.Response{}, fmt.Errorf("snapshot: local parent disk %d lacks a merge base path", i)
			}
			dd.MergeBase, err = resolvedLocalMergeRef(parentDiskPath, parentDiskRef)
			if err != nil {
				return ctl.Response{}, fmt.Errorf("snapshot: disk %d merge base identity: %w", i, err)
			}
			if err := snapshot.ValidateMergeBase(dd.MergeBase, diffSize, opts.LocalCodec, opts.LocalRequired); err != nil {
				return ctl.Response{}, fmt.Errorf("snapshot: disk %d merge base: %w", i, err)
			}
			diskMerged[i] = true
		} else {
			dd.MergeBase = ""
		}
		diffs[i] = dd
	}

	mergeMemory := localParent && mergeRef
	memoryMergeBase := ""
	if mergeMemory {
		if prov.ParentSnapshotPath == "" {
			return ctl.Response{}, fmt.Errorf("snapshot: local parent lacks a memory merge path")
		}
		memoryMergeBase, err = resolvedLocalMergeRef(prov.ParentSnapshotPath, prov.ParentSnapshotRef)
		if err != nil {
			return ctl.Response{}, fmt.Errorf("snapshot: memory merge base identity: %w", err)
		}
		if err := snapshot.ValidateMergeBase(memoryMergeBase, int64(mfd.Size()), opts.LocalCodec, opts.LocalRequired); err != nil {
			return ctl.Response{}, fmt.Errorf("snapshot: memory merge base: %w", err)
		}
	}
	resultMemoryRefs, err := snapshotMemoryRefs(prov, mergeMemory)
	if err != nil {
		return ctl.Response{}, err
	}
	if !req.Upload {
		if err := validateLocalMemoryRefs(req.OutDir, resultMemoryRefs, opts.LocalCodec, opts.LocalRequired); err != nil {
			return ctl.Response{}, err
		}
	}
	if req.Upload {
		if err := validatePortableMemoryRefs(resultMemoryRefs); err != nil {
			return ctl.Response{}, err
		}
	}

	// Construct the sink before quiesce as well: malformed manifest/store
	// configuration must not be discovered only after the guest is frozen.
	var sink snapshot.SnapshotSink
	var ingestSink *snapshot.IngestSink
	if req.Upload {
		if opts.CustomerKeyFn == nil {
			return ctl.Response{}, fmt.Errorf("snapshot ingester: customer key resolver is unavailable")
		}
		if _, keyErr := opts.CustomerKeyFn(); keyErr != nil {
			return ctl.Response{}, fmt.Errorf("snapshot ingester: customer key: %w", keyErr)
		}
		ing, ierr := opts.ManifestCfg.NewIngester(opts.CustomerKeyFn, nil)
		if ierr != nil {
			return ctl.Response{}, fmt.Errorf("snapshot ingester: %w", ierr)
		}
		defer ing.Close()
		ingestSink = snapshot.NewIngestSink(ing, logf)
		sink = ingestSink
	} else {
		sink = snapshot.NewFileSink(req.OutDir, opts.SandboxID, opts.LocalCodec, opts.LocalRequired, logf)
	}
	reattachRunningGuest := func(reason string) {
		if reattachMUX == nil {
			return
		}
		if reattachErr := reattachMUX(); reattachErr != nil {
			logf("stdio MUX re-attach after %s failed: %v (sandbox running, stdio detached)", reason, reattachErr)
		} else {
			logf("stdio MUX re-attached after %s", reason)
		}
	}
	previousMemoryHigh, memoryHighLock, liftErr := liftVMMMemoryHigh(cgroupPath)
	if liftErr != nil {
		return ctl.Response{}, fmt.Errorf("snapshot: lift VMM memory.high: %w", liftErr)
	}
	if previousMemoryHigh != nil && strings.TrimSpace(string(previousMemoryHigh)) != "max" {
		logf("snapshot: lifted VMM memory.high for quiesce/pause/snapshot lifecycle")
	}
	defer func() {
		// Successful destroy mode leaves CH paused and keeps memory.high lifted
		// until destroyAfterSnapshot completes its ordered VMM shutdown. Every
		// running/error path restores enforcement before returning.
		if err == nil && !req.ResumeAfter {
			return
		}
		restored, restoreErr := restoreVMMMemoryHigh(cgroupPath, previousMemoryHigh)
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("snapshot: restore VMM memory.high: %w", restoreErr))
		} else if restored {
			logf("snapshot: restored VMM memory.high after lifecycle operation")
		}
		if memoryHighLock != nil {
			if closeErr := memoryHighLock.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("snapshot: release VMM memory.high lifecycle lock: %w", closeErr))
			}
		}
	}()
	if drainDelay := vmmMemoryHighThrottleDrainDelay(cgroupPath, previousMemoryHigh); drainDelay > 0 {
		logf("snapshot: waiting %s for existing VMM memory.high throttles to drain", drainDelay)
		if drainErr := waitVMMMemoryHighThrottleDrain(drainDelay, chExited); drainErr != nil {
			return ctl.Response{}, fmt.Errorf("snapshot: drain VMM memory.high throttles: %w", drainErr)
		}
	}

	dropCachesResult := proto.DropCachesUnknown
	guestQuiesced := false

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
			go destroyAfterSnapshot(chSock, memoryHighLock, releaseMemoryBarrier, chExited, opts.Cfg.CHApiDeadline(), logf)
		}
	}()
	// Gate new port-forward connects and join every admitted handshake/relay
	// before asking the guest to quiesce. The later `quiesced` response is then
	// a barrier after both host and guest teardown, so no forward shutdown can
	// race /vm.pause. Resume mirrors the pinger: stay paused only on the success
	// + destroy path.
	if forwarder != nil {
		forwarder.PauseAndDrain()
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
		result, qerr := guestlink.SendQuiesce(client, !dropCaches)
		if qerr != nil {
			err = qerr
			logf("quiesce: %v (aborting snapshot)", err)
			return ctl.Response{}, fmt.Errorf("quiesce: %w", err)
		}
		dropCachesResult = result
		guestQuiesced = true
		logf("quiesce: guest acked (drop_caches=%s, MUX + forwards closed), proceeding to /vm.pause", result)
	}
	// Record the last guest observation together with one exact CH target/current
	// pair at the freeze boundary. BeginSnapshot still holds the balloon mutation
	// gate, and a successful guest quiesce has already stopped the application;
	// the following snapshot.Take is the first operation allowed to pause CH.
	if memoryController != nil {
		freeze, captureErr := memoryController.CaptureState(context.Background())
		if captureErr != nil {
			logf("snapshot: freeze memory observation unavailable: %v", captureErr)
		} else {
			logf("snapshot: freeze memory MemAvailable=%d Cached=%d BalloonTarget=%d BalloonCurrent=%d TargetBudget=%d CurrentBudget=%d ObservedBudget=%d Reservation=%d report=%d/%d",
				freeze.GuestMemAvailable, freeze.GuestCached, freeze.BalloonTarget,
				freeze.BalloonCurrent, freeze.TargetBudget, freeze.CurrentBudget,
				freeze.ObservedBudget, freeze.Reservation, freeze.ReportEpoch, freeze.ReportSeq)
		}
	}

	// Build snapshot.cfg builder closure. Take() invokes it with the final
	// overlay_ref the sink produced (scheme-qualified file ref in --output mode,
	// manifest://<key> in --upload mode) so snapshot.cfg's overlay.base is
	// final on first write — no post-hoc ZIP rewrite.
	cfg := opts.Cfg
	snapCfgBuilder := func(overlayRefs []string) ([]byte, error) {
		return buildSnapshotCfg(cfg, overlayRefs, resultMemoryRefs, diskMerged)
	}

	// opts.SandboxID is always populated by the run path (generated when the
	// CLI didn't pass one); see Run() in this file.
	sandboxID := opts.SandboxID

	// Pick the sink. --output writes sparse, content-addressed local files;
	// --upload streams to a fresh Ingester (its own store client, never shared
	// with the long-lived read-side fetcher). Either way Take streams the
	// multi-GiB memory + overlay straight to the sink — only CH's tiny
	// config.json/state.json transit the /run tmpfs staging dir.
	// Per-disk diff list (root, then data disks) for Take. Restored from a LOCAL
	// snapshot ⇒ MERGE this run's resident delta onto the parent local layer
	// (replace the next-newest layer, not stack) for BOTH --output and --upload;
	// buildSnapshotCfg drops the parent ref to match. Cold/manifest parents stack.
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
		LocalCodec:    opts.LocalCodec,
		LocalRequired: opts.LocalRequired,
	}
	if mergeMemory {
		src.MergeBaseSnapshot = memoryMergeBase
	}
	out, err := snapshot.Take(src, sink, req.ResumeAfter)
	if err != nil {
		// SendQuiesce closes the old MUX and freezes the application. Take
		// restores the VM/backend state on failure; reattach completes the
		// guest-side recovery and thaws the application before we return.
		if guestQuiesced {
			reattachRunningGuest("failed snapshot")
		}
		return ctl.Response{}, err
	}

	// --resume: Take has resumed CH, but the guest closed the stdio MUX
	// during quiesce. Re-establish it (best-effort — the snapshot itself
	// succeeded; a reattach failure just leaves the resumed sandbox with
	// detached stdio). Do this before any upload work so the app, which is
	// already running again, isn't blocked on a full stdout pipe for long.
	if req.ResumeAfter {
		reattachRunningGuest("snapshot")
	}

	resp = ctl.Response{
		MemorySize:       out.MemorySize,
		MemoryResident:   out.MemoryResident,
		WallclockPauseMs: out.WallclockPauseMs,
		WallclockDumpMs:  out.WallclockDumpMs,
		DropCachesResult: dropCachesResult,
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
// delay elapses the snapshot_done response has been queued + sent. The
// memory.high lifecycle lock and balloon mutation barrier remain held until CH
// exits, including when the shutdown request itself fails. Errors are only
// logged — the sandbox is being torn down regardless.
func destroyAfterSnapshot(
	chSock string,
	memoryHighLock *os.File,
	releaseMemoryBarrier func(),
	chExited <-chan struct{},
	respDeadline time.Duration,
	logf func(string, ...any),
) {
	if releaseMemoryBarrier != nil {
		defer releaseMemoryBarrier()
	}
	if memoryHighLock != nil {
		defer func() {
			if err := memoryHighLock.Close(); err != nil {
				logf("snapshot: destroy mode — release VMM memory.high lifecycle lock: %v", err)
			}
		}()
	}
	time.Sleep(destroyAfterSnapshotDelay)
	if err := (chapi.Client{Sock: chSock, RespDeadline: respDeadline}).ShutdownVMM(); err != nil {
		logf("snapshot: destroy mode — vmm.shutdown: %v", err)
		logf("snapshot: destroy mode — retaining memory barrier until VMM exit")
	} else {
		logf("snapshot: destroy mode — VMM shutdown requested")
	}
	if chExited != nil {
		<-chExited
	}
}

func ensureSnapshotDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("empty path")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".snapshot-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func validateLocalMemoryRefs(outputDir string, refs []string, codec tarstream.Codec, required bool) error {
	for i, raw := range refs {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return fmt.Errorf("snapshot: memory ref[%d] is invalid", i)
		}
		if ref.Portable() {
			continue
		}
		// Local memory dependencies are intentionally portable only as a
		// sibling artifact set. Never retain an absolute/source-directory path
		// in snapshot.cfg; resolve the ref basename against W's output bundle.
		path := filepath.Join(outputDir, filepath.Base(ref.Path))
		info, err := os.Stat(path)
		if err != nil {
			if codec != nil {
				return fmt.Errorf("snapshot: local memory ref[%d] is not accessible", i)
			}
			return fmt.Errorf("snapshot: local memory ref[%d] is not accessible: %w", i, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("snapshot: local memory ref[%d] is not a regular file", i)
		}
		stream, _, err := openLocalDiskStream(path, ref, codec, required)
		if err != nil {
			return fmt.Errorf("snapshot: local memory ref[%d] validation: %w", i, err)
		}
		if err := stream.Close(); err != nil {
			return fmt.Errorf("snapshot: local memory ref[%d] close: %w", i, err)
		}
	}
	return nil
}

func validatePortableMemoryRefs(refs []string) error {
	for i, raw := range refs {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return fmt.Errorf("snapshot: memory ref[%d] is invalid", i)
		}
		if !ref.Portable() {
			return fmt.Errorf("snapshot: direct upload would retain a local memory lower; use --output then upload-snapshot")
		}
	}
	return nil
}

// snapshotMemoryRefs returns the memory lowers retained by the next snapshot.
// A local merge replaces only the direct parent self; the parent's existing
// lowers remain in order and are not recursively compacted.
func snapshotMemoryRefs(prov config.SnapshotProvenance, mergeLocalParent bool) ([]string, error) {
	refs := prependRef(prov.ParentSnapshotRef, prov.ParentFromRefs)
	if mergeLocalParent {
		refs = append([]string(nil), prov.ParentFromRefs...)
	}
	return normalizeLocalMemoryRefs(refs)
}

func normalizeLocalMemoryRefs(refs []string) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]string, len(refs))
	for i, raw := range refs {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return nil, fmt.Errorf("snapshot: memory ref[%d] is invalid", i)
		}
		if !ref.Portable() {
			base := filepath.Base(ref.Path)
			if ref.Path == "" || base == "." || base == ".." || base == string(filepath.Separator) {
				return nil, fmt.Errorf("snapshot: memory ref[%d] has no artifact basename", i)
			}
			ref.Path = base
		}
		out[i] = ref.String()
	}
	return out, nil
}

// buildSnapshotCfg renders the snapshot.cfg YAML body per docs §3.4.
// runtime_ref / base_ref are pre-computed by sandbox-ctl at boot
// from each artifact's declared identity (see config.SnapshotRefs in
// config.SandboxConfig). overlayRef is filled in by Take() after overlay
// digest is known, or by Upload() after overlay manifest key is known.
func buildSnapshotCfg(cfg *config.SandboxConfig, overlayRefs, memoryFromRefs []string, diskMerged []bool) ([]byte, error) {
	if len(overlayRefs) != 1+len(cfg.Boot.Disks) {
		return nil, fmt.Errorf("snapshot.cfg: got %d disk refs, want %d", len(overlayRefs), 1+len(cfg.Boot.Disks))
	}
	if len(diskMerged) != len(overlayRefs) {
		return nil, fmt.Errorf("snapshot.cfg: got %d disk merge decisions, want %d", len(diskMerged), len(overlayRefs))
	}
	doc := snapshotCfgYAML{}
	doc.Resources.Capacity.CPU = cfg.Resources.Capacity.CPU
	doc.Resources.Capacity.Memory = cfg.Resources.Capacity.Memory
	doc.Metadata = cfg.Metadata
	doc.Launch.CgroupControl = cfg.Launch.CgroupControl
	doc.Boot.RuntimeRef = cfg.SnapshotRefs.RuntimeRef

	// Incremental layered chain (docs/sandbox.md §3.5). The lifecycle passes
	// the already-decided and canonicalized memory chain because working-set
	// snapshots may stack local memory while still merging every local disk.
	prov := cfg.SnapshotProvenance
	coldStart := prov.ParentSnapshotRef == ""
	doc.FromRefs = memoryFromRefs

	// Root node. coldLower is the read-only lower the writable diff sits on: the
	// single-disk CoW base, or the overlay's lower (overlay.base) in two-disk
	// mode — both must be chained on cold start (see renderDiskNode).
	rootColdLower := cfg.Boot.Root.Base
	rootColdChain := cfg.Boot.Root.BaseFromRefs
	if cfg.Boot.Root.Overlay != nil {
		rootColdLower = cfg.Boot.Root.Overlay.Base
		rootColdChain = cfg.Boot.Root.Overlay.BaseFromRefs
	}
	doc.Boot.Root = renderDiskNode(overlayRefs[0], cfg.SnapshotRefs.BaseRef,
		rootColdLower, rootColdChain, cfg.SingleDisk(), diskMerged[0], coldStart,
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
		diskColdChain := d.BaseFromRefs
		if d.Overlay != nil {
			diskColdLower = d.Overlay.Base
			diskColdChain = d.Overlay.BaseFromRefs
		}
		doc.Boot.Disks = append(doc.Boot.Disks, renderDiskNode(overlayRefs[1+i],
			baseRef, diskColdLower, diskColdChain, d.RootConfig.Single(), diskMerged[1+i], coldStart, pTop, pChain))
	}
	return yaml.Marshal(&doc)
}

// renderDiskNode renders one disk's snapshot.cfg node (root or a data disk),
// computing its incremental chain. overlayRef is the captured top diff ref;
// baseRef the erofs image ref (overlay mode); coldLower the cold-start read-only
// lower the writable diff sits on — the single-disk CoW base, or the overlay's
// lower (overlay.base) in two-disk mode — to chain. parentTop/parentChain are
// this disk's parent overlay ref + chain — dropped (parentMerged) or
// prepended (stacked).
func renderDiskNode(overlayRef, baseRef, coldLower string, coldChain []string, single, parentMerged, coldStart bool, parentTop string, parentChain []string) diskNodeYAML {
	var chain []string
	if parentMerged {
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
		chain = append([]string{coldLower}, append([]string{}, coldChain...)...)
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
	Launch   struct {
		CgroupControl bool `yaml:"cgroup_control"`
	} `yaml:"launch"`
	Boot struct {
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

// populateSnapshotRefs reads the identities embedded in boot.runtime and local
// tarstream bases, then stores canonical refs on cfg.SnapshotRefs. It performs
// metadata-only reads; manifest refs already carry their content identity.
func populateSnapshotRefs(cfg *config.SandboxConfig, locations config.RefLocations, codec tarstream.Codec, required bool) error {
	if err := canonicalizeConfiguredTarRefs(cfg, locations, codec, required); err != nil {
		return err
	}
	rRef, err := buildRuntimeRef(cfg.Boot.Runtime)
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
			ref, err := buildDiskRef(d.Base, locations, codec, required)
			if err != nil {
				return fmt.Errorf("boot.disks[%d].base: %w", i, err)
			}
			cfg.SnapshotRefs.DiskBaseRefs[i] = ref
		}
	}

	if cfg.Boot.Root.Base == "" {
		return nil
	}
	bRef, err := buildDiskRef(cfg.Boot.Root.Base, locations, codec, required)
	if err != nil {
		return fmt.Errorf("boot.root.base: %w", err)
	}
	cfg.SnapshotRefs.BaseRef = bRef
	return nil
}

// canonicalizeConfiguredTarRefs replaces every local immutable disk ref in the
// live config with the actual policy-normalized identity returned by its
// artifact. Paths and named locations are preserved for later opens. This
// keeps cold-start lower chains from copying legacy sha256 qualifiers into a
// newly generated key-bound snapshot.cfg.
func canonicalizeConfiguredTarRefs(cfg *config.SandboxConfig, locations config.RefLocations, codec tarstream.Codec, required bool) error {
	if cfg == nil {
		return fmt.Errorf("sandbox config is nil")
	}
	normalize := func(field string, target *string) error {
		if target == nil || *target == "" {
			return nil
		}
		ref, err := canonicalConfiguredTarRef(*target, locations, codec, required)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		*target = ref
		return nil
	}
	normalizeList := func(field string, refs []string) error {
		for i := range refs {
			if err := normalize(fmt.Sprintf("%s[%d]", field, i), &refs[i]); err != nil {
				return err
			}
		}
		return nil
	}
	normalizeRoot := func(field string, root *config.RootConfig) error {
		if err := normalize(field+".base", &root.Base); err != nil {
			return err
		}
		if err := normalizeList(field+".base_from_refs", root.BaseFromRefs); err != nil {
			return err
		}
		if root.Overlay == nil {
			return nil
		}
		if err := normalize(field+".overlay.base", &root.Overlay.Base); err != nil {
			return err
		}
		return normalizeList(field+".overlay.base_from_refs", root.Overlay.BaseFromRefs)
	}
	if err := normalizeRoot("boot.root", &cfg.Boot.Root); err != nil {
		return err
	}
	for i := range cfg.Boot.Disks {
		if err := normalizeRoot(fmt.Sprintf("boot.disks[%d]", i), &cfg.Boot.Disks[i].RootConfig); err != nil {
			return err
		}
	}
	return nil
}

func canonicalConfiguredTarRef(raw string, locations config.RefLocations, codec tarstream.Codec, required bool) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", protectLocalArtifactError(codec, "parse local artifact ref", err)
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		return ref.String(), nil
	}
	stream, _, err := OpenDiskStream(context.Background(), raw, nil, locations, codec, required)
	if err != nil {
		return "", err
	}
	digester, ok := stream.(tarstream.Digester)
	if !ok {
		_ = stream.Close()
		return "", fmt.Errorf("local artifact has no declared digest")
	}
	scheme, digest := digester.Digest()
	if err := stream.Close(); err != nil {
		return "", err
	}
	ref.DigestScheme, ref.Digest = scheme, digest
	if err := ref.Validate(); err != nil {
		return "", fmt.Errorf("invalid canonical local artifact ref")
	}
	return ref.String(), nil
}

// buildRuntimeRef produces the canonical snapshot ref for the PMEM runtime
// bundle. boot.runtime is file-only by configuration contract.
func buildRuntimeRef(uri string) (string, error) {
	if !strings.HasPrefix(uri, "file://") {
		return "", fmt.Errorf("expected file://, got %q", uri)
	}
	path := strings.TrimPrefix(uri, "file://")
	info, err := runtimebundle.Inspect(path)
	if err != nil {
		return "", err
	}
	return "file://" + filepath.Base(path) + "@" + info.Digest, nil
}

// buildDiskRef passes manifest identities through and reads a local tarstream
// artifact's declared identity without scanning its payload.
func buildDiskRef(uri string, locations config.RefLocations, codec tarstream.Codec, required bool) (string, error) {
	ref, err := manifest.ParseRef(uri)
	if err != nil {
		return "", err
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		return ref.String(), nil
	}
	stream, _, err := OpenDiskStream(context.Background(), uri, nil, locations, codec, required)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	digester, ok := stream.(tarstream.Digester)
	if !ok {
		return "", fmt.Errorf("file artifact has no declared digest")
	}
	scheme, digest := digester.Digest()
	ref.Path = filepath.Base(ref.Path)
	ref.DigestScheme = scheme
	ref.Digest = digest
	if err := ref.Validate(); err != nil {
		return "", fmt.Errorf("file artifact ref: %w", err)
	}
	return ref.String(), nil
}

// resolvedLocalMergeRef binds an already-resolved local path to the canonical
// identity carried in snapshot provenance. Merge preflight and Take both open
// this exact ref, avoiding basename inference and TOCTOU loss of the expected
// digest between the two stages.
func resolvedLocalMergeRef(path, raw string) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil || ref.Scheme != manifest.RefSchemeFile || ref.Portable() || ref.Digest == "" {
		return "", fmt.Errorf("scheme-qualified local file identity required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve local merge path: %w", err)
	}
	ref.Path = abs
	ref.Location = ""
	if err := ref.Validate(); err != nil {
		return "", fmt.Errorf("invalid local merge identity")
	}
	return ref.String(), nil
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

package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/runtimebundle"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/chapi"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
	"github.com/kuasar-sandbox/sandboxer/pkg/tapfd"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
	"golang.org/x/sys/unix"
)

// RunOptions controls a single sandbox-ctl run invocation.
type RunOptions struct {
	Cfg *config.SandboxConfig
	// PortableConfig is immutable C0. nil selects projection from an explicit
	// cold config; non-nil selects a Sandbox source and requires SourceBinding.
	PortableConfig *config.PortableSandboxConfig
	SourceBinding  *RunSourceBinding
	MemoryBinding  *MemorySourceBinding
	ManifestCfg    *config.ManifestConfig // for manifest:// resolution; may be nil if all file://
	Fetcher        fetch.Fetcher          // lazily network-backed; caller owns its lifetime
	BundleReader   *manifestbundle.Reader // non-nil while restoring a local root Bundle
	BundleFetcher  *manifestbundle.ManifestFetcher
	RefLocations   config.RefLocations    // trusted logical location -> host directory mappings
	SandboxID      string                 // generated if empty
	CHBinary       string                 // path to bin/cloud-hypervisor
	RuntimeRoot    string                 // tmpfs run root (sockets / snap staging); "/run/sandbox" by default
	BaseRoot       string                 // on-disk base root (overlay diff); "/var/lib/sandbox" by default
	StatsJSONPath  string                 // if set, write vhost stats as JSON to this path on shutdown
	StatsInterval  time.Duration          // if > 0, periodically log lazy-load stats; 0 = off
	StdioMode      stdio.Mode             // CH process stdio wiring; see pkg/stdio
	CustomerKeyFn  ingest.CustomerKeyFunc // process-fixed customer key resolver
	LocalCodec     tarstream.Codec        // nil when crypto.local=off
	LocalRequired  bool                   // reject plaintext local artifacts and active diffs

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

// RunSourceBinding is the non-serializable provenance needed to resolve the
// reserved self value and later materialize it when export builds C1. It never
// enters sandbox.runtime.cfg.
type RunSourceBinding struct {
	SandboxRef   string // canonical identity of the current E
	RuntimeRef   string // host-resolvable ref for that E
	RelativeDir  string // trusted directory for unlocated basename dependencies
	BundleSource *BundleSourceBinding
}

// MemorySourceBinding is restore-only, non-serializable lookup/provenance for
// the current S. Disk state is intentionally absent: it belongs to C0/E.
type MemorySourceBinding struct {
	SnapshotRef  string
	RuntimeRef   string
	RelativeDir  string
	FromRefs     []string
	BundleSource *BundleSourceBinding
}

// BundleSourceBinding keeps physical Bundle lookup separate from logical
// manifest:// references and never enters sandbox.runtime.cfg or snapshot.cfg.
type BundleSourceBinding struct {
	RootRef  string
	RootPath string
	Refs     []string
	// Reader and Fetcher are a paired, runtime-only selector owned by the
	// stream that opened RootPath. Keeping them on each binding preserves
	// independent Sandbox E and Snapshot S provenance after restore.
	Reader  *manifestbundle.Reader
	Fetcher *manifestbundle.ManifestFetcher
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
	if err := validateSandboxID(opts.SandboxID); err != nil {
		return -1, err
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
	fileOpener := FileStreamOpener(func(ctx context.Context, path string, ref manifest.Ref) (fetch.Stream, error) {
		opened, err := artifact.OpenFileWithLocations(ctx, path, ref, opts.ManifestCfg, opts.CustomerKeyFn,
			opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return nil, err
		}
		return opened, nil
	})
	var c0 *config.PortableSandboxConfig
	if opts.PortableConfig != nil {
		if opts.SourceBinding == nil {
			return -1, errors.New("portable run requires RunSourceBinding")
		}
		if _, err := manifest.ParseRef(opts.SourceBinding.SandboxRef); err != nil {
			return -1, fmt.Errorf("Sandbox source identity: %w", err)
		}
		var err error
		c0, err = opts.PortableConfig.Clone()
		if err != nil {
			return -1, err
		}
		if err := config.BindPortableDiskGraph(opts.Cfg, opts.SourceBinding.RuntimeRef, opts.SourceBinding.RelativeDir); err != nil {
			return -1, fmt.Errorf("Sandbox source binding: %w", err)
		}
	}
	if err := opts.Cfg.ValidateCold(); err != nil {
		return -1, fmt.Errorf("config: %w", err)
	}
	if err := canonicalizeConfiguredTarRefsWithOpener(ctx, opts.Cfg, opts.RefLocations, opts.LocalCodec, opts.LocalRequired, fileOpener); err != nil {
		return -1, fmt.Errorf("portable disk refs: %w", err)
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
	imageDefaults, err := preflightColdArtifacts(ctx, opts.Cfg, opts.Fetcher, opts.RefLocations,
		opts.BaseRoot, opts.SandboxID, opts.LocalCodec, diffCustomerKey, opts.LocalRequired, fileOpener)
	if err != nil {
		return -1, err
	}
	if c0 == nil {
		if err := MaterializeImageDefaults(opts.Cfg, imageDefaults); err != nil {
			return -1, fmt.Errorf("materialize image defaults: %w", err)
		}
		if err := opts.Cfg.ValidateCold(); err != nil {
			return -1, fmt.Errorf("materialized config: %w", err)
		}
		// The identity projection is only needed to record the digest into
		// the portable config (the format requires a content identity).
		// Resuming from an existing portable config deliberately skips the
		// re-hash (issue #158): it costs a fixed ~60ms per boot for a check
		// that centrally-managed deployments do not need.
		identities, err := ResolvePortableProjection(opts.Cfg)
		if err != nil {
			return -1, err
		}
		c0, err = config.ProjectPortableCold(opts.Cfg, identities)
		if err != nil {
			return -1, fmt.Errorf("project portable C0: %w", err)
		}
	} else {
		// Only the runtime bundle identity is re-checked here: its digest
		// marker lives in the ZIP footer and is read without scanning the
		// artifact. The kernel gets the cheap artifact preflight only; the
		// full SHA-256 re-hash is skipped (issue #158).
		if err := VerifyKernelArtifact(opts.Cfg.Boot.Kernel); err != nil {
			return -1, fmt.Errorf("boot.kernel identity: %w", err)
		}
		runtimeRef, err := ResolveRuntimeProjection(opts.Cfg)
		if err != nil {
			return -1, err
		}
		if c0.Boot.Runtime != runtimeRef {
			return -1, fmt.Errorf("boot.runtime identity mismatch: portable %s, host %s", c0.Boot.Runtime, runtimeRef)
		}
	}

	runDir := filepath.Join(opts.RuntimeRoot, opts.SandboxID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return -1, fmt.Errorf("mkdir %s: %w", runDir, err)
	}
	defer os.RemoveAll(runDir)
	if _, err := config.WritePortableSandboxConfig(runDir, c0); err != nil {
		return -1, fmt.Errorf("write immutable C0: %w", err)
	}

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
		r, _, err := OpenRootImageBlockReaderWithOpener(ctx, opts.Cfg.Boot.Root.Base, fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired, fileOpener)
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
			r, _, err := OpenLayeredBlockReaderWithOpener(ctx, refs, fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired, fileOpener)
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
			r, _, err := OpenLayeredBlockReaderWithOpener(ctx, refs, fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired, fileOpener)
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
		db, dcleanup, derr := prepColdDataDisk(ctx, &opts.Cfg.Boot.Disks[i], i, opts.BaseRoot, opts.SandboxID, fetcher, opts.RefLocations, opts.LocalCodec, diffCustomerKey, opts.LocalRequired, fileOpener)
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
	params := VMParams{
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
		PortableConfig:    c0,
		SourceBinding:     opts.SourceBinding,
		MemoryBinding:     opts.MemoryBinding,
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
	}
	applyRunCaptureSources(&params, opts)
	return ServeAndWait(params)
}

// applyRunCaptureSources carries the logical and physical source resolvers into
// the live capture handler. In particular, a cold run opened from a local
// Manifest Bundle must retain its selector so later export/snapshot merge plans
// can identify and copy the exact parent Manifests instead of growing the layer
// graph as if every Bundle ref were remote.
func applyRunCaptureSources(params *VMParams, opts RunOptions) {
	params.Fetcher = opts.Fetcher
	params.BundleReader = opts.BundleReader
	params.BundleFetcher = opts.BundleFetcher
	params.RefLocations = opts.RefLocations
}

// ResolvePortableProjection verifies host kernel/runtime artifacts and returns
// their basename identities for C0. Offline export shares this preflight.
func ResolvePortableProjection(cfg *config.SandboxConfig) (config.PortableProjection, error) {
	kernelRef, err := buildKernelRef(cfg.Boot.Kernel)
	if err != nil {
		return config.PortableProjection{}, fmt.Errorf("boot.kernel identity: %w", err)
	}
	runtimeRef, err := buildRuntimeRef(cfg.Boot.Runtime)
	if err != nil {
		return config.PortableProjection{}, fmt.Errorf("boot.runtime identity: %w", err)
	}
	return config.PortableProjection{KernelRef: kernelRef, RuntimeRef: runtimeRef}, nil
}

// ResolveRuntimeProjection verifies the host runtime bundle identity only.
// It reads the digest marker at the bundle's ZIP footer and never scans the
// artifact, so it stays on the restore and portable-config boot paths where
// the full projection (whose kernel SHA-256 costs a fixed ~60ms) is skipped
// per issue #158.
func ResolveRuntimeProjection(cfg *config.SandboxConfig) (string, error) {
	runtimeRef, err := buildRuntimeRef(cfg.Boot.Runtime)
	if err != nil {
		return "", fmt.Errorf("boot.runtime identity: %w", err)
	}
	return runtimeRef, nil
}

// VerifyKernelArtifact performs the cheap kernel preflight — the binding
// shape, that the file exists and is a regular file — without the full
// SHA-256 scan. It preserves buildKernelRef's fail-fast behavior on the
// restore and portable-config boot paths, where the kernel re-hash is
// skipped per issue #158.
func VerifyKernelArtifact(uri string) error {
	ref, err := manifest.ParseRef(uri)
	if err != nil || ref.Scheme != manifest.RefSchemeFile || ref.Location != "" || !filepath.IsAbs(ref.Path) {
		return fmt.Errorf("expected absolute unlocated file:// binding")
	}
	if ref.Digest != "" {
		return errors.New("host kernel binding must be a path without an artifact identity")
	}
	f, err := os.Open(ref.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("kernel is not a regular file")
	}
	return nil
}

// PrepareOfflinePortableConfig validates and canonicalizes an explicit config
// for offline flattened-EROFS export, then replaces its root graph with the
// direct EROFS self layout.
func PrepareOfflinePortableConfig(ctx context.Context, cfg *config.SandboxConfig, locations config.RefLocations, codec tarstream.Codec, required bool, opener FileStreamOpener) (*config.PortableSandboxConfig, error) {
	if cfg == nil {
		return nil, errors.New("offline export config is nil")
	}
	if err := cfg.ValidateCold(); err != nil {
		return nil, err
	}
	if err := canonicalizeConfiguredTarRefsWithOpener(ctx, cfg, locations, codec, required, opener); err != nil {
		return nil, err
	}
	identities, err := ResolvePortableProjection(cfg)
	if err != nil {
		return nil, err
	}
	return config.NewPortableEROFS(cfg, identities)
}

func buildKernelRef(uri string) (string, error) {
	ref, err := manifest.ParseRef(uri)
	if err != nil || ref.Scheme != manifest.RefSchemeFile || ref.Location != "" || !filepath.IsAbs(ref.Path) {
		return "", fmt.Errorf("expected absolute unlocated file:// binding")
	}
	if ref.Digest != "" {
		return "", errors.New("host kernel binding must be a path without an artifact identity")
	}
	f, err := os.Open(ref.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("kernel is not a regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return "file://" + filepath.Base(ref.Path) + "@digest:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func preflightColdArtifacts(
	ctx context.Context,
	cfg *config.SandboxConfig,
	fetcher fetch.Fetcher,
	locations config.RefLocations,
	baseRoot, sandboxID string,
	codec tarstream.Codec,
	diffCustomerKey [32]byte,
	required bool,
	opener FileStreamOpener,
) (*ImageConfig, error) {
	imageDefaults := &ImageConfig{}
	root := &cfg.Boot.Root
	if root.Base != "" && !cfg.SingleDisk() {
		reader, _, err := OpenRootImageBlockReaderWithOpener(ctx, root.Base, fetcher, locations, codec, required, opener)
		if err != nil {
			return nil, fmt.Errorf("preflight boot.root.base: %w", err)
		}
		imageCfg, imageErr := LoadImageConfigFrom(reader, reader.Size())
		if imageErr == nil {
			_, imageErr = MergeLaunch(imageCfg, cfg.Launch)
		}
		closeErr := reader.Close()
		if imageErr != nil {
			return nil, errors.Join(fmt.Errorf("preflight root image/launch: %w", imageErr), closeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("preflight boot.root.base close: %w", closeErr)
		}
		imageDefaults = imageCfg
	}
	if err := preflightWritableExt4(ctx, root, "boot.root", DefaultDiffURI(baseRoot, sandboxID),
		fetcher, locations, codec, diffCustomerKey, required, opener); err != nil {
		return nil, err
	}
	for i := range cfg.Boot.Disks {
		disk := &cfg.Boot.Disks[i].RootConfig
		if !disk.Single() {
			reader, _, err := OpenRootImageBlockReaderWithOpener(ctx, disk.Base, fetcher, locations, codec, required, opener)
			if err != nil {
				return nil, fmt.Errorf("preflight boot.disks[%d].base: %w", i, err)
			}
			if err := reader.Close(); err != nil {
				return nil, fmt.Errorf("preflight boot.disks[%d].base close: %w", i, err)
			}
		}
		defaultDiff := "file://" + filepath.Join(DefaultBaseDir(baseRoot, sandboxID), fmt.Sprintf("%s.disk%d.diff", sandboxID, i))
		if err := preflightWritableExt4(ctx, disk, fmt.Sprintf("boot.disks[%d]", i), defaultDiff,
			fetcher, locations, codec, diffCustomerKey, required, opener); err != nil {
			return nil, err
		}
	}
	if cfg.SingleDisk() {
		if _, err := MergeLaunch(&ImageConfig{}, cfg.Launch); err != nil {
			return nil, fmt.Errorf("preflight launch: %w", err)
		}
	}
	return imageDefaults, nil
}

const ext4SuperblockMagicOffset = int64(1024 + 0x38)

func preflightWritableExt4(
	ctx context.Context,
	root *config.RootConfig,
	field, defaultDiffURI string,
	fetcher fetch.Fetcher,
	locations config.RefLocations,
	codec tarstream.Codec,
	diffCustomerKey [32]byte,
	required bool,
	opener FileStreamOpener,
) (retErr error) {
	var baseRefs []string
	diffURI, templateURI := root.Diff, root.DiffTemplate
	if root.Single() {
		if root.Base != "" {
			baseRefs = append([]string{root.Base}, root.BaseFromRefs...)
		}
	} else {
		diffURI, templateURI = root.Overlay.Diff, root.Overlay.DiffTemplate
		if root.Overlay.Base != "" {
			baseRefs = append([]string{root.Overlay.Base}, root.Overlay.BaseFromRefs...)
		}
	}

	var base vhost.BlockReader
	if len(baseRefs) != 0 {
		reader, _, err := OpenLayeredBlockReaderWithOpener(ctx, baseRefs, fetcher, locations, codec, required, opener)
		if err != nil {
			return fmt.Errorf("preflight %s immutable ext4 layers: %w", field, err)
		}
		base = reader
		defer func() {
			retErr = errors.Join(retErr, base.Close())
		}()
	}

	if diffURI == "" {
		diffURI = defaultDiffURI
	}
	scheme, diffPath, ok := config.SchemeAndPath(diffURI)
	if !ok || scheme != "file" {
		return fmt.Errorf("preflight %s writable diff invalid URI: %s", field, diffURI)
	}
	var cowOptions []vhost.BlockCOWOption
	if codec != nil {
		cowOptions = append(cowOptions, vhost.WithDiffEncryption(diffCustomerKey, required))
	}
	info, statErr := os.Stat(diffPath)
	switch {
	case statErr == nil:
		if info.Size() == 0 {
			return fmt.Errorf("preflight %s writable diff %s is empty; provide a formatted ext4 diff", field, diffPath)
		}
		if err := vhost.ValidateExistingDiffExt4(ctx, diffPath, base, cowOptions...); err != nil {
			return fmt.Errorf("preflight %s writable diff: %w", field, err)
		}
		return nil
	case !os.IsNotExist(statErr):
		return fmt.Errorf("preflight %s writable diff stat: %w", field, statErr)
	case templateURI != "":
		templateScheme, templatePath, templateOK := config.SchemeAndPath(templateURI)
		if !templateOK || templateScheme != "file" {
			return fmt.Errorf("preflight %s diff_template invalid URI: %s", field, templateURI)
		}
		if err := vhost.ValidateDiffTemplateExt4(ctx, templatePath, base, cowOptions...); err != nil {
			return fmt.Errorf("preflight %s diff_template: %w", field, err)
		}
		return nil
	case base != nil:
		if err := validateExt4BlockReader(ctx, base); err != nil {
			return fmt.Errorf("preflight %s immutable ext4 layers: %w", field, err)
		}
		return nil
	default:
		return fmt.Errorf("preflight %s has no formatted ext4 source", field)
	}
}

func validateExt4BlockReader(ctx context.Context, reader vhost.BlockReader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader.Size() < ext4SuperblockMagicOffset+2 {
		return errors.New("effective writable disk is too small for an ext4 superblock")
	}
	var magic [2]byte
	n, err := reader.ReadAt(magic[:], ext4SuperblockMagicOffset)
	if err != nil && !(errors.Is(err, io.EOF) && n == len(magic)) {
		return fmt.Errorf("read ext4 superblock magic: %w", err)
	}
	if n != len(magic) {
		return io.ErrUnexpectedEOF
	}
	if magic != [2]byte{0x53, 0xef} {
		return errors.New("effective writable disk is not a formatted ext4 filesystem")
	}
	return ctx.Err()
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
func prepColdDataDisk(ctx context.Context, d *config.DiskConfig, ordinal int, baseRoot, sandboxID string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, diffCustomerKey [32]byte, required bool, opener FileStreamOpener) (DiskBackend, func(), error) {
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
		r, _, err := OpenRootImageBlockReaderWithOpener(ctx, d.Base, fetcher, locations, codec, required, opener)
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
		r, _, err := OpenLayeredBlockReaderWithOpener(ctx, refs, fetcher, locations, codec, required, opener)
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
	Cfg            *config.SandboxConfig
	PortableConfig *config.PortableSandboxConfig
	SourceBinding  *RunSourceBinding
	MemoryBinding  *MemorySourceBinding
	ManifestCfg    *config.ManifestConfig // required for --upload; the Ingester is built lazily per snapshot
	Fetcher        fetch.Fetcher
	BundleReader   *manifestbundle.Reader
	BundleFetcher  *manifestbundle.ManifestFetcher
	RefLocations   config.RefLocations
	CustomerKeyFn  ingest.CustomerKeyFunc
	LocalCodec     tarstream.Codec
	LocalRequired  bool
	SandboxID      string // required: snapshot.Take rejects empty
	Memfd          *memory.Memfd
	Disks          []SnapDiskRef   // writable diffs to capture, logical order (root, then data disks)
	Servers        []*vhost.Server // all vhost servers (quiesced together around the dump)
	CHSock         string
	RunDir         string
	Pinger         *guestlink.Pinger        // optional; if non-nil, paused around quiesce/Take
	Forwarder      *Forwarder               // optional; if non-nil, paused + active relays collapsed around quiesce
	Reattach       func() error             // optional; re-establishes the stdio MUX whenever a snapshot attempt resumes
	Memory         *resctl.MemoryController // optional; blocks local Budget mutation across capture
	CHProcess      func() processSignaler   // late-bound after CH starts; used to guarantee destroy completion
	Context        context.Context
	Logf           func(string, ...any)

	capture captureGate
}

// captureGate serializes export and snapshot for one live VM. A successful
// non-resume capture is terminal because the VMM shutdown is deliberately
// scheduled after the ctl response can be flushed; no second request may enter
// that interval.
type captureGate struct {
	mu       sync.Mutex
	active   bool
	terminal bool
}

type terminalCaptureError struct{ err error }

func (e *terminalCaptureError) Error() string { return e.err.Error() }
func (e *terminalCaptureError) Unwrap() error { return e.err }

func isTerminalCaptureError(err error) bool {
	var terminal *terminalCaptureError
	return errors.As(err, &terminal)
}

func (g *captureGate) begin() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.terminal {
		return errors.New("sandbox capture already committed and shutdown is in progress")
	}
	if g.active {
		return errors.New("sandbox capture already in progress")
	}
	g.active = true
	return nil
}

func (g *captureGate) finish(terminal bool) {
	g.mu.Lock()
	g.active = false
	g.terminal = g.terminal || terminal
	g.mu.Unlock()
}

// Handle dispatches one ctl snapshot_request. Public for restore.Run.
func (h *SnapshotHandler) Handle(req ctl.Request) (ctl.Response, error) {
	return h.handle(req, "", nil)
}

func (h *SnapshotHandler) handle(req ctl.Request, cgroupPath string, chExited <-chan struct{}) (resp ctl.Response, err error) {
	if err := h.capture.begin(); err != nil {
		return ctl.Response{}, err
	}
	defer func() { h.capture.finish((err == nil && !req.ResumeAfter) || isTerminalCaptureError(err)) }()
	opts := RunOptions{
		Cfg: h.Cfg, PortableConfig: h.PortableConfig, SourceBinding: h.SourceBinding, MemoryBinding: h.MemoryBinding,
		ManifestCfg: h.ManifestCfg, SandboxID: h.SandboxID,
		Fetcher: h.Fetcher, BundleReader: h.BundleReader, BundleFetcher: h.BundleFetcher, RefLocations: h.RefLocations,
		CustomerKeyFn: h.CustomerKeyFn, LocalCodec: h.LocalCodec, LocalRequired: h.LocalRequired,
	}
	var chProcess processSignaler
	if h.CHProcess != nil {
		chProcess = h.CHProcess()
		if chProcess == nil {
			return ctl.Response{}, errors.New("snapshot: Cloud Hypervisor process is not started")
		}
	}
	return handleSnapshotRequest(h.Context, req, opts, h.Memfd, h.Disks, h.Servers, h.CHSock, h.RunDir, cgroupPath, chExited, chProcess, h.Pinger, h.Forwarder, h.Reattach, h.Memory, h.Logf)
}

func (h *SnapshotHandler) HandleExport(req ctl.Request) (ctl.Response, error) {
	return h.handleExport(req, "", nil)
}

func (h *SnapshotHandler) handleExport(req ctl.Request, cgroupPath string, chExited <-chan struct{}) (resp ctl.Response, err error) {
	if err := h.capture.begin(); err != nil {
		return ctl.Response{}, err
	}
	defer func() { h.capture.finish((err == nil && !req.ResumeAfter) || isTerminalCaptureError(err)) }()
	opts := RunOptions{
		Cfg: h.Cfg, PortableConfig: h.PortableConfig, SourceBinding: h.SourceBinding, MemoryBinding: h.MemoryBinding,
		ManifestCfg: h.ManifestCfg, SandboxID: h.SandboxID,
		Fetcher: h.Fetcher, BundleReader: h.BundleReader, BundleFetcher: h.BundleFetcher, RefLocations: h.RefLocations,
		CustomerKeyFn: h.CustomerKeyFn, LocalCodec: h.LocalCodec, LocalRequired: h.LocalRequired,
	}
	var chProcess processSignaler
	if h.CHProcess != nil {
		chProcess = h.CHProcess()
		if chProcess == nil {
			return ctl.Response{}, errors.New("export: Cloud Hypervisor process is not started")
		}
	}
	return handleExportRequest(h.Context, req, opts, h.Disks, h.Servers, h.CHSock, h.RunDir,
		cgroupPath, chExited, chProcess, h.Pinger, h.Forwarder, h.Reattach, h.Memory, h.Logf)
}

func handleExportRequest(
	ctx context.Context,
	req ctl.Request,
	opts RunOptions,
	disks []SnapDiskRef,
	servers []*vhost.Server,
	chSock, runDir, cgroupPath string,
	chExited <-chan struct{},
	chProcess processSignaler,
	pinger *guestlink.Pinger,
	forwarder *Forwarder,
	reattachMUX func() error,
	memoryController *resctl.MemoryController,
	logf func(string, ...any),
) (resp ctl.Response, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	mode, err := req.SnapshotMode()
	if err != nil {
		return ctl.Response{}, err
	}
	if req.DropCaches != nil {
		return ctl.Response{}, errors.New("export does not accept drop_caches")
	}
	if req.MergeRef != nil {
		return ctl.Response{}, errors.New("export does not accept memory merge options")
	}
	if err := validateSandboxID(opts.SandboxID); err != nil {
		return ctl.Response{}, fmt.Errorf("export: %w", err)
	}
	if opts.PortableConfig == nil {
		return ctl.Response{}, errors.New("export: live Sandbox C0/source binding is unavailable")
	}
	if err := opts.PortableConfig.Validate(); err != nil {
		return ctl.Response{}, fmt.Errorf("export: immutable C0: %w", err)
	}
	if opts.Cfg == nil {
		return ctl.Response{}, errors.New("export: runtime host configuration is unavailable")
	}
	if len(disks) != 1+len(opts.PortableConfig.Boot.Disks) {
		return ctl.Response{}, fmt.Errorf("export: runtime disk count %d does not match C0 disk count %d", len(disks), 1+len(opts.PortableConfig.Boot.Disks))
	}
	if req.Upload && req.OutDir != "" {
		return ctl.Response{}, errors.New("--output and --upload are mutually exclusive")
	}
	if !req.Upload && req.OutDir == "" {
		return ctl.Response{}, errors.New("--output and --upload are mutually exclusive; one is required")
	}
	if req.Upload && req.Mode != "" {
		return ctl.Response{}, errors.New("--upload and explicit --mode are mutually exclusive")
	}
	if req.Upload && (opts.ManifestCfg == nil || opts.ManifestCfg.Store.Endpoint == "") {
		return ctl.Response{}, errors.New("export upload requires manifest store configuration")
	}
	if mode == ctl.SnapshotModeBundle && opts.ManifestCfg == nil {
		return ctl.Response{}, errors.New("export Bundle mode requires manifest configuration")
	}
	if strings.TrimSpace(chSock) == "" {
		return ctl.Response{}, errors.New("export: Cloud Hypervisor API socket is unavailable")
	}
	if strings.TrimSpace(runDir) == "" {
		return ctl.Response{}, errors.New("export: run directory is unavailable")
	}
	if reattachMUX == nil && (req.ResumeAfter || chProcess == nil) {
		return ctl.Response{}, errors.New("export: stdio MUX re-attach is unavailable and capture recovery cannot be guaranteed")
	}
	if !req.Upload {
		if err := ensureSnapshotDir(req.OutDir); err != nil {
			return ctl.Response{}, fmt.Errorf("export output directory: %w", err)
		}
	}
	for i := range disks {
		if disks[i].SnapshotView == nil || disks[i].Size <= 0 {
			return ctl.Response{}, fmt.Errorf("export: disk %d has invalid SnapshotView/size", i)
		}
	}

	mergeBaseOpener := newDiskMergeBaseOpener(opts)
	diffs := make([]snapshot.DiskDiff, len(disks))
	diskMerged := make([]bool, len(disks))
	for i, disk := range disks {
		diff := snapshot.DiskDiff{Path: disk.DiffPath, Owned: disk.OwnedDiff, SnapshotView: disk.SnapshotView}
		parentRef, parentPath, parentErr := currentDiskParentBinding(opts, i)
		if parentErr != nil {
			return ctl.Response{}, fmt.Errorf("export: disk %d parent binding: %w", i, parentErr)
		}
		if parentRef != "" {
			parsed, parseErr := manifest.ParseRef(parentRef)
			if parseErr != nil {
				return ctl.Response{}, fmt.Errorf("export: disk %d parent ref: %w", i, parseErr)
			}
			switch {
			case parsed.Scheme == manifest.RefSchemeFile && parsed.DigestScheme != "manifest":
				diff.MergeBase, err = resolvedLocalMergeRef(parentPath, parentRef)
				diskMerged[i] = err == nil
			case parsed.Scheme == manifest.RefSchemeManifest && activeBundleSource(opts) != nil:
				diff.MergeBase, diskMerged[i], err = bundleManifestMergeRef(ctx, parentRef, opts)
			}
			if err != nil {
				return ctl.Response{}, fmt.Errorf("export: disk %d merge base identity: %w", i, err)
			}
		}
		if diskMerged[i] {
			if err := snapshot.ValidateMergeBaseWithOpener(ctx, diff.MergeBase, disk.Size, opts.LocalCodec, opts.LocalRequired, mergeBaseOpener); err != nil {
				return ctl.Response{}, fmt.Errorf("export: disk %d merge base: %w", i, err)
			}
		} else {
			diff.MergeBase = ""
		}
		diffs[i] = diff
	}
	prospectiveParentRef := ""
	if opts.SourceBinding != nil {
		prospectiveParentRef = opts.SourceBinding.SandboxRef
	}
	if err := snapshot.ValidateExportGraph(opts.PortableConfig, prospectiveParentRef, diskMerged); err != nil {
		return ctl.Response{}, fmt.Errorf("export: prospective C1: %w", err)
	}

	var (
		sink                  snapshot.ArtifactSink
		bundleSink            *snapshot.BundleSink
		dependencyPlan        *snapshotBundlePlan
		bundleRefReplacements map[string]string
		sinkOpen              bool
		dependencyPlanOpen    bool
	)
	defer func() {
		if sinkOpen {
			err = errors.Join(err, sink.Close())
		}
	}()
	defer func() {
		if dependencyPlanOpen {
			err = errors.Join(err, dependencyPlan.Close())
		}
	}()
	if mode == ctl.SnapshotModeBundle {
		admission, admissionErr := opts.ManifestCfg.WriteAdmission(ctx)
		if admissionErr != nil {
			return ctl.Response{}, fmt.Errorf("export Bundle admission: %w", admissionErr)
		}
		plan, planErr := prepareSnapshotBundlePlan(ctx, opts, nil, diskMerged, req.OutDir, admission)
		if planErr != nil {
			return ctl.Response{}, planErr
		}
		dependencyPlan = plan
		dependencyPlanOpen = true
		bundleSink, err = snapshot.NewPlannedBundleSink(req.OutDir, opts.SandboxID,
			opts.ManifestCfg, opts.CustomerKeyFn, admission, plan.Refs(), logf)
		if err != nil {
			return ctl.Response{}, err
		}
		sink = bundleSink
		sinkOpen = true
	} else {
		if req.Upload {
			if opts.CustomerKeyFn == nil {
				return ctl.Response{}, errors.New("export ingester customer key resolver is unavailable")
			}
			if _, err := opts.CustomerKeyFn(); err != nil {
				return ctl.Response{}, fmt.Errorf("export ingester customer key: %w", err)
			}
			ing, ingestErr := opts.ManifestCfg.NewIngester(opts.CustomerKeyFn, nil)
			if ingestErr != nil {
				return ctl.Response{}, fmt.Errorf("export ingester: %w", ingestErr)
			}
			sink = snapshot.NewIngestSink(ing, logf)
		} else {
			sink = snapshot.NewFileSink(req.OutDir, opts.SandboxID, opts.LocalCodec, opts.LocalRequired, logf)
		}
		sinkOpen = true
		dependencyPlan, err = prepareSnapshotDependencyPlan(ctx, opts, nil, diskMerged, req.OutDir, store.WriteAdmission{}, false)
		if err != nil {
			return ctl.Response{}, err
		}
		dependencyPlanOpen = true
	}
	if bundleSink != nil {
		bundleRefReplacements, err = dependencyPlan.Ingest(ctx, bundleSink, logf)
	} else {
		bundleRefReplacements, err = dependencyPlan.Emit(ctx, sink, logf)
	}
	planCloseErr := dependencyPlan.Close()
	dependencyPlanOpen = false
	if err != nil || planCloseErr != nil {
		return ctl.Response{}, fmt.Errorf("export dependencies: %w", errors.Join(err, planCloseErr))
	}
	parentRef := ""
	if opts.SourceBinding != nil {
		parentRef = opts.SourceBinding.SandboxRef
	}
	captureC0 := opts.PortableConfig
	if len(bundleRefReplacements) != 0 {
		captureC0, err = captureC0.RewriteDiskArtifactRefs(bundleRefReplacements)
		if err != nil {
			return ctl.Response{}, fmt.Errorf("export dependency ref rewrite: %w", err)
		}
		if replacement := bundleRefReplacements[parentRef]; replacement != "" {
			parentRef = replacement
		}
	}

	// The memory/Budget mutation barrier begins only after every predictable
	// output, admission, dependency, merge and C1-source check has completed.
	// From here through sink commit (and destroy/resume recovery), balloon and
	// memory.high lifecycle changes are serialized with the freeze point.
	releaseMemoryBarrier := func() {}
	if memoryController != nil {
		releaseMemoryBarrier, err = memoryController.BeginSnapshot(ctx)
		if err != nil {
			return ctl.Response{}, fmt.Errorf("export: begin memory Budget barrier: %w", err)
		}
	}
	defer func() {
		if err == nil && !req.ResumeAfter {
			return
		}
		releaseMemoryBarrier()
	}()
	previousMemoryHigh, memoryHighLock, err := liftVMMMemoryHigh(cgroupPath)
	if err != nil {
		return ctl.Response{}, fmt.Errorf("export: lift VMM memory.high: %w", err)
	}
	defer func() {
		if err == nil && !req.ResumeAfter {
			return
		}
		_, restoreErr := restoreVMMMemoryHigh(cgroupPath, previousMemoryHigh)
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("export: restore VMM memory.high: %w", restoreErr))
		}
		if memoryHighLock != nil {
			err = errors.Join(err, memoryHighLock.Close())
		}
	}()
	if delay := vmmMemoryHighThrottleDrainDelay(cgroupPath, previousMemoryHigh); delay > 0 {
		if err := waitVMMMemoryHighThrottleDrain(delay, chExited); err != nil {
			return ctl.Response{}, fmt.Errorf("export: drain VMM memory.high throttles: %w", err)
		}
	}
	defer func() {
		if err == nil && !req.ResumeAfter {
			go destroyAfterSnapshot(chSock, chProcess, memoryHighLock, releaseMemoryBarrier, chExited, opts.Cfg.CHApiDeadline(), logf)
		}
	}()
	recoveryTerminated := false
	forwarderNeedsAbort := false
	if forwarder != nil {
		forwarder.Pause()
		forwarderNeedsAbort = true
		defer func() {
			if forwarderNeedsAbort {
				forwarder.AbortAndDrain()
			}
			if !recoveryTerminated && (err != nil || req.ResumeAfter) {
				forwarder.Resume()
			}
		}()
	}
	if pinger != nil {
		if pauseErr := pinger.PauseContext(ctx); pauseErr != nil {
			pinger.Resume()
			return ctl.Response{}, fmt.Errorf("export: drain pinger before quiesce: %w", pauseErr)
		}
		defer func() {
			if !recoveryTerminated && (err != nil || req.ResumeAfter) {
				pinger.Resume()
			}
		}()
	}
	client := &guestlink.HostClient{BasePath: filepath.Join(runDir, "vsock.sock"), Logf: logf}
	if pinger != nil && pinger.Client != nil {
		client = pinger.Client
	}
	guestRecoveryRequired := false
	failRecovery := func(recoveryErr error) error {
		if recoveryErr == nil {
			return nil
		}
		shutdownErr, terminal := terminateAfterCaptureRecoveryFailure(
			chSock, chProcess, chExited, opts.Cfg.CHApiDeadline(), logf)
		recoveryTerminated = terminal
		combined := errors.Join(recoveryErr, shutdownErr)
		if terminal {
			return &terminalCaptureError{err: combined}
		}
		return combined
	}
	reattach := func(reason string) error {
		if !guestRecoveryRequired {
			return nil
		}
		if reattachMUX == nil {
			return fmt.Errorf("export: cannot recover guest after %s: stdio MUX re-attach is unavailable", reason)
		}
		if reattachErr := reattachMUX(); reattachErr != nil {
			return fmt.Errorf("export: stdio MUX re-attach after %s: %w", reason, reattachErr)
		}
		logf("stdio MUX re-attached after %s", reason)
		guestRecoveryRequired = false
		return nil
	}
	guestRecoveryRequired = true
	if _, quiesceErr := guestlink.SendQuiesceContext(ctx, client, true); quiesceErr != nil {
		if forwarderNeedsAbort {
			forwarder.AbortAndDrain()
			forwarderNeedsAbort = false
		}
		recoveryErr := failRecovery(reattach("failed quiesce"))
		return ctl.Response{}, errors.Join(fmt.Errorf("export quiesce: %w", quiesceErr), recoveryErr)
	}
	if forwarder != nil {
		forwarder.Drain()
		forwarderNeedsAbort = false
	}
	sinkOpen = false // snapshot.Export owns and closes the sink on every path.
	out, err := snapshot.Export(ctx, snapshot.ExportSources{
		SandboxID: opts.SandboxID, APISock: chSock, CHApiDeadline: opts.Cfg.CHApiDeadline(),
		PortableConfig: captureC0, ParentSandboxRef: parentRef,
		Diffs: diffs, Quiescer: &allQuiescer{servers: servers}, Logf: logf,
		LocalCodec: opts.LocalCodec, LocalRequired: opts.LocalRequired, MergeBaseOpener: mergeBaseOpener,
	}, sink, req.ResumeAfter)
	if err != nil {
		return ctl.Response{}, errors.Join(err, failRecovery(reattach("failed export")))
	}
	if req.ResumeAfter {
		if reattachErr := reattach("resumed export"); reattachErr != nil {
			return ctl.Response{}, failRecovery(reattachErr)
		}
	}
	resp = ctl.Response{
		SandboxRef: out.SandboxRef, SandboxPath: out.SandboxPath,
		DiskRefs: out.DataRefs, DiskPaths: out.DataPaths,
		WallclockPauseMs: out.WallclockPauseMs, WallclockDumpMs: out.WallclockDumpMs,
	}
	if parsed, parseErr := manifest.ParseRef(out.SandboxRef); parseErr == nil && parsed.Scheme == manifest.RefSchemeManifest {
		resp.SandboxManifestKey = parsed.Path
	}
	return resp, nil
}

// handleSnapshotRequest executes one snapshot_request received via
// ctl.sock. Wraps pkg/snapshot.Take with the runtime state
// owned by Run. Upload mode opens a fresh manifest.IngesterCloser
// here (per request) so the snapshot write path never shares a
// connection pool with the long-lived disk read path.
func handleSnapshotRequest(
	ctx context.Context,
	req ctl.Request,
	opts RunOptions,
	mfd *memory.Memfd,
	disks []SnapDiskRef, // writable diffs, logical order (root, then data disks)
	servers []*vhost.Server, // all vhost servers (quiesced together)
	chSock, runDir string,
	cgroupPath string,
	chExited <-chan struct{},
	chProcess processSignaler,
	pinger *guestlink.Pinger,
	forwarder *Forwarder, // gates new work, then drains host relays after guest quiesce; may be nil
	reattachMUX func() error, // re-establishes the stdio MUX after a resumed snapshot attempt; may be nil
	memoryController *resctl.MemoryController,
	logf func(string, ...any),
) (resp ctl.Response, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dropCaches := req.DropCachesEnabled()
	mergeRef := req.MergeRefEnabled()
	snapshotMode, err := req.SnapshotMode()
	if err != nil {
		return ctl.Response{}, err
	}
	memoryBinding := opts.MemoryBinding
	localParent := false
	if memoryBinding != nil && memoryBinding.SnapshotRef != "" {
		parentRef, parseErr := manifest.ParseRef(memoryBinding.SnapshotRef)
		if parseErr != nil {
			return ctl.Response{}, fmt.Errorf("parent snapshot ref: %w", parseErr)
		}
		localParent = parentRef.Scheme == manifest.RefSchemeFile && parentRef.DigestScheme != "manifest"
		if memoryBinding.RuntimeRef != "" {
			runtimeRef, runtimeErr := manifest.ParseRef(memoryBinding.RuntimeRef)
			if runtimeErr != nil {
				return ctl.Response{}, fmt.Errorf("parent snapshot runtime ref: %w", runtimeErr)
			}
			localParent = runtimeRef.Scheme == manifest.RefSchemeFile && runtimeRef.DigestScheme != "manifest"
		}
	}

	// Finish every predictable request/artifact/directory check before pausing
	// forwards or the pinger and, critically, before asking the guest to freeze.
	if err := validateSandboxID(opts.SandboxID); err != nil {
		return ctl.Response{}, fmt.Errorf("snapshot: %w", err)
	}
	if opts.PortableConfig == nil {
		return ctl.Response{}, fmt.Errorf("snapshot: immutable C0 is unavailable")
	}
	if req.Upload && (opts.ManifestCfg == nil || opts.ManifestCfg.Store.Endpoint == "") {
		return ctl.Response{}, fmt.Errorf("upload mode requires --manifest-config or MANIFEST_CONFIG (manifest store endpoints)")
	}
	if req.Upload && req.OutDir != "" {
		return ctl.Response{}, fmt.Errorf("--output and --upload are mutually exclusive")
	}
	if req.Upload && req.Mode != "" {
		return ctl.Response{}, fmt.Errorf("--upload and explicit --mode are mutually exclusive")
	}
	if snapshotMode == ctl.SnapshotModeBundle && opts.ManifestCfg == nil {
		return ctl.Response{}, fmt.Errorf("bundle mode requires --manifest-config or MANIFEST_CONFIG")
	}
	if !req.Upload && req.OutDir == "" {
		return ctl.Response{}, fmt.Errorf("--output and --upload are mutually exclusive; one is required")
	}
	if opts.Cfg == nil {
		return ctl.Response{}, errors.New("snapshot: runtime host configuration is unavailable")
	}
	if err := opts.PortableConfig.Validate(); err != nil {
		return ctl.Response{}, fmt.Errorf("snapshot: immutable C0: %w", err)
	}
	if mfd == nil || mfd.Size() == 0 {
		return ctl.Response{}, errors.New("snapshot: memory backing is unavailable")
	}
	if strings.TrimSpace(chSock) == "" {
		return ctl.Response{}, errors.New("snapshot: Cloud Hypervisor API socket is unavailable")
	}
	if strings.TrimSpace(runDir) == "" {
		return ctl.Response{}, errors.New("snapshot: run directory is unavailable")
	}
	if reattachMUX == nil && (req.ResumeAfter || chProcess == nil) {
		return ctl.Response{}, errors.New("snapshot: stdio MUX re-attach is unavailable and capture recovery cannot be guaranteed")
	}
	if len(disks) != 1+len(opts.Cfg.Boot.Disks) {
		return ctl.Response{}, fmt.Errorf("snapshot: runtime disk count %d does not match configured disk count %d",
			len(disks), 1+len(opts.Cfg.Boot.Disks))
	}
	mergeBaseOpener := newDiskMergeBaseOpener(opts)
	memoryMergeBaseOpener := newMemoryMergeBaseOpener(opts)
	var bundleSink *snapshot.BundleSink
	var bundleAdmission store.WriteAdmission

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
	if snapshotMode == ctl.SnapshotModeBundle {
		bundleAdmission, err = opts.ManifestCfg.WriteAdmission(ctx)
		if err != nil {
			return ctl.Response{}, fmt.Errorf("snapshot Bundle admission: %w", err)
		}
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
		parentDiskRef, parentDiskPath, parentErr := currentDiskParentBinding(opts, i)
		if parentErr != nil {
			return ctl.Response{}, fmt.Errorf("snapshot: disk %d parent binding: %w", i, parentErr)
		}
		mergeDisk := false
		if parentDiskRef != "" {
			ref, parseErr := manifest.ParseRef(parentDiskRef)
			if parseErr != nil {
				return ctl.Response{}, fmt.Errorf("snapshot: disk %d parent ref: %w", i, parseErr)
			}
			switch {
			case ref.Scheme == manifest.RefSchemeFile && ref.DigestScheme != "manifest":
				dd.MergeBase, err = resolvedLocalMergeRef(parentDiskPath, parentDiskRef)
				mergeDisk = err == nil
			case mergeRef && ref.Scheme == manifest.RefSchemeManifest:
				dd.MergeBase, mergeDisk, err = bundleManifestMergeRef(ctx, parentDiskRef, opts)
			}
			if err != nil {
				return ctl.Response{}, fmt.Errorf("snapshot: disk %d merge base identity: %w", i, err)
			}
		}
		if mergeDisk {
			if err := snapshot.ValidateMergeBaseWithOpener(ctx, dd.MergeBase, diffSize, opts.LocalCodec, opts.LocalRequired, mergeBaseOpener); err != nil {
				return ctl.Response{}, fmt.Errorf("snapshot: disk %d merge base: %w", i, err)
			}
			diskMerged[i] = true
		} else {
			dd.MergeBase = ""
		}
		diffs[i] = dd
	}
	prospectiveParentRef := ""
	if opts.SourceBinding != nil {
		prospectiveParentRef = opts.SourceBinding.SandboxRef
	}
	if err := snapshot.ValidateExportGraph(opts.PortableConfig, prospectiveParentRef, diskMerged); err != nil {
		return ctl.Response{}, fmt.Errorf("snapshot: prospective C1: %w", err)
	}

	mergeMemory := false
	memoryMergeBase := ""
	if localParent && mergeRef {
		if memoryBinding == nil || memoryBinding.RuntimeRef == "" {
			return ctl.Response{}, fmt.Errorf("snapshot: local parent lacks a memory merge path")
		}
		memoryMergeBase, err = resolvedBindingMergeRef(memoryBinding.RuntimeRef, memoryBinding.RelativeDir, opts.RefLocations)
		if err != nil {
			return ctl.Response{}, fmt.Errorf("snapshot: memory merge base identity: %w", err)
		}
		if err := snapshot.ValidateMergeBaseWithOpener(ctx, memoryMergeBase, int64(mfd.Size()), opts.LocalCodec, opts.LocalRequired, memoryMergeBaseOpener); err != nil {
			return ctl.Response{}, fmt.Errorf("snapshot: memory merge base: %w", err)
		}
		mergeMemory = true
	} else if mergeRef && memoryBinding != nil && memoryBinding.BundleSource != nil && memoryBinding.SnapshotRef != "" {
		memoryMergeBase, mergeMemory, err = bundleMemoryManifestMergeRef(ctx, memoryBinding.SnapshotRef, opts)
		if err != nil {
			return ctl.Response{}, fmt.Errorf("snapshot: Bundle memory merge base identity: %w", err)
		}
		if mergeMemory {
			if err := snapshot.ValidateMergeBaseWithOpener(ctx, memoryMergeBase, int64(mfd.Size()), opts.LocalCodec, opts.LocalRequired, memoryMergeBaseOpener); err != nil {
				return ctl.Response{}, fmt.Errorf("snapshot: Bundle memory merge base: %w", err)
			}
		}
	}
	resultMemoryRefs, err := memoryRefsForSnapshot(memoryBinding, mergeMemory)
	if err != nil {
		return ctl.Response{}, err
	}
	var (
		dependencyPlan        *snapshotBundlePlan
		bundleRefReplacements map[string]string
		dependencyPlanOpen    bool
	)
	defer func() {
		if dependencyPlanOpen {
			err = errors.Join(err, dependencyPlan.Close())
		}
	}()
	if snapshotMode == ctl.SnapshotModeBundle {
		plan, planErr := prepareSnapshotBundlePlan(ctx, opts, resultMemoryRefs,
			diskMerged, req.OutDir, bundleAdmission)
		if planErr != nil {
			return ctl.Response{}, planErr
		}
		dependencyPlan = plan
		dependencyPlanOpen = true
		bundleSink, err = snapshot.NewPlannedBundleSink(req.OutDir, opts.SandboxID,
			opts.ManifestCfg, opts.CustomerKeyFn, bundleAdmission, plan.Refs(), logf)
		if err != nil {
			return ctl.Response{}, err
		}
	} else {
		dependencyPlan, err = prepareSnapshotDependencyPlan(ctx, opts, resultMemoryRefs,
			diskMerged, req.OutDir, store.WriteAdmission{}, false)
		if err != nil {
			return ctl.Response{}, err
		}
		dependencyPlanOpen = true
	}

	// Construct the sink before quiesce as well: malformed manifest/store
	// configuration must not be discovered only after the guest is frozen.
	var sink snapshot.ArtifactSink
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
		ingestSink = snapshot.NewIngestSink(ing, logf)
		sink = ingestSink
	} else if snapshotMode == ctl.SnapshotModeBundle {
		sink = bundleSink
	} else {
		sink = snapshot.NewFileSink(req.OutDir, opts.SandboxID, opts.LocalCodec, opts.LocalRequired, logf)
	}
	sinkOpen := true
	defer func() {
		if sinkOpen {
			err = errors.Join(err, sink.Close())
		}
	}()
	if bundleSink != nil {
		bundleRefReplacements, err = dependencyPlan.Ingest(ctx, bundleSink, logf)
	} else {
		bundleRefReplacements, err = dependencyPlan.Emit(ctx, sink, logf)
	}
	planCloseErr := dependencyPlan.Close()
	dependencyPlanOpen = false
	if err != nil || planCloseErr != nil {
		return ctl.Response{}, fmt.Errorf("snapshot dependencies: %w", errors.Join(err, planCloseErr))
	}

	cfg := opts.Cfg
	captureC0 := opts.PortableConfig
	captureMemoryRefs := append([]string(nil), resultMemoryRefs...)
	captureParentSandboxRef := ""
	if opts.SourceBinding != nil {
		captureParentSandboxRef = opts.SourceBinding.SandboxRef
	}
	if len(bundleRefReplacements) != 0 {
		captureC0, err = opts.PortableConfig.RewriteDiskArtifactRefs(bundleRefReplacements)
		if err != nil {
			return ctl.Response{}, fmt.Errorf("snapshot dependency ref rewrite: %w", err)
		}
		if replacement := bundleRefReplacements[captureParentSandboxRef]; replacement != "" {
			captureParentSandboxRef = replacement
		}
		for i := range captureMemoryRefs {
			if replacement := bundleRefReplacements[captureMemoryRefs[i]]; replacement != "" {
				captureMemoryRefs[i] = replacement
			}
		}
	}
	if err := validateProspectiveMemoryConfig(captureMemoryRefs); err != nil {
		return ctl.Response{}, fmt.Errorf("snapshot: rewritten memory config: %w", err)
	}
	sandboxID := opts.SandboxID

	// Every predictable output, admission, dependency, merge and provenance
	// check has completed. Hold the memory/Budget mutation barrier only for the
	// actual freeze/capture/commit window and ordered destroy/resume recovery.
	releaseMemoryBarrier := func() {}
	if memoryController != nil {
		var barrierErr error
		releaseMemoryBarrier, barrierErr = memoryController.BeginSnapshot(ctx)
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

	guestRecoveryRequired := false
	recoveryTerminated := false
	reattachRunningGuest := func(reason string) error {
		if !guestRecoveryRequired {
			return nil
		}
		if reattachMUX == nil {
			return fmt.Errorf("snapshot: cannot recover guest after %s: stdio MUX re-attach is unavailable", reason)
		}
		if reattachErr := reattachMUX(); reattachErr != nil {
			return fmt.Errorf("snapshot: stdio MUX re-attach after %s: %w", reason, reattachErr)
		}
		logf("stdio MUX re-attached after %s", reason)
		guestRecoveryRequired = false
		return nil
	}
	failRecovery := func(recoveryErr error) error {
		if recoveryErr == nil {
			return nil
		}
		shutdownErr, terminal := terminateAfterCaptureRecoveryFailure(
			chSock, chProcess, chExited, opts.Cfg.CHApiDeadline(), logf)
		recoveryTerminated = terminal
		combined := errors.Join(recoveryErr, shutdownErr)
		if terminal {
			return &terminalCaptureError{err: combined}
		}
		return combined
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

	// Quiesce sequence (docs/sandbox.md §6.2, sandbox-init.md §3.4):
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
	// that is what makes `sandbox-ctl run` return (docs/sandbox.md §6.2).
	defer func() {
		if err == nil && !req.ResumeAfter {
			go destroyAfterSnapshot(chSock, chProcess, memoryHighLock, releaseMemoryBarrier, chExited, opts.Cfg.CHApiDeadline(), logf)
		}
	}()
	// Gate new port-forward/exec work before asking the guest to quiesce, without
	// perturbing admitted transports. The guest owns the authoritative
	// reverse-channel close; after `quiesced`, Drain joins the host handlers so
	// no teardown can race /vm.pause. Resume mirrors the pinger: stay paused only
	// on the success + destroy path.
	forwarderNeedsAbort := false
	if forwarder != nil {
		forwarder.Pause()
		forwarderNeedsAbort = true
		defer func() {
			if forwarderNeedsAbort {
				forwarder.AbortAndDrain()
			}
			if recoveryTerminated {
				return
			}
			if err == nil && !req.ResumeAfter {
				return
			}
			forwarder.Resume()
		}()
	}
	if pinger != nil {
		if pauseErr := pinger.PauseContext(ctx); pauseErr != nil {
			pinger.Resume()
			return ctl.Response{}, fmt.Errorf("snapshot: drain pinger before quiesce: %w", pauseErr)
		}
		// Resume on the way out UNLESS this is the success + destroy
		// path: there the VM stays paused while destroyAfterSnapshot
		// tears it down, so a resumed pinger only spews misleading
		// `ping ... i/o timeout` lines against a paused/dying VM. On
		// error (sandbox keeps running) or --resume (sandbox resumed),
		// the pinger must come back.
		defer func() {
			if recoveryTerminated {
				return
			}
			if err == nil && !req.ResumeAfter {
				return
			}
			pinger.Resume()
		}()
	}
	client := &guestlink.HostClient{BasePath: filepath.Join(runDir, "vsock.sock"), Logf: logf}
	if pinger != nil && pinger.Client != nil {
		client = pinger.Client
	}
	guestRecoveryRequired = true
	result, qerr := guestlink.SendQuiesceContext(ctx, client, !dropCaches)
	if qerr != nil {
		if forwarderNeedsAbort {
			forwarder.AbortAndDrain()
			forwarderNeedsAbort = false
		}
		operationErr := fmt.Errorf("quiesce: %w", qerr)
		logf("quiesce: %v (aborting snapshot)", operationErr)
		return ctl.Response{}, errors.Join(operationErr, failRecovery(reattachRunningGuest("failed quiesce")))
	}
	if forwarder != nil {
		forwarder.Drain()
		forwarderNeedsAbort = false
	}
	dropCachesResult = result
	guestQuiesced = true
	logf("quiesce: guest acked (drop_caches=%s, MUX + forwards closed), proceeding to /vm.pause", result)
	// Record the last guest observation together with one exact CH target/current
	// pair at the freeze boundary. BeginSnapshot still holds the balloon mutation
	// gate, and a successful guest quiesce has already stopped the application;
	// the following snapshot.Take is the first operation allowed to pause CH.
	if memoryController != nil {
		freeze, captureErr := memoryController.CaptureState(ctx)
		if captureErr != nil {
			logf("snapshot: freeze memory observation unavailable: %v", captureErr)
		} else {
			logf("snapshot: freeze memory MemAvailable=%d Cached=%d BalloonTarget=%d BalloonCurrent=%d TargetBudget=%d CurrentBudget=%d ObservedBudget=%d Reservation=%d report=%d/%d",
				freeze.GuestMemAvailable, freeze.GuestCached, freeze.BalloonTarget,
				freeze.BalloonCurrent, freeze.TargetBudget, freeze.CurrentBudget,
				freeze.ObservedBudget, freeze.Reservation, freeze.ReportEpoch, freeze.ReportSeq)
		}
	}

	// Pick the sink. --output writes sparse, content-addressed local files;
	// --upload streams to a fresh Ingester (its own store client, never shared
	// with the long-lived read-side fetcher). Either way Take streams the
	// multi-GiB memory + overlay straight to the sink — only CH's tiny
	// config.json/state.json transit the /run tmpfs staging dir.
	// Per-disk diff list (root, then data disks) for Take. Restored from a LOCAL
	// artifact ⇒ MERGE this run's resident delta onto the parent local layer
	// (replace the next-newest layer, not stack) for BOTH --output and --upload;
	// E/C1 drops that direct parent to match. Cold/manifest parents stack.
	src := snapshot.Sources{
		Context:               ctx,
		SandboxID:             sandboxID,
		APISock:               chSock,
		MemfdFD:               mfd.FD(),
		MemfdSize:             int64(mfd.Size()),
		Diffs:                 diffs,
		PortableConfig:        captureC0,
		MemoryFromRefs:        captureMemoryRefs,
		StagingDir:            stagingDir,
		CHApiDeadline:         cfg.CHApiDeadline(),
		Quiescer:              &allQuiescer{servers: servers},
		Logf:                  logf,
		LocalCodec:            opts.LocalCodec,
		LocalRequired:         opts.LocalRequired,
		MergeBaseOpener:       mergeBaseOpener,
		MemoryMergeBaseOpener: memoryMergeBaseOpener,
	}
	src.ParentSandboxRef = captureParentSandboxRef
	if mergeMemory {
		src.MergeBaseSnapshot = memoryMergeBase
	}
	sinkOpen = false // snapshot.Take owns and closes the sink on every path.
	out, err := snapshot.Take(src, sink, req.ResumeAfter)
	if err != nil {
		// SendQuiesce closes the old MUX and freezes the application. Take
		// restores the VM/backend state on failure; reattach completes the
		// guest-side recovery and thaws the application before we return.
		if guestQuiesced {
			return ctl.Response{}, errors.Join(err, failRecovery(reattachRunningGuest("failed snapshot")))
		}
		return ctl.Response{}, err
	}

	// --resume: Take has resumed CH, but the guest closed the stdio MUX
	// during quiesce. Re-establish it before reporting success: attach_ack is
	// also the guest's thaw boundary. Do this before any upload work so the app,
	// which is
	// already running again, isn't blocked on a full stdout pipe for long.
	if req.ResumeAfter {
		if reattachErr := reattachRunningGuest("snapshot"); reattachErr != nil {
			return ctl.Response{}, failRecovery(reattachErr)
		}
	}

	resp = ctl.Response{
		MemorySize:       out.MemorySize,
		MemoryResident:   out.MemoryResident,
		WallclockPauseMs: out.WallclockPauseMs,
		WallclockDumpMs:  out.WallclockDumpMs,
		DropCachesResult: dropCachesResult,
		SnapshotRef:      out.SnapshotRef,
		SandboxRef:       out.SandboxRef,
		SandboxPath:      out.SandboxPath,
	}
	// Keep the established overlay_* response fields populated for current
	// callers. Their value is now the committed Sandbox E; the disk graph is
	// carried exclusively by E.sandbox.runtime.cfg.
	if !req.Upload {
		resp.SnapshotPath = out.SnapshotPath
		resp.OverlayPath = out.SandboxPath
		resp.OverlayRef = out.SandboxRef
		return resp, nil
	}

	// Upload mode: the IngestSink already streamed the overlays + bundle to the
	// store during Take (resident pages only). Report the manifest keys it
	// produced (out.*Ref are manifest://<key>) plus per-artifact dedup stats.
	// OverlayRef stays scheme-tagged as a compatibility mirror of SandboxRef.
	_, bundleRes := ingestSink.Results()
	sandboxRes := ingestSink.SandboxResult()
	resp.SnapshotManifestKey = strings.TrimPrefix(out.SnapshotRef, "manifest://")
	resp.SandboxManifestKey = strings.TrimPrefix(out.SandboxRef, "manifest://")
	resp.OverlayManifestKey = resp.SandboxManifestKey
	resp.OverlayRef = out.SandboxRef
	if sandboxRes != nil && bundleRes != nil {
		resp.Msg = fmt.Sprintf("upload OK; Sandbox E stored=%d dedup=%d, Snapshot S stored=%d dedup=%d",
			sandboxRes.StoredChunks, sandboxRes.DedupChunks, bundleRes.StoredChunks, bundleRes.DedupChunks)
	}
	return resp, nil
}

// destroyAfterSnapshotDelay is how long destroyAfterSnapshot waits before
// tearing the VMM down — long enough for the snapshot_done response to
// flush over ctl.sock back to the `sandbox-ctl snapshot` CLI (a tiny JSON
// over a local UDS; this margin is generous).
const destroyAfterSnapshotDelay = 300 * time.Millisecond

// terminateAfterCaptureRecoveryFailure is the fail-safe for a resumed
// capture whose guest MUX cannot be reattached even after the idempotent
// attach retry. Keeping a VM alive in that state would leave the app cgroup
// frozen (or running without its stdio transport), so terminate it while the
// capture's memory/Budget and memory.high lifecycle guards are still held.
// terminal reports whether teardown was initiated; callers use it to keep the
// capture gate closed and avoid restarting pinger/forward activity.
func terminateAfterCaptureRecoveryFailure(
	chSock string,
	chProcess processSignaler,
	chExited <-chan struct{},
	respDeadline time.Duration,
	logf func(string, ...any),
) (retErr error, terminal bool) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if chProcess == nil && chExited == nil {
		return errors.New("capture recovery failed and no Cloud Hypervisor lifecycle handle is available"), false
	}
	terminal = true
	shutdownErr := (chapi.Client{Sock: chSock, RespDeadline: respDeadline}).ShutdownVMM()
	if shutdownErr == nil {
		logf("capture recovery failed — requested VMM shutdown")
	} else {
		logf("capture recovery failed — vmm.shutdown: %v", shutdownErr)
		if chProcess == nil {
			retErr = errors.Join(retErr, errors.New("capture recovery teardown cannot signal Cloud Hypervisor"))
		} else if signalErr := chProcess.Signal(syscall.SIGTERM); signalErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("capture recovery SIGTERM fallback: %w", signalErr))
		} else {
			logf("capture recovery failed — sent SIGTERM fallback")
		}
	}
	if chExited == nil {
		return retErr, terminal
	}
	waitForExit := func(duration time.Duration) bool {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-chExited:
			return true
		case <-timer.C:
			return false
		}
	}
	if waitForExit(chShutdownGrace) {
		return retErr, terminal
	}
	if chProcess == nil {
		return errors.Join(retErr, fmt.Errorf("Cloud Hypervisor did not exit within %s and no process signaler is available", chShutdownGrace)), terminal
	}
	if signalErr := chProcess.Signal(syscall.SIGKILL); signalErr != nil {
		retErr = errors.Join(retErr, fmt.Errorf("capture recovery SIGKILL after %s: %w", chShutdownGrace, signalErr))
	} else {
		logf("capture recovery failed — sent SIGKILL after %s", chShutdownGrace)
	}
	if !waitForExit(chShutdownGrace) {
		retErr = errors.Join(retErr, fmt.Errorf("Cloud Hypervisor did not exit within %s after SIGKILL", chShutdownGrace))
	}
	return retErr, terminal
}

// destroyAfterSnapshot tears the VMM down (PUT /api/v1/vmm.shutdown) so
// the `sandbox-ctl run` process owning this ctl.sock returns. Run on a
// goroutine on the resume_after=false ("destroy") path: by the time the
// delay elapses the snapshot_done response has been queued + sent. The
// memory.high lifecycle lock and balloon mutation barrier remain held until CH
// exits, including when the shutdown request itself fails. Errors are only
// logged — the sandbox is being torn down regardless.
func destroyAfterSnapshot(
	chSock string,
	chProcess processSignaler,
	memoryHighLock *os.File,
	releaseMemoryBarrier func(),
	chExited <-chan struct{},
	respDeadline time.Duration,
	logf func(string, ...any),
) {
	destroyAfterSnapshotWithBounds(chSock, chProcess, memoryHighLock, releaseMemoryBarrier,
		chExited, respDeadline, destroyAfterSnapshotDelay, chShutdownGrace, logf)
}

func destroyAfterSnapshotWithBounds(
	chSock string,
	chProcess processSignaler,
	memoryHighLock *os.File,
	releaseMemoryBarrier func(),
	chExited <-chan struct{},
	respDeadline, responseDelay, exitGrace time.Duration,
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
	if responseDelay > 0 {
		time.Sleep(responseDelay)
	}
	shutdownErr := (chapi.Client{Sock: chSock, RespDeadline: respDeadline}).ShutdownVMM()
	if shutdownErr != nil {
		logf("snapshot: destroy mode — vmm.shutdown: %v", shutdownErr)
		if chProcess != nil {
			if signalErr := chProcess.Signal(syscall.SIGTERM); signalErr != nil {
				logf("snapshot: destroy mode — SIGTERM fallback: %v", signalErr)
			} else {
				logf("snapshot: destroy mode — sent SIGTERM fallback")
			}
		}
	} else {
		logf("snapshot: destroy mode — VMM shutdown requested")
	}
	if chExited == nil {
		return
	}
	waitForExit := func() bool {
		timer := time.NewTimer(exitGrace)
		defer timer.Stop()
		select {
		case <-chExited:
			return true
		case <-timer.C:
			return false
		}
	}
	if waitForExit() {
		return
	}
	if chProcess != nil {
		if signalErr := chProcess.Signal(syscall.SIGKILL); signalErr != nil {
			logf("snapshot: destroy mode — SIGKILL after %s: %v", exitGrace, signalErr)
		} else {
			logf("snapshot: destroy mode — sent SIGKILL after %s", exitGrace)
		}
	} else {
		logf("snapshot: destroy mode — CH still running after %s and process signaler is unavailable", exitGrace)
		return
	}
	if !waitForExit() {
		logf("snapshot: destroy mode — CH exit was not observed within %s after SIGKILL; releasing lifecycle guards", exitGrace)
	}
}

func ensureSnapshotDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("empty path")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("path must be a real directory, not a symlink or non-directory")
	}
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if err := unix.Close(dirFD); err != nil {
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

type artifactDependencyRole uint8

const (
	dependencyDiskLayer artifactDependencyRole = iota
	dependencyRootImage
	dependencyMemorySnapshot
)

func (r artifactDependencyRole) String() string {
	switch r {
	case dependencyDiskLayer:
		return "disk layer"
	case dependencyRootImage:
		return "root image"
	case dependencyMemorySnapshot:
		return "memory Snapshot"
	default:
		return "unknown dependency"
	}
}

type artifactDependency struct {
	raw  string
	role artifactDependencyRole
}

type dependencyPlanImport struct {
	raw    string
	label  string
	role   artifactDependencyRole
	stream fetch.Stream
}

type dependencyPlanCopy struct {
	raw    string
	label  string
	key    store.ContentKey
	reader *manifestbundle.Reader
	stream fetch.Stream
}

// snapshotBundlePlan is also used for local and direct-Manifest dependency
// materialization. The historical name is kept private to avoid widening the
// lifecycle API. Every selected stream is opened and validated before guest
// quiesce; Emit then writes leaves through the same role-specific sink used by
// E/S capture.
type snapshotBundlePlan struct {
	admission    store.WriteAdmission
	refs         []string
	replacements map[string]string
	imports      []dependencyPlanImport
	copies       []dependencyPlanCopy
	closed       bool
}

// SandboxDependencyPlan is the narrow two-phase dependency closure used by
// offline EROFS export. Planning opens and validates every explicit disk ref;
// Bundle callers can then create their writer with the final refs list before
// emitting any metadata. It is intentionally specific to the Sandbox graph.
type SandboxDependencyPlan struct{ plan *snapshotBundlePlan }

func PrepareSandboxDependencyPlan(ctx context.Context, opts RunOptions, diskMerged []bool, outputDir string, admission store.WriteAdmission, bundleTarget bool) (*SandboxDependencyPlan, error) {
	plan, err := prepareSnapshotDependencyPlan(ctx, opts, nil, diskMerged, outputDir, admission, bundleTarget)
	if err != nil {
		return nil, err
	}
	return &SandboxDependencyPlan{plan: plan}, nil
}

func (p *SandboxDependencyPlan) Refs() []string {
	if p == nil {
		return nil
	}
	return p.plan.Refs()
}

func (p *SandboxDependencyPlan) Emit(ctx context.Context, sink snapshot.ArtifactSink, logf func(string, ...any)) (map[string]string, error) {
	if p == nil || p.plan == nil {
		return nil, errors.New("Sandbox dependency plan is unavailable")
	}
	if bundleSink, ok := sink.(*snapshot.BundleSink); ok {
		return p.plan.Ingest(ctx, bundleSink, logf)
	}
	return p.plan.Emit(ctx, sink, logf)
}

func (p *SandboxDependencyPlan) Close() error {
	if p == nil || p.plan == nil {
		return nil
	}
	return p.plan.Close()
}

func (p *snapshotBundlePlan) Refs() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.refs...)
}

func (p *snapshotBundlePlan) Ingest(ctx context.Context, sink *snapshot.BundleSink, logf func(string, ...any)) (map[string]string, error) {
	if p == nil || sink == nil {
		return nil, fmt.Errorf("snapshot Bundle plan or sink is unavailable")
	}
	if sink.Admission() != p.admission {
		return nil, fmt.Errorf("snapshot Bundle plan admission changed before ingest")
	}
	return p.emit(ctx, sink, logf)
}

func (p *snapshotBundlePlan) Emit(ctx context.Context, sink snapshot.ArtifactSink, logf func(string, ...any)) (map[string]string, error) {
	if p == nil || sink == nil {
		return nil, fmt.Errorf("artifact dependency plan or sink is unavailable")
	}
	if len(p.copies) != 0 {
		return nil, errors.New("artifact dependency plan requires a Bundle sink for exact Manifest copies")
	}
	return p.emit(ctx, sink, logf)
}

func (p *snapshotBundlePlan) emit(ctx context.Context, sink snapshot.ArtifactSink, logf func(string, ...any)) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bundleSink, ok := sink.(*snapshot.BundleSink); ok {
		for index := range p.copies {
			item := &p.copies[index]
			copyErr := bundleSink.CopyManifestFromBundle(ctx, item.key, item.reader)
			closeErr := item.stream.Close()
			item.stream = nil
			if copyErr != nil || closeErr != nil {
				return nil, errors.Join(copyErr, closeErr)
			}
			ref := "manifest://" + manifest.HexKey(item.key)
			p.replacements[item.raw] = ref
			if logf != nil {
				logf("bundle: copied %s Manifest %s", item.label, manifest.HexKey(item.key))
			}
		}
	}
	for index := range p.imports {
		item := &p.imports[index]
		if item.stream == nil {
			continue
		}
		var ref string
		var absorbErr error
		switch item.role {
		case dependencyMemorySnapshot:
			ref, _, absorbErr = sink.AbsorbSnapshot(ctx, item.stream)
		case dependencyDiskLayer:
			ref, _, absorbErr = sink.AbsorbOverlaySource(ctx, item.stream)
		case dependencyRootImage:
			ref, _, absorbErr = sink.AbsorbImageSource(ctx, item.stream)
		default:
			absorbErr = fmt.Errorf("unsupported dependency role %d", item.role)
		}
		closeErr := item.stream.Close()
		item.stream = nil
		if absorbErr != nil || closeErr != nil {
			return nil, errors.Join(absorbErr, closeErr)
		}
		p.replacements[item.raw] = ref
		if logf != nil {
			logf("artifact: collected %s as %s", item.label, ref)
		}
	}
	return cloneStringMap(p.replacements), nil
}

func (p *snapshotBundlePlan) Close() error {
	if p == nil || p.closed {
		return nil
	}
	p.closed = true
	var closeErr error
	for index := range p.imports {
		if p.imports[index].stream != nil {
			closeErr = errors.Join(closeErr, p.imports[index].stream.Close())
			p.imports[index].stream = nil
		}
	}
	for index := range p.copies {
		if p.copies[index].stream != nil {
			closeErr = errors.Join(closeErr, p.copies[index].stream.Close())
			p.copies[index].stream = nil
		}
	}
	return closeErr
}

func cloneStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func prepareSnapshotBundlePlan(
	ctx context.Context,
	opts RunOptions,
	memoryFromRefs []string,
	diskMerged []bool,
	outputDir string,
	admission store.WriteAdmission,
) (_ *snapshotBundlePlan, retErr error) {
	return prepareSnapshotDependencyPlan(ctx, opts, memoryFromRefs, diskMerged, outputDir, admission, true)
}

func prepareSnapshotDependencyPlan(
	ctx context.Context,
	opts RunOptions,
	memoryFromRefs []string,
	diskMerged []bool,
	outputDir string,
	admission store.WriteAdmission,
	bundleTarget bool,
) (_ *snapshotBundlePlan, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logicalRefs, err := snapshotLayerDependencies(opts, memoryFromRefs, diskMerged)
	if err != nil {
		return nil, err
	}

	plan := &snapshotBundlePlan{admission: admission, replacements: make(map[string]string)}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, plan.Close())
		}
	}()
	knownPaths := bundleKnownRefPaths(opts)
	seenLogical := make(map[string]artifactDependencyRole, len(logicalRefs))
	for index := len(logicalRefs) - 1; index >= 0; index-- {
		dependency := logicalRefs[index]
		raw := dependency.raw
		if previous, seen := seenLogical[raw]; seen {
			if previous != dependency.role {
				return nil, fmt.Errorf("artifact dependency %q is used as both %s and %s", raw, previous, dependency.role)
			}
			continue
		}
		seenLogical[raw] = dependency.role
		stream, reader, key, label, err := openSnapshotDependency(ctx, dependency, opts, knownPaths, outputDir)
		if err != nil {
			return nil, err
		}
		prepared, transformed, err := prepareSnapshotDependencyStream(ctx, stream, dependency.role)
		if err != nil {
			return nil, fmt.Errorf("prepare %s %q: %w", dependency.role, raw, err)
		}
		portable, retain, err := portableSnapshotDependencyRef(ctx, dependency, reader, opts)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("retain %s %q: %w", dependency.role, raw, err), prepared.Close())
		}
		if retain {
			if err := prepared.Close(); err != nil {
				return nil, fmt.Errorf("close retained %s %q: %w", dependency.role, raw, err)
			}
			if portable != raw {
				plan.replacements[raw] = portable
			}
			continue
		}
		if bundleTarget && reader != nil && reader.Admission() == admission && !transformed {
			plan.copies = append(plan.copies, dependencyPlanCopy{
				raw: raw, label: label, key: key, reader: reader, stream: prepared,
			})
			continue
		}
		plan.imports = append(plan.imports, dependencyPlanImport{
			raw: raw, label: label, role: dependency.role, stream: prepared,
		})
	}
	// Portable refs retain their own explicit provenance. Unlocated local
	// dependencies are materialized into the new destination, so a Bundle does
	// not need external ordered refs and its plan remains fixed before NewWriter
	// emits metadata.
	plan.refs = nil
	return plan, nil
}

// portableSnapshotDependencyRef keeps dependencies whose carrier already has
// portable provenance. A logical Manifest selected from a located Bundle is
// made explicit as that Bundle's located @manifest selector; a Manifest that
// came from the remote fetcher and an already-located file ref remain unchanged.
// Unlocated Bundle/file sources deliberately fall through to materialization.
func portableSnapshotDependencyRef(ctx context.Context, dependency artifactDependency, reader *manifestbundle.Reader, opts RunOptions) (string, bool, error) {
	ref, err := manifest.ParseRef(dependency.raw)
	if err != nil {
		return "", false, err
	}
	if ref.Scheme == manifest.RefSchemeFile {
		if ref.Location == "" {
			return "", false, nil
		}
		return ref.String(), true, nil
	}
	if ref.Scheme != manifest.RefSchemeManifest {
		return "", false, nil
	}
	if reader == nil {
		return ref.String(), true, nil
	}

	lookup := snapshotDependencyBundleLookup(dependency, opts)
	if lookup.binding == nil {
		return "", false, nil
	}
	physical, found, err := bundleManifestMergeRefWithLookup(ctx, dependency.raw, opts, lookup)
	if err != nil || !found {
		return "", false, err
	}
	physicalRef, err := manifest.ParseRef(physical)
	if err != nil {
		return "", false, err
	}
	if !physicalRef.Portable() {
		return "", false, nil
	}
	return physicalRef.String(), true, nil
}

func openSnapshotDependency(ctx context.Context, dependency artifactDependency, opts RunOptions, knownPaths map[string]string, outputDir string) (fetch.Stream, *manifestbundle.Reader, store.ContentKey, string, error) {
	ref, err := manifest.ParseRef(dependency.raw)
	if err != nil {
		return nil, nil, store.ContentKey{}, "", fmt.Errorf("artifact dependency %q: %w", dependency.raw, err)
	}
	label := dependency.role.String() + " " + dependency.raw
	if ref.Scheme == manifest.RefSchemeManifest {
		key, err := manifest.ParseHexKey(ref.Path)
		if err != nil {
			return nil, nil, store.ContentKey{}, "", err
		}
		lookup := snapshotDependencyBundleLookup(dependency, opts)
		if lookup.err != nil {
			return nil, nil, store.ContentKey{}, "", fmt.Errorf("%s Bundle provenance: %w", label, lookup.err)
		}
		if lookup.fetcher != nil {
			source, err := lookup.fetcher.SelectManifest(ctx, key)
			if err != nil {
				return nil, nil, store.ContentKey{}, "", err
			}
			stream, err := source.OpenManifest(ctx, key)
			return stream, source.Reader, key, label, err
		}
		if opts.Fetcher == nil {
			return nil, nil, store.ContentKey{}, "", fmt.Errorf("%s requires a Manifest fetcher", label)
		}
		stream, err := opts.Fetcher.OpenManifest(ctx, key)
		return stream, nil, key, label, err
	}
	if ref.Scheme != manifest.RefSchemeFile {
		return nil, nil, store.ContentKey{}, "", fmt.Errorf("%s uses unsupported scheme %q", label, ref.Scheme)
	}
	path, err := resolveBundleArtifactPath(ref, dependency.raw, knownPaths, outputDir, opts.RefLocations, bindingRelativeDirs(opts))
	if err != nil {
		return nil, nil, store.ContentKey{}, "", err
	}
	opened, err := artifact.OpenFileWithLocations(ctx, path, ref, opts.ManifestCfg, opts.CustomerKeyFn,
		opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
	if err != nil {
		return nil, nil, store.ContentKey{}, "", fmt.Errorf("open %s: %w", label, err)
	}
	key, bundled := opened.RootManifestKey()
	if !bundled {
		key = store.ContentKey{}
	}
	return opened, opened.BundleReader(), key, dependency.role.String() + " " + filepath.Base(path), nil
}

func prepareSnapshotDependencyStream(ctx context.Context, stream fetch.Stream, role artifactDependencyRole) (fetch.Stream, bool, error) {
	if stream == nil {
		return nil, false, errors.New("nil dependency stream")
	}
	switch role {
	case dependencyMemorySnapshot:
		root, err := snapshotfile.Open(ctx, stream)
		if err != nil {
			return nil, false, err
		}
		cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
		if err != nil {
			return nil, false, errors.Join(err, root.Close())
		}
		canonical, err := snapshot.MarshalConfig(cfg)
		if err != nil || !bytes.Equal(canonical, root.SnapshotConfig) {
			if err == nil {
				err = errors.New("snapshot.cfg is not canonically encoded")
			}
			return nil, false, errors.Join(err, root.Close())
		}
		return root.FullStream, false, nil
	case dependencyDiskLayer, dependencyRootImage:
		payload, sandboxLayer, err := sandboxfile.PayloadIfSandbox(ctx, stream, false)
		if err != nil {
			return nil, false, err
		}
		if role == dependencyDiskLayer {
			return payload, sandboxLayer, nil
		}
		originalSize := payload.Size()
		image, err := sandboxfile.OpenEROFSArtifact(ctx, payload)
		if err != nil {
			return nil, false, err
		}
		// A root-image dependency remains a flattened image artifact. Its block
		// consumer narrows to Payload later; publication must retain config.json
		// so image defaults remain available to builders and cold starts.
		return image.FullStream, sandboxLayer || image.FullStream.Size() != originalSize, nil
	default:
		return nil, false, errors.Join(fmt.Errorf("unsupported dependency role %d", role), stream.Close())
	}
}

func snapshotLayerDependencies(opts RunOptions, memoryFromRefs []string, diskMerged []bool) ([]artifactDependency, error) {
	if opts.PortableConfig == nil {
		return nil, errors.New("snapshot Bundle requires immutable C0")
	}
	if len(diskMerged) != 1+len(opts.PortableConfig.Boot.Disks) {
		return nil, fmt.Errorf("snapshot Bundle disk merge count %d does not match C0", len(diskMerged))
	}
	dependencies := make([]artifactDependency, 0, len(memoryFromRefs)+8)
	for _, raw := range memoryFromRefs {
		dependencies = append(dependencies, artifactDependency{raw: raw, role: dependencyMemorySnapshot})
	}
	appendRef := func(raw string, role artifactDependencyRole) {
		if raw != "" && raw != "self" {
			dependencies = append(dependencies, artifactDependency{raw: raw, role: role})
		}
	}
	appendRoot := func(root *config.PortableRootConfig, merged bool) {
		if root.Overlay == nil {
			if !merged {
				appendRef(root.Base, dependencyDiskLayer)
			}
			for _, raw := range root.BaseFromRefs {
				appendRef(raw, dependencyDiskLayer)
			}
			return
		}
		appendRef(root.Base, dependencyRootImage)
		if !merged {
			appendRef(root.Overlay.Base, dependencyDiskLayer)
		}
		for _, raw := range root.Overlay.BaseFromRefs {
			appendRef(raw, dependencyDiskLayer)
		}
	}
	appendRoot(&opts.PortableConfig.Boot.Root, diskMerged[0])
	if opts.SourceBinding != nil && !diskMerged[0] {
		role := dependencyDiskLayer
		root := &opts.PortableConfig.Boot.Root
		if root.Base == "self" && root.Overlay != nil && root.Overlay.Base == "" {
			role = dependencyRootImage
		}
		appendRef(opts.SourceBinding.SandboxRef, role)
	}
	for i := range opts.PortableConfig.Boot.Disks {
		appendRoot(&opts.PortableConfig.Boot.Disks[i].PortableRootConfig, diskMerged[i+1])
	}
	return dependencies, nil
}

type bundleLookup struct {
	binding *BundleSourceBinding
	reader  *manifestbundle.Reader
	fetcher *manifestbundle.ManifestFetcher
	err     error
}

func bundleLookupFor(binding *BundleSourceBinding, fallbackReader *manifestbundle.Reader, fallbackFetcher *manifestbundle.ManifestFetcher) bundleLookup {
	lookup := bundleLookup{binding: binding}
	if binding != nil {
		lookup.reader = binding.Reader
		lookup.fetcher = binding.Fetcher
		if (lookup.reader == nil) != (lookup.fetcher == nil) {
			lookup.err = errors.New("Bundle source reader/fetcher must be paired")
			return lookup
		}
	}
	if lookup.reader == nil && lookup.fetcher == nil {
		lookup.reader = fallbackReader
		lookup.fetcher = fallbackFetcher
	}
	if (lookup.reader == nil) != (lookup.fetcher == nil) {
		lookup.err = errors.New("Bundle lookup reader/fetcher must be paired")
	}
	return lookup
}

func sandboxBundleLookup(opts RunOptions) bundleLookup {
	if opts.SourceBinding != nil && opts.SourceBinding.BundleSource != nil {
		return bundleLookupFor(opts.SourceBinding.BundleSource, opts.BundleReader, opts.BundleFetcher)
	}
	if opts.MemoryBinding != nil && opts.MemoryBinding.BundleSource != nil {
		return bundleLookupFor(opts.MemoryBinding.BundleSource, opts.BundleReader, opts.BundleFetcher)
	}
	return bundleLookupFor(nil, opts.BundleReader, opts.BundleFetcher)
}

func memoryBundleLookup(opts RunOptions) bundleLookup {
	if opts.MemoryBinding == nil || opts.MemoryBinding.BundleSource == nil {
		return bundleLookup{}
	}
	return bundleLookupFor(opts.MemoryBinding.BundleSource, opts.BundleReader, opts.BundleFetcher)
}

func snapshotDependencyBundleLookup(dependency artifactDependency, opts RunOptions) bundleLookup {
	if dependency.role == dependencyMemorySnapshot {
		return memoryBundleLookup(opts)
	}
	return sandboxBundleLookup(opts)
}

func bundleSourceForManifest(ctx context.Context, key store.ContentKey, opts RunOptions) (string, string, bool, error) {
	return bundleSourceForLookup(ctx, key, opts, sandboxBundleLookup(opts))
}

func bundleSourceForLookup(ctx context.Context, key store.ContentKey, opts RunOptions, lookup bundleLookup) (string, string, bool, error) {
	if lookup.err != nil {
		return "", "", false, lookup.err
	}
	if lookup.fetcher == nil {
		return "", "", false, nil
	}
	source, err := lookup.fetcher.SelectManifest(ctx, key)
	if err != nil {
		return "", "", false, err
	}
	if source.Reader == nil {
		stream, err := source.OpenManifest(ctx, key)
		if err != nil {
			return "", "", false, err
		}
		if err := stream.Close(); err != nil {
			return "", "", false, err
		}
		return "", "", false, nil
	}
	bundleSource := lookup.binding
	if bundleSource == nil {
		return "", "", false, fmt.Errorf("selected Bundle source has no physical provenance")
	}
	if source.Reader == lookup.reader {
		return bundleSource.RootRef, bundleSource.RootPath, true, nil
	}
	if source.Ref == "" {
		return "", "", false, fmt.Errorf("selected referenced Bundle has no source ref")
	}
	ref, err := manifest.ParseRef(source.Ref)
	if err != nil {
		return "", "", false, err
	}
	path, err := opts.RefLocations.ResolveFile(ref, filepath.Dir(bundleSource.RootPath))
	if err != nil {
		return "", "", false, err
	}
	return source.Ref, path, true, nil
}

func activeBundleSource(opts RunOptions) *BundleSourceBinding {
	return sandboxBundleLookup(opts).binding
}

func bundleManifestMergeRef(ctx context.Context, raw string, opts RunOptions) (string, bool, error) {
	return bundleManifestMergeRefWithLookup(ctx, raw, opts, sandboxBundleLookup(opts))
}

func bundleMemoryManifestMergeRef(ctx context.Context, raw string, opts RunOptions) (string, bool, error) {
	return bundleManifestMergeRefWithLookup(ctx, raw, opts, memoryBundleLookup(opts))
}

func bundleManifestMergeRefWithLookup(ctx context.Context, raw string, opts RunOptions, lookup bundleLookup) (string, bool, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", false, err
	}
	if ref.Scheme != manifest.RefSchemeManifest {
		return "", false, nil
	}
	key, err := manifest.ParseHexKey(ref.Path)
	if err != nil {
		return "", false, err
	}
	sourceRef, sourcePath, found, err := bundleSourceForLookup(ctx, key, opts, lookup)
	if err != nil || !found {
		return "", false, err
	}
	physical, err := manifest.ParseRef(sourceRef)
	if err != nil {
		return "", false, err
	}
	if physical.Location == "" {
		physical.Path, err = filepath.Abs(sourcePath)
		if err != nil {
			return "", false, err
		}
	}
	physical.DigestScheme = "manifest"
	physical.Digest = manifest.HexKey(key)
	if err := physical.Validate(); err != nil {
		return "", false, err
	}
	return physical.String(), true, nil
}

func canonicalBundleSource(path string, ref manifest.Ref) (string, string, error) {
	if ref.Scheme != manifest.RefSchemeFile {
		return "", "", fmt.Errorf("Bundle source must use file://")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil {
		return "", "", err
	}
	ref.DigestScheme = ""
	ref.Digest = ""
	if ref.Location != "" {
		locationDir, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return "", "", err
		}
		if filepath.Clean(locationDir) != filepath.Clean(filepath.Dir(realPath)) {
			return "", "", fmt.Errorf("located Bundle alias target must remain in the same location directory")
		}
	}
	ref.Path = filepath.Base(realPath)
	raw := ref.String()
	if _, err := manifestbundle.EncodeRefs([]string{raw}); err != nil {
		return "", "", err
	}
	return raw, realPath, nil
}

func bundleKnownRefPaths(opts RunOptions) map[string]string {
	paths := make(map[string]string)
	add := func(identity, source, relativeDir string) {
		if identity == "" || source == "" {
			return
		}
		ref, err := manifest.ParseRef(source)
		if err != nil || ref.Scheme != manifest.RefSchemeFile {
			return
		}
		path, err := opts.RefLocations.ResolveFile(ref, relativeDir)
		if err == nil {
			paths[identity] = path
		}
	}
	if opts.SourceBinding != nil {
		add(opts.SourceBinding.SandboxRef, opts.SourceBinding.RuntimeRef, opts.SourceBinding.RelativeDir)
	}
	if opts.MemoryBinding != nil {
		add(opts.MemoryBinding.SnapshotRef, opts.MemoryBinding.RuntimeRef, opts.MemoryBinding.RelativeDir)
		for _, raw := range opts.MemoryBinding.FromRefs {
			if ref, err := manifest.ParseRef(raw); err == nil && ref.Scheme == manifest.RefSchemeFile {
				if path, resolveErr := opts.RefLocations.ResolveFile(ref, opts.MemoryBinding.RelativeDir); resolveErr == nil {
					paths[raw] = path
				}
			}
		}
	}
	if opts.PortableConfig != nil && opts.Cfg != nil {
		mapRootBinding(paths, &opts.PortableConfig.Boot.Root, &opts.Cfg.Boot.Root, opts.SourceBinding, opts.RefLocations)
		for i := range opts.PortableConfig.Boot.Disks {
			if i < len(opts.Cfg.Boot.Disks) {
				mapRootBinding(paths, &opts.PortableConfig.Boot.Disks[i].PortableRootConfig, &opts.Cfg.Boot.Disks[i].RootConfig, opts.SourceBinding, opts.RefLocations)
			}
		}
	}
	return paths
}

func mapRootBinding(paths map[string]string, portable *config.PortableRootConfig, runtime *config.RootConfig, source *RunSourceBinding, locations config.RefLocations) {
	if portable == nil || runtime == nil {
		return
	}
	mapOne := func(logical, bound string) {
		if logical == "" || logical == "self" || bound == "" {
			return
		}
		if ref, err := manifest.ParseRef(bound); err == nil && ref.Scheme == manifest.RefSchemeFile {
			relativeDir := ""
			if source != nil {
				relativeDir = source.RelativeDir
			}
			if path, resolveErr := locations.ResolveFile(ref, relativeDir); resolveErr == nil {
				paths[logical] = path
			}
		}
	}
	mapOne(portable.Base, runtime.Base)
	for i, logical := range portable.BaseFromRefs {
		if i < len(runtime.BaseFromRefs) {
			mapOne(logical, runtime.BaseFromRefs[i])
		}
	}
	if portable.Overlay != nil && runtime.Overlay != nil {
		mapOne(portable.Overlay.Base, runtime.Overlay.Base)
		for i, logical := range portable.Overlay.BaseFromRefs {
			if i < len(runtime.Overlay.BaseFromRefs) {
				mapOne(logical, runtime.Overlay.BaseFromRefs[i])
			}
		}
	}
}

func bindingRelativeDirs(opts RunOptions) []string {
	var dirs []string
	if opts.SourceBinding != nil && opts.SourceBinding.RelativeDir != "" {
		dirs = append(dirs, opts.SourceBinding.RelativeDir)
	}
	if opts.MemoryBinding != nil && opts.MemoryBinding.RelativeDir != "" {
		dirs = append(dirs, opts.MemoryBinding.RelativeDir)
	}
	return dirs
}

func resolveBundleArtifactPath(ref manifest.Ref, raw string, known map[string]string, outputDir string, locations config.RefLocations, relativeDirs []string) (string, error) {
	if path := known[raw]; path != "" {
		return path, nil
	}
	if ref.Location != "" {
		path, err := locations.ResolveFile(ref, "")
		if err != nil {
			return "", err
		}
		return path, nil
	}
	if filepath.IsAbs(ref.Path) {
		return ref.Path, nil
	}
	var candidates []string
	for _, relativeDir := range relativeDirs {
		candidates = append(candidates, filepath.Join(relativeDir, filepath.Base(ref.Path)))
	}
	if outputDir != "" {
		candidates = append(candidates, filepath.Join(outputDir, filepath.Base(ref.Path)))
	}
	candidates = append(candidates, ref.Path)
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("snapshot Bundle cannot resolve unlocated local ref %q before pause", raw)
}

// memoryRefsForSnapshot returns the memory lowers retained by the next S. A
// merge replaces only the direct parent S; its existing lowers remain in order.
func memoryRefsForSnapshot(binding *MemorySourceBinding, mergeParent bool) ([]string, error) {
	if binding == nil {
		return nil, nil
	}
	refs := prependRef(binding.SnapshotRef, binding.FromRefs)
	if mergeParent {
		refs = append([]string(nil), binding.FromRefs...)
	}
	normalized, err := normalizeLocalMemoryRefs(refs)
	if err != nil {
		return nil, err
	}
	if err := validateProspectiveMemoryConfig(normalized); err != nil {
		return nil, fmt.Errorf("snapshot: prospective memory config: %w", err)
	}
	return normalized, nil
}

func validateProspectiveMemoryConfig(refs []string) error {
	// The final E identity is not known until capture, but every sink emits a
	// bounded content-addressed ref. Validate the complete prospective S config
	// with the longest local E ref shape so count, duplicates, ref syntax and
	// the serialized-size limit all fail before quiesce. Callers repeat this
	// after dependency rewriting because distinct source aliases may collapse
	// to one content-addressed output identity.
	probeDigest := strings.Repeat("f", 64)
	if _, err := snapshot.MarshalConfig(&snapshot.Config{
		Version:    snapshot.SnapshotConfigVersion,
		SandboxRef: "file://" + probeDigest + ".sandbox@digest:" + probeDigest,
		FromRefs:   refs,
	}); err != nil {
		return err
	}
	return nil
}

func currentDiskParentBinding(opts RunOptions, index int) (string, string, error) {
	if opts.SourceBinding == nil {
		return "", "", nil
	}
	raw := ""
	relativeDir := opts.SourceBinding.RelativeDir
	if index == 0 {
		root := &opts.PortableConfig.Boot.Root
		if root.Base == "self" && root.Overlay != nil && root.Overlay.Base == "" {
			// self is the immutable EROFS image. The active ext4 upper is a
			// different device and must never be merged with the Sandbox payload.
			return "", "", nil
		}
		raw = opts.SourceBinding.RuntimeRef
	} else {
		diskIndex := index - 1
		if diskIndex >= len(opts.Cfg.Boot.Disks) {
			return "", "", fmt.Errorf("data disk index %d is out of range", diskIndex)
		}
		disk := &opts.Cfg.Boot.Disks[diskIndex].RootConfig
		raw = disk.Base
		if disk.Overlay != nil {
			raw = disk.Overlay.Base
		}
	}
	if raw == "" {
		return "", "", nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", "", err
	}
	if ref.Scheme != manifest.RefSchemeFile || ref.DigestScheme == "manifest" {
		return ref.String(), "", nil
	}
	resolved, err := resolvedBindingMergeRef(ref.String(), relativeDir, opts.RefLocations)
	if err != nil {
		return "", "", err
	}
	resolvedRef, err := manifest.ParseRef(resolved)
	if err != nil {
		return "", "", err
	}
	return resolvedRef.String(), resolvedRef.Path, nil
}

// newDiskMergeBaseOpener narrows a Sandbox E parent to its disk Payload before
// local merge. Ordinary overlay tarstreams remain whole.
func newDiskMergeBaseOpener(opts RunOptions) snapshot.MergeBaseOpener {
	return func(ctx context.Context, raw string) (fetch.Stream, error) {
		ref, err := manifest.ParseRef(raw)
		if err != nil || ref.Scheme != manifest.RefSchemeFile {
			return nil, fmt.Errorf("merge base requires resolved file:// ref")
		}
		path, err := opts.RefLocations.ResolveFile(ref, "")
		if err != nil {
			return nil, err
		}
		opened, err := artifact.OpenFileWithLocations(ctx, path, ref, opts.ManifestCfg, opts.CustomerKeyFn,
			opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return nil, err
		}
		requireSandbox := filepath.Ext(ref.Path) == ".sandbox"
		payload, _, err := sandboxfile.PayloadIfSandbox(ctx, opened, requireSandbox)
		if err != nil {
			return nil, err
		}
		return payload, nil
	}
}

// newMemoryMergeBaseOpener exposes only S's memory prefix. In particular, the
// strict Snapshot ZIP can never be merged into guest RAM during re-snapshot.
func newMemoryMergeBaseOpener(opts RunOptions) snapshot.MergeBaseOpener {
	return func(ctx context.Context, raw string) (fetch.Stream, error) {
		ref, err := manifest.ParseRef(raw)
		if err != nil || ref.Scheme != manifest.RefSchemeFile {
			return nil, fmt.Errorf("memory merge base requires resolved file:// ref")
		}
		path, err := opts.RefLocations.ResolveFile(ref, "")
		if err != nil {
			return nil, err
		}
		opened, err := artifact.OpenFileWithLocations(ctx, path, ref, opts.ManifestCfg, opts.CustomerKeyFn,
			opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return nil, err
		}
		root, err := snapshotfile.Open(ctx, opened)
		if err != nil {
			return nil, err
		}
		return root.Memory, nil
	}
}

func resolvedBindingMergeRef(raw, relativeDir string, locations config.RefLocations) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil || ref.Scheme != manifest.RefSchemeFile || ref.Digest == "" || ref.DigestScheme == "manifest" {
		return "", fmt.Errorf("scheme-qualified local file identity required")
	}
	path, err := locations.ResolveFile(ref, relativeDir)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	ref.Path = path
	ref.Location = ""
	if err := ref.Validate(); err != nil {
		return "", err
	}
	return ref.String(), nil
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

// canonicalizeConfiguredTarRefs replaces every local immutable disk ref in the
// live config with the actual policy-normalized identity returned by its
// artifact. Paths and named locations are preserved for later opens. The
// physical carrier determines whether the resulting identity is @digest,
// @hmac, or a Bundle @manifest selector.
func canonicalizeConfiguredTarRefs(cfg *config.SandboxConfig, locations config.RefLocations, codec tarstream.Codec, required bool) error {
	return canonicalizeConfiguredTarRefsWithOpener(context.Background(), cfg, locations, codec, required, nil)
}

func canonicalizeConfiguredTarRefsWithOpener(ctx context.Context, cfg *config.SandboxConfig, locations config.RefLocations, codec tarstream.Codec, required bool, opener FileStreamOpener) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("sandbox config is nil")
	}
	normalize := func(field string, target *string) error {
		if target == nil || *target == "" {
			return nil
		}
		ref, err := canonicalConfiguredTarRefWithOpener(ctx, *target, locations, codec, required, opener)
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
	return canonicalConfiguredTarRefWithOpener(context.Background(), raw, locations, codec, required, nil)
}

func canonicalConfiguredTarRefWithOpener(ctx context.Context, raw string, locations config.RefLocations, codec tarstream.Codec, required bool, opener FileStreamOpener) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", protectLocalArtifactError(codec, "parse local artifact ref", err)
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		return ref.String(), nil
	}
	stream, _, err := OpenDiskStreamAtWithOpener(ctx, raw, nil, locations, "", codec, required, opener)
	if err != nil {
		return "", err
	}
	if selected, ok := stream.(interface {
		RootManifestKey() (store.ContentKey, bool)
	}); ok {
		if key, isBundle := selected.RootManifestKey(); isBundle {
			if err := stream.Close(); err != nil {
				return "", err
			}
			ref.DigestScheme, ref.Digest = "manifest", manifest.HexKey(key)
			if err := ref.Validate(); err != nil {
				return "", fmt.Errorf("invalid canonical local Bundle ref: %w", err)
			}
			return ref.String(), nil
		}
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
	return buildDiskRefWithOpener(uri, locations, codec, required, nil)
}

func buildDiskRefWithOpener(uri string, locations config.RefLocations, codec tarstream.Codec, required bool, opener FileStreamOpener) (string, error) {
	ref, err := manifest.ParseRef(uri)
	if err != nil {
		return "", err
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		return ref.String(), nil
	}
	stream, _, err := OpenDiskStreamAtWithOpener(context.Background(), uri, nil, locations, "", codec, required, opener)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	if selected, ok := stream.(interface {
		RootManifestKey() (store.ContentKey, bool)
	}); ok {
		if key, isBundle := selected.RootManifestKey(); isBundle {
			ref.Path = filepath.Base(ref.Path)
			ref.DigestScheme, ref.Digest = "manifest", manifest.HexKey(key)
			if err := ref.Validate(); err != nil {
				return "", fmt.Errorf("file artifact ref: %w", err)
			}
			return ref.String(), nil
		}
	}
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

func validateSandboxID(sandboxID string) error {
	if sandboxID == "" || sandboxID == "." || sandboxID == ".." ||
		filepath.Base(sandboxID) != sandboxID || strings.ContainsAny(sandboxID, `/\`) {
		return fmt.Errorf("sandbox id %q must be one non-empty path component", sandboxID)
	}
	return nil
}

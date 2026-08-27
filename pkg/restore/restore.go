// Package restore implements the `sandbox-ctl run --restore=` lifecycle. It
// opens Snapshot S, follows snapshot.cfg.sandbox_ref to Sandbox E for portable
// workload and disk provenance, prepares memfd + va_report, and starts patched
// CH from S's config.json/state.json so memory faults can flow.
package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/chmemory"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/chapi"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
	"github.com/kuasar-sandbox/sandboxer/pkg/tapfd"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

// Options is the restore-specific input.
//
// Snapshot can be supplied either as a local file path or as a
// manifest:// URI. When a manifest:// URI is given, Fetcher must
// be non-nil and the bundle is read via fetch.Fetcher (chunk-granular,
// cache-ctl backed); the uffd source becomes ManifestSnapshotSource
// instead of SparseSnapshotSource.
//
// Disk refs in the referenced Sandbox E likewise support manifest:// when
// Fetcher is set. Parent .sandbox refs are always narrowed to their Payload.
type Options struct {
	SnapshotPath        string                 // file path; mutually exclusive with SnapshotManifestKey
	SnapshotManifestKey string                 // hex content key; mutually exclusive with SnapshotPath
	SnapshotRef         string                 // canonical portable root ref; empty for a node-local file
	HostCfg             *config.SandboxConfig  // host yaml: TAP, blk1.diff, etc.
	HostPresence        config.FieldPresence   // explicit host YAML fields for restore ownership checks
	ManifestCfg         *config.ManifestConfig // for snapshot --upload from a restored sandbox
	Fetcher             fetch.Fetcher          // required when any URI is manifest://; caller owns lifecycle
	CustomerKeyFn       ingest.CustomerKeyFunc // process-fixed key used by later snapshot upload
	LocalCodec          tarstream.Codec        // nil when crypto.local=off
	LocalRequired       bool                   // reject plaintext local artifacts and active diffs
	RefLocations        config.RefLocations    // trusted named file locations
	ArtifactRelativeDir string                 // trusted directory of the referenced Sandbox E
	SandboxID           string
	CHBinary            string
	RuntimeRoot         string        // tmpfs run root; "/run/sandbox" by default
	BaseRoot            string        // on-disk base root (fresh overlay diff); "/var/lib/sandbox" by default
	StatsJSONPath       string        // if non-empty, dump uffd + per-backend stats here on exit
	StatsInterval       time.Duration // if > 0, periodically log lazy-load stats; 0 = off
	StdioMode           stdio.Mode    // CH process stdio wiring; see pkg/stdio

	// PingFatalThreshold: same semantics as sandbox.RunOptions —
	// SIGTERM CH after N consecutive ping failures. 0 disables.
	PingFatalThreshold int

	// Forwards are parsed `--connect` port-forward directives — a restored
	// sandbox re-opens the same host-local listeners (forward.go). Empty →
	// no port forwarding.
	Forwards []sandbox.ForwardSpec

	// NotifyReadiness receives the one-shot startup milestones for this run.
	// nil preserves the historical behavior exactly.
	NotifyReadiness sandbox.ReadinessNotify
}

// Run executes restore. Returns the CH exit code.
func Run(ctx context.Context, opts Options) (int, error) {
	if opts.SnapshotPath == "" && opts.SnapshotManifestKey == "" {
		return -1, errors.New("restore: SnapshotPath or SnapshotManifestKey required")
	}
	if opts.SnapshotPath != "" && opts.SnapshotManifestKey != "" {
		return -1, errors.New("restore: SnapshotPath and SnapshotManifestKey are mutually exclusive")
	}
	if opts.SnapshotManifestKey != "" && opts.Fetcher == nil {
		return -1, errors.New("restore: manifest:// snapshot requires Fetcher")
	}
	if opts.HostCfg == nil {
		return -1, errors.New("restore: HostCfg required")
	}
	if opts.LocalRequired && opts.LocalCodec == nil {
		return -1, errors.New("restore: LocalRequired requires LocalCodec")
	}
	if opts.LocalCodec != nil && opts.CustomerKeyFn == nil {
		return -1, errors.New("restore: LocalCodec requires CustomerKeyFn")
	}
	if err := opts.HostCfg.ValidateRestoreHostConfigWithPresence(opts.HostPresence); err != nil {
		return -1, err
	}
	var diffCustomerKey [32]byte
	if opts.LocalCodec != nil {
		var err error
		diffCustomerKey, err = opts.CustomerKeyFn()
		if err != nil {
			return -1, fmt.Errorf("restore: diff customer key: %w", err)
		}
		defer clear(diffCustomerKey[:])
	}
	prefetchMode, err := config.ParsePrefetchMode(opts.HostCfg.Restore.Prefetch)
	if err != nil {
		return -1, err
	}
	if opts.SandboxID == "" {
		opts.SandboxID = "rs-default"
	}
	if opts.RuntimeRoot == "" {
		opts.RuntimeRoot = "/run/sandbox"
	}
	if opts.BaseRoot == "" {
		opts.BaseRoot = sandbox.DefaultBaseRoot
	}
	if opts.CHBinary == "" {
		opts.CHBinary = "cloud-hypervisor"
	}
	root, err := openRootSnapshot(ctx, opts)
	if err != nil {
		return -1, fmt.Errorf("open snapshot: %w", err)
	}
	opts = root.opts
	snapshotRoot, err := snapshotfile.Open(ctx, root.stream)
	if err != nil {
		return -1, fmt.Errorf("open Snapshot S: %w", err)
	}
	defer snapshotRoot.Close()
	memoryConfig, err := snapshot.ParseConfig(snapshotRoot.SnapshotConfig)
	if err != nil {
		return -1, fmt.Errorf("unsupported snapshot format/version: %w", err)
	}
	canonicalSnapshotConfig, err := snapshot.MarshalConfig(memoryConfig)
	if err != nil {
		return -1, err
	}
	if string(canonicalSnapshotConfig) != string(snapshotRoot.SnapshotConfig) {
		return -1, errors.New("snapshot.cfg is not canonically encoded")
	}

	sandboxSource, err := openReferencedSandbox(ctx, memoryConfig.SandboxRef, opts)
	if err != nil {
		return -1, fmt.Errorf("open snapshot.cfg sandbox_ref: %w", err)
	}
	defer sandboxSource.Root.Close()
	opts = sandboxSource.opts
	opts.ArtifactRelativeDir = sandboxSource.RelativeDir
	applyDefaultRestoreArtifactBindings(opts.HostCfg, sandboxSource.Root.Portable, sandboxSource.RelativeDir)
	merged, c0, err := config.ApplyRestoreRules(sandboxSource.Root.Portable, opts.HostCfg, opts.HostPresence)
	if err != nil {
		return -1, err
	}
	snapCfg := *merged
	identities, err := sandbox.ResolvePortableProjection(&snapCfg)
	if err != nil {
		return -1, err
	}
	if c0.Boot.Kernel != identities.KernelRef {
		return -1, fmt.Errorf("boot.kernel identity mismatch: portable %s, host %s", c0.Boot.Kernel, identities.KernelRef)
	}
	if c0.Boot.Runtime != identities.RuntimeRef {
		return -1, fmt.Errorf("boot.runtime identity mismatch: portable %s, host %s", c0.Boot.Runtime, identities.RuntimeRef)
	}
	if err := config.BindPortableDiskGraph(&snapCfg, sandboxSource.RuntimeRef, sandboxSource.RelativeDir); err != nil {
		return -1, fmt.Errorf("Sandbox source binding: %w", err)
	}
	if err := preflightRestoreDiskGraph(ctx, &snapCfg, opts, sandboxSource.RelativeDir, diffCustomerKey); err != nil {
		return -1, fmt.Errorf("restore disk graph preflight: %w", err)
	}

	logf := func(format string, a ...any) { log.Printf("[sandbox-ctl run --restore] "+format, a...) }
	startUnixNs := time.Now().UnixNano()
	runtimeMemoryRef := root.selfRef
	selfRef := runtimeMemoryRef
	if root.bundleRoot != "" {
		selfRef = "manifest://" + root.bundleRoot
	}
	var bundleSource *sandbox.BundleSourceBinding
	if root.bundleReader != nil {
		physicalRef, physicalPath, sourceErr := fileBundleSource(opts.SnapshotPath, opts.SnapshotRef)
		if sourceErr != nil {
			return -1, fmt.Errorf("Bundle source provenance: %w", sourceErr)
		}
		bundleSource = &sandbox.BundleSourceBinding{
			RootRef: physicalRef, RootPath: physicalPath, Refs: root.bundleReader.Refs(),
		}
	}
	memoryBinding := &sandbox.MemorySourceBinding{
		SnapshotRef: selfRef, RuntimeRef: runtimeMemoryRef,
		FromRefs: append([]string(nil), memoryConfig.FromRefs...), BundleSource: bundleSource,
	}
	if opts.SnapshotPath != "" {
		memoryBinding.RelativeDir = filepath.Dir(opts.SnapshotPath)
	}

	runDir := filepath.Join(opts.RuntimeRoot, opts.SandboxID)
	chSock := filepath.Join(runDir, "ch.sock")
	uffdSock := filepath.Join(runDir, "uffd.sock")
	stateDir := filepath.Join(runDir, "snap-state")

	// UFFD sees only S's memory prefix. The strict reader derives this section
	// from ZIP geometry, so config/state bytes can never be exposed as RAM.
	selfStream := snapshotRoot.Memory
	totalSize := root.size
	if root.bundleRoot != "" {
		logf("local Manifest Bundle: root=%s bundle_size=%d", root.bundleRoot, totalSize)
	} else if opts.SnapshotManifestKey != "" {
		logf("manifest snapshot: key=%s bundle_size=%d", opts.SnapshotManifestKey, totalSize)
	}

	snapshotHasNetwork, err := snapshotConfigHasNetwork(snapshotRoot.ConfigJSON)
	if err != nil {
		return -1, err
	}
	if snapshotHasNetwork != c0.Network.Enabled {
		return -1, fmt.Errorf("snapshot CH network topology=%t conflicts with Sandbox network.enabled=%t", snapshotHasNetwork, c0.Network.Enabled)
	}
	hostHasNetwork := snapCfg.Network.TAP != "" || snapCfg.Network.TapFD != nil
	if err := validateRestoreNetworkTopology(c0.Network.Enabled, hostHasNetwork); err != nil {
		return -1, err
	}

	// Device sockets in CH --disk order: root (1 single / 2 overlay) + each data
	// disk (1 / 2), as blk0.sock, blk1.sock, … — matching ServeAndWait's layout.
	nDev := 1
	if !snapCfg.SingleDisk() {
		nDev = 2
	}
	for i := range snapCfg.Boot.Disks {
		if snapCfg.Boot.Disks[i].Single() {
			nDev++
		} else {
			nDev += 2
		}
	}
	diskSocks := make([]string, nDev)
	for i := range diskSocks {
		diskSocks[i] = filepath.Join(runDir, fmt.Sprintf("blk%d.sock", i))
	}

	// CH snapshot config is the Capacity authority. The referenced Sandbox E
	// portable config must describe the same exact byte domain, but it is not
	// used to infer CH total size.
	snapCap, err := chmemory.CapacityFromVMConfig(snapshotRoot.ConfigJSON)
	if err != nil {
		return -1, fmt.Errorf("snapshot CH capacity: %w", err)
	}
	declaredCap, err := snapCfg.CapacityMemoryBytes()
	if err != nil {
		return -1, fmt.Errorf("referenced Sandbox capacity: %w", err)
	}
	if declaredCap != snapCap {
		return -1, fmt.Errorf("snapshot Capacity mismatch: CH memory zones=%d Sandbox=%d", snapCap, declaredCap)
	}
	if snapshotRoot.ArchiveBase != snapCap {
		return -1, fmt.Errorf("snapshot memory payload size=%d does not match CH capacity=%d", snapshotRoot.ArchiveBase, snapCap)
	}
	if snapCap > uint64(^uint(0)>>1) {
		return -1, fmt.Errorf("restore Capacity %d exceeds host addressable memory size", snapCap)
	}
	balTarget, balCurrent, balOk, err := parseBalloonFromState(snapshotRoot.StateJSON)
	if err != nil {
		return -1, fmt.Errorf("parse balloon from state.json: %w", err)
	}
	if balOk {
		if err := resctl.ValidateBalloonSize(snapCap, balTarget); err != nil {
			return -1, fmt.Errorf("snapshot balloon target: %w", err)
		}
		if err := resctl.ValidateBalloonSize(snapCap, balCurrent); err != nil {
			return -1, fmt.Errorf("snapshot balloon current: %w", err)
		}
	}
	settledHeadroom, err := snapCfg.AllocatableMemoryBytes()
	if err != nil {
		return -1, fmt.Errorf("restore settled headroom: %w", err)
	}
	if err := validateRestoreBalloonControl(snapCap, settledHeadroom, balOk); err != nil {
		return -1, err
	}
	budgetAtSnapshot := deriveBudgetAtSnapshot(snapCap, balTarget, balCurrent, balOk)
	if err := validateBudgetAtSnapshot(snapCap, budgetAtSnapshot); err != nil {
		return -1, err
	}
	safeTarget := uint64(0)
	if balOk {
		safeTarget = min(balTarget, balCurrent)
	}

	// Preserve both snapshot sides. SafeTarget normalization is deliberately
	// deferred until restore ACK and MUX establishment.
	var balloonCtl *resctl.BalloonController
	if balOk {
		balloonCtl = resctl.NewBalloonController(chSock, snapCap, snapCfg.CHApiDeadline(), logf)
		if err := balloonCtl.SeedRestoredState(balTarget, balCurrent); err != nil {
			return -1, err
		}
	}

	// Rewrite and validate CH's captured paths before cgroup/controller/network
	// side effects. Only after every deterministic format/topology/capacity check
	// succeeds do we create the run directory and persist immutable C0.
	vsockSock := filepath.Join(runDir, "vsock.sock")
	rewritten, err := rewriteConfigPaths(snapshotRoot.ConfigJSON, pathRewrite{
		UffdSocket: uffdSock,
		DiskSocks:  diskSocks,
		APISock:    chSock,
		VsockSock:  vsockSock,
	})
	if err != nil {
		return -1, fmt.Errorf("rewrite config.json: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return -1, err
	}
	defer os.RemoveAll(runDir)
	if _, err := config.WritePortableSandboxConfig(runDir, c0); err != nil {
		return -1, fmt.Errorf("write immutable C0: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), snapshotRoot.StateJSON, 0o644); err != nil {
		return -1, err
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.json"), rewritten, 0o644); err != nil {
		return -1, err
	}

	// Cgroup setup must use the resolved snapshot Capacity, not a host-only
	// preflight value. memory.high remains max until the first trusted report.
	cg, err := resctl.SetupCgroupForConfig(&snapCfg)
	if err != nil {
		return -1, fmt.Errorf("cgroup: %w", err)
	}
	if cg.Active() {
		logf("cgroup limits set: %s (CH starts in cgroup; sandbox-ctl stays out)", cg.Path)
	}
	defer func() { _ = cg.Cleanup() }()

	// Publish the immutable lifecycle lease only when every snapshot field
	// needed by Admit is available. A stalled manifest fetch must not appear
	// to restart inventory as a live, full-capacity sandbox that cannot yet
	// StateSync because it has never been admitted.
	hooks, err := resctl.NewControllerHooks(resctl.ControllerHookOptions{
		SocketPath: opts.HostCfg.Resources.Control.Controller,
		CgroupPath: cg.LocalPath(),
		SandboxID:  opts.SandboxID,
		Context:    sandbox.ControllerWorkContext(ctx),
		Logf:       logf,
	}, &snapCfg)
	if err != nil {
		return -1, fmt.Errorf("controller dial: %w", err)
	}
	defer hooks.Release("normal")
	initialBudget, err := hooks.Admit(opts.SandboxID, budgetAtSnapshot)
	if err != nil {
		return -1, fmt.Errorf("controller admit: %w", err)
	}
	if initialBudget != budgetAtSnapshot {
		return -1, fmt.Errorf("restore initial Budget=%d, require BudgetAtSnapshot=%d", initialBudget, budgetAtSnapshot)
	}
	logf("restore BudgetAtSnapshot reserved=%d (balloon target/current=%d/%d)",
		budgetAtSnapshot, balTarget, balCurrent)
	memoryCtl, err := resctl.NewMemoryController(resctl.MemoryControllerOptions{
		Config: &snapCfg, CgroupPath: cg.LocalPath(), Balloon: balloonCtl,
		Reservation: hooks, InitialBudget: initialBudget, Logf: logf,
	})
	if err != nil {
		return -1, fmt.Errorf("memory controller: %w", err)
	}
	// The CH-authoritative Capacity also sizes the memfd and UFFD source.
	capBytes := snapCap

	// Build the layered memory source: [self bundle] ++ from_refs (§3.5). A
	// non-resident page (hole) in an upper layer falls through to a lower
	// layer; a page hole in every layer (merged hole) → ZEROPAGE. Single layer
	// (no from_refs) degenerates to today's behaviour. The from_refs streams
	// live until the run exits (closed below); selfStream is closed at open.
	memLayers := []fetch.Stream{selfStream}
	for i, ref := range memoryConfig.FromRefs {
		s, err := openMemorySnapshotRef(ctx, ref, opts)
		if err != nil {
			return -1, fmt.Errorf("from_refs[%d]: %w", i, err)
		}
		if s.Size() != capBytes {
			_ = s.Close()
			return -1, fmt.Errorf("from_refs[%d] memory size=%d does not match capacity=%d", i, s.Size(), capBytes)
		}
		defer s.Close()
		memLayers = append(memLayers, s)
	}
	source, err := uffd.NewStreamSnapshotSource(fetch.NewLayered(memLayers...), capBytes)
	if err != nil {
		return -1, fmt.Errorf("snapshot source: %w", err)
	}
	logf("snapshot source: %d memory layer(s)", len(memLayers))
	prefetch := startMemoryPrefetch(ctx, prefetchMode, opts.SnapshotManifestKey, root.stream, len(memLayers)-1, logf)
	// Close is a lifetime boundary for fetch.Stream. Register this after every
	// memory-layer Close defer so cancellation and join always run first.
	defer prefetch.Stop()

	// Reconstruct each logical disk (root + data disks, in order): layer the
	// captured base ([top] ++ base_from_refs) into a ro base, build a fresh
	// writable CoW on top, and (overlay mode) open the erofs base as the ro
	// device. All disk provenance comes from E/C0; S contributes no disk fields.
	rootTop, rootChain := snapCfg.Boot.Root.Base, append([]string(nil), snapCfg.Boot.Root.BaseFromRefs...)
	if !snapCfg.SingleDisk() {
		rootTop = snapCfg.Boot.Root.Overlay.Base
		rootChain = append([]string(nil), snapCfg.Boot.Root.Overlay.BaseFromRefs...)
	}
	rootDiffURI, rootDiffTmpl := snapCfg.Boot.Root.Diff, snapCfg.Boot.Root.DiffTemplate
	if !snapCfg.SingleDisk() {
		rootDiffURI, rootDiffTmpl = snapCfg.Boot.Root.Overlay.Diff, snapCfg.Boot.Root.Overlay.DiffTemplate
	}
	rootDiffSize, err := snapCfg.DiffSizeBytes()
	if err != nil {
		return -1, err
	}
	rootDB, rootCleanup, err := reconstructDisk(ctx, opts, diffCustomerKey, snapCfg.SingleDisk(), rootTop, rootChain,
		snapCfg.Boot.Root.Base, rootDiffURI, rootDiffTmpl, rootDiffSize, "overlay", logf)
	if err != nil {
		return -1, err
	}
	defer rootCleanup()
	disks := []sandbox.DiskBackend{rootDB}

	for i := range snapCfg.Boot.Disks {
		d := &snapCfg.Boot.Disks[i]
		single := d.Single()
		top, chain := d.Base, append([]string(nil), d.BaseFromRefs...)
		diffURI, diffTmpl := d.Diff, d.DiffTemplate
		if !single {
			top, chain = d.Overlay.Base, append([]string(nil), d.Overlay.BaseFromRefs...)
			diffURI, diffTmpl = d.Overlay.Diff, d.Overlay.DiffTemplate
		}
		dsz, err := d.RootConfig.DiffSizeBytes(fmt.Sprintf("boot.disks[%d]", i))
		if err != nil {
			return -1, err
		}
		db, dcleanup, derr := reconstructDisk(ctx, opts, diffCustomerKey, single, top, chain, d.Base, diffURI, diffTmpl, dsz, fmt.Sprintf("disk%d", i), logf)
		if derr != nil {
			return -1, derr
		}
		defer dcleanup()
		disks = append(disks, db)
	}

	// Network: when the snapshot has a virtio-net device, re-acquire its host
	// side for this restore. tapfd mode re-runs
	// the configured handoff (docs/tapfd.md §4, idempotent) for a fresh queue
	// fd, passed to CH via --restore net_fds; tap-name mode lets CH reopen the
	// named tap from the restored config. The merged metadata also yields the
	// NetworkSpec the guest re-applies flush-and-replace (clone takes a fresh
	// L3 identity; the MAC stays the snapshot's, so the provider must use a
	// stable per-port MAC — see docs/tapfd.md §5).
	var tapFile, netnsFile *os.File
	var metaMAC, metaIP string
	if snapCfg.Network.TapFD != nil {
		f, nsf, meta, err := tapfd.AcquireConfig(ctx, snapCfg.Network.TapFD)
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
		logf("tapfd: received tap fd for restore (mac=%s ip=%s netns=%t)", meta.MAC, meta.IP, netnsFile != nil)
	}
	netMAC, netSpec := snapCfg.Network.Effective(metaMAC, metaIP)

	// The shared back-half (memfd, uffd va_report handler, vhost-blk
	// backends, the launch server — incl. the guest→host mem_report /
	// app_exited channel that was missing on the restore path — pinger,
	// ctl.sock, signal escalation, stats) lives in sandbox.ServeAndWait.
	// Restore supplies: a snapshot uffd Source (not ZeroSource), a
	// placeholder launch spec (the guest does NOT re-hello after a
	// restore, so WireLaunchMUX=false — the stdio MUX is re-established
	// by PostSpawn over the reverse channel), and a settle protocol of
	// waitAPI → /vm.resume → restore{epoch} → local restore normalization.
	return sandbox.ServeAndWait(sandbox.VMParams{
		Ctx:                ctx,
		SandboxID:          opts.SandboxID,
		RunDir:             runDir,
		Logf:               logf,
		StdioMode:          opts.StdioMode,
		PingFatalThreshold: opts.PingFatalThreshold,
		StartUnixNs:        startUnixNs,
		StatsJSONPath:      opts.StatsJSONPath,
		StatsInterval:      opts.StatsInterval,
		VAReportDeadline:   opts.HostCfg.VAReportDeadline(),
		PingTimeout:        opts.HostCfg.PingDeadline(),
		AppNotifyDeadline:  opts.HostCfg.AppNotifyDeadline(),

		CapBytes:   int64(capBytes),
		UffdSource: source,
		Disks:      disks,

		LaunchSpec:    &proto.LaunchSpec{},
		WireLaunchMUX: false,
		Hooks:         hooks,
		Memory:        memoryCtl,

		TapFile:   tapFile, // nil in tap-name/no-network modes; non-nil tapfd is inherited at fd 4
		NetMAC:    netMAC,
		NetnsFile: netnsFile, // non-nil → launch CH inside the tap's netns

		SnapCfg:        &snapCfg,
		PortableConfig: c0,
		SourceBinding: &sandbox.RunSourceBinding{
			SandboxRef: sandboxSource.PortableRef, RuntimeRef: sandboxSource.RuntimeRef,
			RelativeDir:  sandboxSource.RelativeDir,
			BundleSource: firstBundleSource(sandboxSource.bundleSource, bundleSource),
		},
		MemoryBinding:   memoryBinding,
		ManifestCfg:     opts.ManifestCfg,
		Fetcher:         opts.Fetcher,
		BundleReader:    firstBundleReader(sandboxSource.bundleReader, root.bundleReader),
		BundleFetcher:   firstBundleFetcher(sandboxSource.bundleFetcher, root.bundleFetcher),
		RefLocations:    opts.RefLocations,
		CustomerKeyFn:   opts.CustomerKeyFn,
		LocalCodec:      opts.LocalCodec,
		LocalRequired:   opts.LocalRequired,
		Forwards:        opts.Forwards,
		Cgroup:          cg,
		NotifyReadiness: opts.NotifyReadiness,

		BuildCmd: func(e sandbox.CmdEnv) (*exec.Cmd, func(), error) {
			// CH 51 `--restore source_url=file://<dir>` replaces
			// --kernel/--vsock; --console/--serial are restored from the
			// snapshot bundle (taken with `--console tty --serial off`),
			// so we don't repeat them. consoleArg is unused here.
			cmd := exec.CommandContext(sandbox.VMLifecycleContext(ctx), opts.CHBinary)
			_, cleanup, err := opts.StdioMode.SetupCHStdio(cmd)
			if err != nil {
				return nil, nil, fmt.Errorf("stdio: %w", err)
			}
			restoreArg := "source_url=file://" + stateDir
			if e.TapFDNum > 0 {
				// CH can't serialize fds, so the snapshot's net fd is dead;
				// re-bind the fresh tap queue fd (CH fd 4) to the restored net
				// device named _net0 at cold boot via net_fds.
				// net_fds is a CH Tuple<String,Vec<u64>>: the whole value is
				// bracket-wrapped, each entry is <net-id>@<fd-list>. Single
				// net _net0 with one fd → [_net0@[N]].
				restoreArg += fmt.Sprintf(",net_fds=[_net0@[%d]]", e.TapFDNum)
			}
			cmd.Args = append(cmd.Args, "--api-socket", e.CHSock, "--restore", restoreArg)
			logf("spawning %s --api-socket %s --restore %s", opts.CHBinary, e.CHSock, restoreArg)
			return cmd, cleanup, nil
		},

		// Restore settle (docs/sandbox.md §7 T14-T15): wait for CH's
		// API, /vm.resume to release the vCPUs from the snapshot point,
		// then notify the guest (restore{epoch=1}) and turn that
		// reverse-channel conn into the stdio MUX. Synchronous — a
		// non-nil return aborts the run (ServeAndWait kills CH); we
		// don't hand back a sandbox whose guest agent is unreachable.
		PostSpawn: func(pc sandbox.PostSpawnCtx) error {
			if err := chapi.WaitReady(pc.Ctx, pc.CHSock, opts.HostCfg.APIReadyDeadline()); err != nil {
				return fmt.Errorf("ch api not ready: %w", err)
			}
			if err := (chapi.Client{Sock: pc.CHSock, RespDeadline: opts.HostCfg.CHApiDeadline()}).ResumeContext(pc.Ctx); err != nil {
				return fmt.Errorf("vm.resume: %w", err)
			}
			pc.Logf("VM resumed, vCPU running")

			tRestore := time.Now()
			// 0 = no forced timeout: DialRaw needs a finite value, so fall back
			// to noForcedTimeout (effective-infinity; cancellation still flows
			// via pc.Ctx → CH teardown closing the vsock conn).
			restoreDeadline := opts.HostCfg.RestoreDeadline()
			if restoreDeadline <= 0 {
				restoreDeadline = config.NoForcedTimeout
			}
			muxSpec, err := openAndEstablishRestoreMUX(func() (net.Conn, proto.StdioSpec, error) {
				return guestlink.OpenMUXViaRestoreContext(pc.Ctx, pc.Pinger.Client, 1, netSpec, restoreDeadline)
			}, pc.EstablishMUX, pc.NotifyReady)
			if err != nil {
				return err
			}
			pc.Logf("restore notify acked in %dµs (stdio MUX re-established: tty=%v); starting ping ticker",
				time.Since(tRestore).Microseconds(), muxSpec.TTY)
			pc.Pinger.Start(pc.Ctx)
			// Settled is only the node reservation lifecycle fact. Publish it as
			// soon as ACK + MUX establishes the restored sandbox; it must not be
			// delayed by, or made conditional on, sandbox-local CH normalization.
			if pc.Hooks != nil {
				if err := pc.Hooks.Settled(); err != nil {
					pc.Logf("settled: %v (continuing)", err)
				}
				if pc.Hooks.Enabled() {
					pc.Hooks.StartHeartbeat(pc.Ctx, 5*time.Second)
				}
			}
			// ACK + MUX is the local safety boundary. SafeTarget normalization
			// is separate from steady policy and reports remain closed until it
			// is confirmed. Failure is retained for forward retry.
			if pc.Memory != nil {
				if err := pc.Memory.StartRestore(pc.Ctx, safeTarget); err != nil {
					pc.Logf("restore memory normalization: %v (retrying)", err)
				}
				pc.Memory.StartSensor(pc.Ctx)
			}
			return nil
		},
	})
}

// openAndEstablishRestoreMUX is the restore readiness barrier after CH API
// readiness and /vm.resume: OpenMUXViaRestore returns only after restore_ack,
// then the host MUX must be established before ready is emitted. The pinger,
// balloon, settled hook, heartbeat, and sensor deliberately remain outside.
func openAndEstablishRestoreMUX(
	open func() (net.Conn, proto.StdioSpec, error),
	establish func(net.Conn, proto.StdioSpec) error,
	notifyReady func(),
) (proto.StdioSpec, error) {
	conn, spec, err := open()
	if err != nil {
		return proto.StdioSpec{}, fmt.Errorf("notify restore: %w (guest agent unreachable)", err)
	}
	if err := establish(conn, spec); err != nil {
		// EstablishMUX did not take ownership on failure.
		_ = conn.Close()
		return proto.StdioSpec{}, fmt.Errorf("stdio MUX bridge: %w", err)
	}
	if notifyReady != nil {
		notifyReady()
	}
	return spec, nil
}

// reconstructDisk rebuilds one logical disk for restore: the read-only base is
// [captured top] ++ chain (§3.5) layered into a Stream; a FRESH writable CoW is
// built on top (single mode: this IS the disk; overlay mode: the ext4 upper).
// In overlay mode the erofs base (erofsBaseURI) is opened as the ro device.
// capturedTop/chain come from Sandbox E's portable disk graph;
// diffURI/diffTemplate come from the restore host binding (or the auto-default
// <sid>.<diskKey>.diff).
// The returned cleanup closes the readers/CoW and removes an auto-created diff.
func reconstructDisk(ctx context.Context, opts Options, diffCustomerKey [32]byte, single bool, capturedTop string, chain []string, erofsBaseURI, diffURI, diffTemplate string, diffSize int64, diskKey string, logf func(string, ...any)) (sandbox.DiskBackend, func(), error) {
	var db sandbox.DiskBackend
	var closers []func()
	cleanup := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	fail := func(e error) (sandbox.DiskBackend, func(), error) { cleanup(); return sandbox.DiskBackend{}, nil, e }
	db.Overlay = !single

	// Layered ro base: [captured top] ++ chain.
	logf("disk %s image: opening captured top", diskKey)
	top, err := openDiskRefStream(ctx, capturedTop, opts)
	if err != nil {
		return fail(fmt.Errorf("%s: open base: %w", diskKey, err))
	}
	layers := []fetch.Stream{top}
	for i, ref := range chain {
		s, serr := openDiskRefStream(ctx, ref, opts)
		if serr != nil {
			return fail(fmt.Errorf("%s base_from_refs[%d]: %w", diskKey, i, serr))
		}
		closers = append(closers, func() { s.Close() })
		layers = append(layers, s)
	}
	baseStream := fetch.NewLayered(layers...)
	baseReader := vhost.NewStreamReader(ctx, baseStream, int64(baseStream.Size()))
	closers = append(closers, func() { baseReader.Close() })

	// Fresh writable diff. Empty URI → auto-default (ours to remove).
	if diffURI == "" {
		baseDir := sandbox.DefaultBaseDir(opts.BaseRoot, opts.SandboxID)
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			return fail(fmt.Errorf("%s: mkdir base dir: %w", diskKey, err))
		}
		p := filepath.Join(baseDir, fmt.Sprintf("%s.%s.diff", opts.SandboxID, diskKey))
		diffURI = "file://" + p
		db.OwnedDiff = true
		closers = append(closers, func() { _ = os.Remove(p) })
	}
	_, diffPath, ok := config.SchemeAndPath(diffURI)
	if !ok {
		return fail(fmt.Errorf("%s: bad diff uri: %s", diskKey, diffURI))
	}
	diffInit, err := sandbox.PrepareDiff(diffPath, diffTemplate, baseReader.Size(), diffSize)
	if err != nil {
		return fail(fmt.Errorf("%s: prepare diff: %w", diskKey, err))
	}
	var cowOptions []vhost.BlockCOWOption
	if opts.LocalCodec != nil {
		cowOptions = append(cowOptions, vhost.WithDiffEncryption(diffCustomerKey, opts.LocalRequired))
	}
	cow, err := vhost.OpenBlockCOW(diffPath, baseReader, diffInit, cowOptions...)
	if err != nil {
		return fail(fmt.Errorf("%s: open BlockCOW: %w", diskKey, err))
	}
	closers = append(closers, func() { cow.Close() })
	db.Cow, db.DiffPath = cow, diffPath

	// Overlay mode: the ro erofs base device.
	if !single {
		r, _, rerr := openEROFSBlockReader(ctx, erofsBaseURI, opts)
		if rerr != nil {
			return fail(fmt.Errorf("%s: open erofs base: %w", diskKey, rerr))
		}
		db.Reader, db.BasePath = r, erofsBaseURI
		closers = append(closers, func() { r.Close() })
	}
	return db, cleanup, nil
}

// openEROFSBlockReader exposes only the EROFS prefix of a flattened image or
// parent Sandbox. The optional config ZIP is host metadata and must never be
// visible to a vhost block backend.
func openEROFSBlockReader(ctx context.Context, raw string, opts Options) (vhost.BlockReader, int64, error) {
	opener := sandbox.FileStreamOpener(func(ctx context.Context, path string, ref manifest.Ref) (fetch.Stream, error) {
		opened, err := artifact.OpenFileWithLocations(ctx, path, ref, opts.ManifestCfg, opts.CustomerKeyFn,
			opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return nil, err
		}
		return opened, nil
	})
	return sandbox.OpenRootImageBlockReaderWithOpener(ctx, raw, opts.Fetcher, opts.RefLocations,
		opts.LocalCodec, opts.LocalRequired, opener)
}

// openDiskRefStream resolves only E's explicit immutable disk graph. A parent
// .sandbox is narrowed to Payload by sandbox.OpenDiskStreamAtWithOpener; its
// runtime config is never adopted recursively.
func openDiskRefStream(ctx context.Context, raw string, opts Options) (fetch.Stream, error) {
	opener := sandbox.FileStreamOpener(func(ctx context.Context, path string, ref manifest.Ref) (fetch.Stream, error) {
		opened, err := artifact.OpenFileWithLocations(ctx, path, ref, opts.ManifestCfg, opts.CustomerKeyFn,
			opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return nil, err
		}
		return opened, nil
	})
	stream, _, err := sandbox.OpenDiskStreamAtWithOpener(ctx, raw, opts.Fetcher, opts.RefLocations,
		opts.ArtifactRelativeDir, opts.LocalCodec, opts.LocalRequired, opener)
	return stream, err
}

// openMemorySnapshotRef opens one opaque parent S, validates its strict
// logical format, and returns only its memory prefix. Its own from_refs and
// sandbox_ref are deliberately not traversed here; the current S already
// carries the flattened ordered memory chain.
func openMemorySnapshotRef(ctx context.Context, raw string, opts Options) (fetch.Stream, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return nil, protectArtifactReadError(opts.LocalCodec, "parse snapshot memory ref", err)
	}
	var stream fetch.Stream
	switch ref.Scheme {
	case manifest.RefSchemeFile:
		relativeDir := ""
		if opts.SnapshotPath != "" {
			relativeDir = filepath.Dir(opts.SnapshotPath)
		}
		path, err := opts.RefLocations.ResolveFile(ref, relativeDir)
		if err != nil {
			return nil, err
		}
		opened, err := artifact.OpenFileWithLocations(ctx, path, ref, opts.ManifestCfg, opts.CustomerKeyFn,
			opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return nil, err
		}
		stream = opened
	case manifest.RefSchemeManifest:
		stream, _, err = sandbox.OpenManifestStream(ctx, ref.Path, opts.Fetcher)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported snapshot memory ref scheme %q", ref.Scheme)
	}
	root, err := snapshotfile.Open(ctx, stream)
	if err != nil {
		return nil, err
	}
	parsed, err := snapshot.ParseConfig(root.SnapshotConfig)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("unsupported snapshot format/version: %w", err)
	}
	canonical, err := snapshot.MarshalConfig(parsed)
	if err != nil || string(canonical) != string(root.SnapshotConfig) {
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("snapshot memory parent snapshot.cfg is not canonically encoded")
	}
	return root.Memory, nil
}

type openedRootSnapshot struct {
	stream        fetch.Stream
	size          int64
	selfRef       string
	bundleRoot    string
	bundleReader  *manifestbundle.Reader
	bundleFetcher *manifestbundle.ManifestFetcher
	opts          Options
}

type openedSandboxSource struct {
	Root          *sandboxfile.Root
	PortableRef   string
	RuntimeRef    string
	RelativeDir   string
	bundleReader  *manifestbundle.Reader
	bundleFetcher *manifestbundle.ManifestFetcher
	bundleSource  *sandbox.BundleSourceBinding
	opts          Options
}

func firstBundleReader(values ...*manifestbundle.Reader) *manifestbundle.Reader {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstBundleFetcher(values ...*manifestbundle.ManifestFetcher) *manifestbundle.ManifestFetcher {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstBundleSource(values ...*sandbox.BundleSourceBinding) *sandbox.BundleSourceBinding {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// openReferencedSandbox resolves S.snapshot_ref without treating it as a disk
// layer. The strict Sandbox reader exposes portable C0 and optional image
// config; later disk opens consume only its Payload through openDiskRefStream.
func openReferencedSandbox(ctx context.Context, raw string, opts Options) (*openedSandboxSource, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return nil, err
	}
	source := &openedSandboxSource{PortableRef: ref.String(), RuntimeRef: ref.String(), opts: opts}
	var stream fetch.Stream
	switch ref.Scheme {
	case manifest.RefSchemeManifest:
		stream, _, err = sandbox.OpenManifestStream(ctx, ref.Path, opts.Fetcher)
		if err != nil {
			return nil, err
		}
	case manifest.RefSchemeFile:
		relativeDir := ""
		if opts.SnapshotPath != "" {
			relativeDir = filepath.Dir(opts.SnapshotPath)
		}
		path, err := opts.RefLocations.ResolveFile(ref, relativeDir)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(path) {
			path, err = filepath.Abs(path)
			if err != nil {
				return nil, err
			}
		}
		opened, err := artifact.OpenFileWithLocations(ctx, path, ref, opts.ManifestCfg, opts.CustomerKeyFn,
			opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return nil, err
		}
		stream = opened
		source.RelativeDir = filepath.Dir(path)
		resolved := ref
		if resolved.Location == "" {
			resolved.Path = path
		}
		portable := ref
		if key, bundle := opened.RootManifestKey(); bundle {
			resolved.DigestScheme, resolved.Digest = "manifest", manifest.HexKey(key)
			portable.Path = filepath.Base(path)
			portable.DigestScheme, portable.Digest = "manifest", manifest.HexKey(key)
			source.opts.Fetcher = opened.ScopedFetcher()
			source.bundleReader = opened.BundleReader()
			source.bundleFetcher = opened.ManifestFetcher()
			physicalRef, physicalPath, sourceErr := fileBundleSource(path, ref.String())
			if sourceErr != nil {
				_ = opened.Close()
				return nil, sourceErr
			}
			source.bundleSource = &sandbox.BundleSourceBinding{
				RootRef: physicalRef, RootPath: physicalPath, Refs: opened.BundleReader().Refs(),
			}
		} else {
			scheme, digest := opened.Digest()
			if scheme == "" || digest == "" {
				_ = opened.Close()
				return nil, errors.New("local Sandbox has no declared content identity")
			}
			resolved.DigestScheme, resolved.Digest = scheme, digest
			portable.Path = digest + ".sandbox"
			portable.DigestScheme, portable.Digest = scheme, digest
		}
		source.PortableRef = portable.String()
		source.RuntimeRef = resolved.String()
	default:
		return nil, fmt.Errorf("unsupported Sandbox source scheme %q", ref.Scheme)
	}
	root, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		return nil, err
	}
	source.Root = root
	return source, nil
}

func applyDefaultRestoreArtifactBindings(host *config.SandboxConfig, portable *config.PortableSandboxConfig, relativeDir string) {
	if host == nil || portable == nil || relativeDir == "" {
		return
	}
	bind := func(current *string, raw string) {
		if *current != "" {
			return
		}
		ref, err := manifest.ParseRef(raw)
		if err != nil || ref.Scheme != manifest.RefSchemeFile {
			return
		}
		*current = "file://" + filepath.Join(relativeDir, ref.Path)
	}
	bind(&host.Boot.Kernel, portable.Boot.Kernel)
	bind(&host.Boot.Runtime, portable.Boot.Runtime)
}

func preflightRestoreDiskGraph(ctx context.Context, cfg *config.SandboxConfig, opts Options, relativeDir string, diffCustomerKey [32]byte) error {
	if cfg == nil {
		return errors.New("nil restored Sandbox config")
	}
	opts.ArtifactRelativeDir = relativeDir
	seen := make(map[string]uint64)
	seenImages := make(map[string]uint64)
	open := func(field, raw string) (uint64, error) {
		if raw == "" {
			return 0, fmt.Errorf("%s is empty", field)
		}
		if size, ok := seen[raw]; ok {
			return size, nil
		}
		stream, err := openDiskRefStream(ctx, raw, opts)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", field, err)
		}
		size := stream.Size()
		if closeErr := stream.Close(); closeErr != nil {
			return 0, fmt.Errorf("%s close: %w", field, closeErr)
		}
		seen[raw] = size
		return size, nil
	}
	openImage := func(field, raw string) (uint64, error) {
		if raw == "" {
			return 0, fmt.Errorf("%s is empty", field)
		}
		if size, ok := seenImages[raw]; ok {
			return size, nil
		}
		reader, size, err := openEROFSBlockReader(ctx, raw, opts)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", field, err)
		}
		if closeErr := reader.Close(); closeErr != nil {
			return 0, fmt.Errorf("%s close: %w", field, closeErr)
		}
		seenImages[raw] = uint64(size)
		return uint64(size), nil
	}
	checkLayers := func(field, top string, lowers []string) error {
		want, err := open(field, top)
		if err != nil {
			return err
		}
		for i, raw := range lowers {
			size, err := open(fmt.Sprintf("%s.base_from_refs[%d]", field, i), raw)
			if err != nil {
				return err
			}
			if size != want {
				return fmt.Errorf("%s layer size %d conflicts with top size %d", field, size, want)
			}
		}
		return nil
	}
	openLayers := func(field, top string, lowers []string) (vhost.BlockReader, error) {
		refs := append([]string{top}, lowers...)
		streams := make([]fetch.Stream, 0, len(refs))
		var logicalSize uint64
		fail := func(cause error) (vhost.BlockReader, error) {
			for _, stream := range streams {
				cause = errors.Join(cause, stream.Close())
			}
			return nil, cause
		}
		for i, raw := range refs {
			stream, err := openDiskRefStream(ctx, raw, opts)
			if err != nil {
				return fail(fmt.Errorf("%s layer[%d]: %w", field, i, err))
			}
			if i == 0 {
				logicalSize = stream.Size()
				if logicalSize > math.MaxInt64 {
					streams = append(streams, stream)
					return fail(fmt.Errorf("%s logical size %d exceeds host block-reader limit", field, logicalSize))
				}
			} else if stream.Size() != logicalSize {
				streams = append(streams, stream)
				return fail(fmt.Errorf("%s layer[%d] size %d conflicts with top size %d", field, i, stream.Size(), logicalSize))
			}
			streams = append(streams, stream)
		}
		layered := fetch.NewLayered(streams...)
		return vhost.NewStreamReader(ctx, layered, int64(logicalSize)), nil
	}
	validateWritable := func(field string, root *config.RootConfig, diskKey string) (retErr error) {
		top, lowers := root.Base, root.BaseFromRefs
		diffURI, templateURI := root.Diff, root.DiffTemplate
		if root.Overlay != nil {
			top, lowers = root.Overlay.Base, root.Overlay.BaseFromRefs
			diffURI, templateURI = root.Overlay.Diff, root.Overlay.DiffTemplate
		}
		base, err := openLayers(field, top, lowers)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, base.Close()) }()
		if diffURI == "" {
			diffURI = "file://" + filepath.Join(sandbox.DefaultBaseDir(opts.BaseRoot, opts.SandboxID),
				fmt.Sprintf("%s.%s.diff", opts.SandboxID, diskKey))
		}
		scheme, diffPath, ok := config.SchemeAndPath(diffURI)
		if !ok || scheme != "file" {
			return fmt.Errorf("%s writable diff invalid URI: %s", field, diffURI)
		}
		var cowOptions []vhost.BlockCOWOption
		if opts.LocalCodec != nil {
			cowOptions = append(cowOptions, vhost.WithDiffEncryption(diffCustomerKey, opts.LocalRequired))
		}
		info, statErr := os.Stat(diffPath)
		switch {
		case statErr == nil:
			if info.Size() == 0 {
				return fmt.Errorf("%s writable diff %s is empty; provide a formatted ext4 diff", field, diffPath)
			}
			if err := vhost.ValidateExistingDiffExt4(ctx, diffPath, base, cowOptions...); err != nil {
				return fmt.Errorf("%s writable diff: %w", field, err)
			}
		case !os.IsNotExist(statErr):
			return fmt.Errorf("%s writable diff stat: %w", field, statErr)
		case templateURI != "":
			templateScheme, templatePath, templateOK := config.SchemeAndPath(templateURI)
			if !templateOK || templateScheme != "file" {
				return fmt.Errorf("%s diff_template invalid URI: %s", field, templateURI)
			}
			if err := vhost.ValidateDiffTemplateExt4(ctx, templatePath, base, cowOptions...); err != nil {
				return fmt.Errorf("%s diff_template: %w", field, err)
			}
		default:
			if err := validateRestoreExt4Reader(ctx, base); err != nil {
				return fmt.Errorf("%s immutable ext4 layers: %w", field, err)
			}
		}
		return nil
	}
	checkRoot := func(field string, root *config.RootConfig) error {
		if root.Overlay == nil {
			return checkLayers(field, root.Base, root.BaseFromRefs)
		}
		if _, err := openImage(field+".base", root.Base); err != nil {
			return err
		}
		return checkLayers(field+".overlay", root.Overlay.Base, root.Overlay.BaseFromRefs)
	}
	if err := checkRoot("boot.root", &cfg.Boot.Root); err != nil {
		return err
	}
	if err := validateWritable("boot.root", &cfg.Boot.Root, "overlay"); err != nil {
		return err
	}
	for i := range cfg.Boot.Disks {
		if err := checkRoot(fmt.Sprintf("boot.disks[%d]", i), &cfg.Boot.Disks[i].RootConfig); err != nil {
			return err
		}
		if err := validateWritable(fmt.Sprintf("boot.disks[%d]", i), &cfg.Boot.Disks[i].RootConfig, fmt.Sprintf("disk%d", i)); err != nil {
			return err
		}
	}
	return nil
}

func validateRestoreExt4Reader(ctx context.Context, reader vhost.BlockReader) error {
	const magicOffset = int64(1024 + 0x38)
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader.Size() < magicOffset+2 {
		return errors.New("effective writable disk is too small for an ext4 superblock")
	}
	var magic [2]byte
	n, err := reader.ReadAt(magic[:], magicOffset)
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

// openRootSnapshot opens a root once before restore side effects. A local
// Manifest Bundle installs its Manifest-level scoped Fetcher into the returned
// Options and remains open for the complete sandbox lifetime.
func openRootSnapshot(ctx context.Context, opts Options) (*openedRootSnapshot, error) {
	if opts.SnapshotPath != "" {
		ref := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: opts.SnapshotPath}
		if opts.SnapshotRef != "" {
			parsed, err := manifest.ParseRef(opts.SnapshotRef)
			if err != nil {
				return nil, protectArtifactReadError(opts.LocalCodec, "parse snapshot ref", err)
			}
			if parsed.Scheme != manifest.RefSchemeFile {
				return nil, fmt.Errorf("snapshot path cannot use %s ref", parsed.Scheme)
			}
			if parsed.Location != "" {
				resolved, err := opts.RefLocations.ResolveFile(parsed, "")
				if err != nil {
					return nil, err
				}
				if filepath.Clean(resolved) != filepath.Clean(opts.SnapshotPath) {
					return nil, fmt.Errorf("located root ref does not resolve to snapshot path")
				}
			}
			ref = parsed
		}
		opened, err := artifact.OpenFileWithLocations(ctx, opts.SnapshotPath, ref, opts.ManifestCfg,
			opts.CustomerKeyFn, opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return nil, protectArtifactReadError(opts.LocalCodec, "open local snapshot", err)
		}
		if opened.Size() > math.MaxInt64 {
			_ = opened.Close()
			return nil, fmt.Errorf("snapshot is too large")
		}
		root := &openedRootSnapshot{stream: opened, size: int64(opened.Size()), opts: opts}
		if key, ok := opened.RootManifestKey(); ok {
			root.bundleRoot = manifest.HexKey(key)
			root.opts.Fetcher = opened.ScopedFetcher()
			root.bundleReader = opened.BundleReader()
			root.bundleFetcher = opened.ManifestFetcher()
			root.selfRef, err = fileBundleRef(opts.SnapshotPath, opts.SnapshotRef, key)
		} else {
			scheme, digest := opened.Digest()
			root.selfRef, err = fileSnapshotRef(opts.SnapshotPath, opts.SnapshotRef, scheme, digest, opts.LocalCodec)
		}
		if err != nil {
			_ = opened.Close()
			return nil, fmt.Errorf("snapshot self ref: %w", err)
		}
		return root, nil
	}

	stream, size, err := sandbox.OpenManifestStream(ctx, opts.SnapshotManifestKey, opts.Fetcher)
	if err != nil {
		return nil, fmt.Errorf("open manifest snapshot: %w", err)
	}
	selfRef := opts.SnapshotRef
	if selfRef == "" {
		selfRef = "manifest://" + opts.SnapshotManifestKey
	}
	return &openedRootSnapshot{stream: stream, size: size, selfRef: selfRef, opts: opts}, nil
}

func fileBundleRef(path, rawRef string, key store.ContentKey) (string, error) {
	source, _, err := fileBundleSource(path, rawRef)
	if err != nil {
		return "", err
	}
	ref, err := manifest.ParseRef(source)
	if err != nil {
		return "", err
	}
	ref.DigestScheme = "manifest"
	ref.Digest = manifest.HexKey(key)
	if err := ref.Validate(); err != nil {
		return "", err
	}
	return ref.String(), nil
}

func fileBundleSource(path, rawRef string) (string, string, error) {
	ref := manifest.Ref{Scheme: manifest.RefSchemeFile}
	if rawRef != "" {
		parsed, err := manifest.ParseRef(rawRef)
		if err != nil {
			return "", "", err
		}
		if parsed.Scheme != manifest.RefSchemeFile {
			return "", "", fmt.Errorf("Bundle source must use file://")
		}
		ref = parsed
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return "", "", err
	}
	if ref.Location != "" {
		// A located alias is portable only when its final target is a
		// canonical sibling in the same location directory. Otherwise the
		// basename recorded in bundle/refs would resolve to a different file.
		locationDir, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return "", "", err
		}
		if filepath.Clean(locationDir) != filepath.Clean(filepath.Dir(real)) {
			return "", "", fmt.Errorf("located Bundle alias target must remain in the same location directory")
		}
	}
	ref.Path = filepath.Base(real)
	ref.DigestScheme = ""
	ref.Digest = ""
	if err := ref.Validate(); err != nil {
		return "", "", err
	}
	if _, err := manifestbundle.EncodeRefs([]string{ref.String()}); err != nil {
		return "", "", fmt.Errorf("canonical Bundle source: %w", err)
	}
	return ref.String(), real, nil
}

// fileSnapshotRef returns the content-addressed ref for a file-mode snapshot
// bundle. It follows a node-local <sid>.snapshot symlink, then records the
// actual external scheme and digest in the child snapshot's from_refs.
func fileSnapshotRef(path, rawRef, scheme, digest string, codec tarstream.Codec) (string, error) {
	var ref manifest.Ref
	if rawRef != "" {
		parsed, err := manifest.ParseRef(rawRef)
		if err != nil {
			return "", protectArtifactReadError(codec, "parse snapshot ref", err)
		}
		ref = parsed
	} else {
		ref = manifest.Ref{Scheme: manifest.RefSchemeFile}
	}
	real := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		real = resolved
	}
	if ref.Location == "" {
		ref.Path = filepath.Base(real)
	}
	ref.DigestScheme = scheme
	ref.Digest = digest
	if err := ref.Validate(); err != nil {
		return "", err
	}
	return ref.String(), nil
}

// Package restore implements the `sandbox-ctl run --restore=` lifecycle.
// Reads a <sid>.snapshot bundle (memory + ZIP at end with config.json /
// state.json / snapshot.cfg), prepares memfd + va_report server, spawns
// patched CH with --restore source_url pointing at a temp dir holding
// the rewritten state.json, and lets faults flow.
package restore

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/chmemory"
	"github.com/kuasar-sandbox/sandboxer/pkg/chapi"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
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
// blk0 / overlay.base in the embedded sandbox.cfg likewise support
// manifest:// when Fetcher is set.
type Options struct {
	SnapshotPath        string                 // file path; mutually exclusive with SnapshotManifestKey
	SnapshotManifestKey string                 // hex content key; mutually exclusive with SnapshotPath
	SnapshotRef         string                 // canonical portable root ref; empty for a node-local file
	HostCfg             *config.SandboxConfig  // host yaml: TAP, blk1.diff, etc.
	ManifestCfg         *config.ManifestConfig // for snapshot --upload from a restored sandbox
	Fetcher             fetch.Fetcher          // required when any URI is manifest://; caller owns lifecycle
	CustomerKeyFn       ingest.CustomerKeyFunc // process-fixed key used by later snapshot upload
	LocalCodec          tarstream.Codec        // nil when crypto.local=off
	LocalRequired       bool                   // reject plaintext local artifacts and active diffs
	RefLocations        config.RefLocations    // trusted named file locations
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
	if err := opts.HostCfg.ValidateRestoreHostConfig(); err != nil {
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
	if err := preflightLocatedRefs(ctx, opts); err != nil {
		return -1, fmt.Errorf("restore preflight: %w", err)
	}

	logf := func(format string, a ...any) { log.Printf("[sandbox-ctl run --restore] "+format, a...) }
	startUnixNs := time.Now().UnixNano()

	runDir := filepath.Join(opts.RuntimeRoot, opts.SandboxID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return -1, err
	}
	defer os.RemoveAll(runDir)

	chSock := filepath.Join(runDir, "ch.sock")
	uffdSock := filepath.Join(runDir, "uffd.sock")
	stateDir := filepath.Join(runDir, "snap-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return -1, err
	}

	// Open the snapshot bundle as a single fetch.Stream — file:// is a local
	// tarstream artifact (hole map from the envelope), manifest:// is
	// chunk-granular via cache-ctl.
	// The same Stream feeds the ZIP reader (via NewReaderAt) and, layered with
	// from_refs (§3.5), the uffd SnapshotReader. selfRef is this bundle's
	// content-addressed identity, recorded into a child snapshot's from_refs.
	var (
		selfStream   fetch.Stream
		snapReaderAt io.ReaderAt
		totalSize    int64
		selfRef      string
	)
	if opts.SnapshotPath != "" {
		fs, scheme, digest, err := openSnapshotArtifact(ctx, opts)
		if err != nil {
			return -1, fmt.Errorf("open snapshot: %w", err)
		}
		defer fs.Close()
		selfStream = fs
		totalSize = int64(fs.Size())
		selfRef, err = fileSnapshotRef(opts.SnapshotPath, opts.SnapshotRef, scheme, digest, opts.LocalCodec)
		if err != nil {
			return -1, fmt.Errorf("snapshot self ref: %w", err)
		}
	} else {
		fc, sz, err := sandbox.OpenManifestStream(ctx, opts.SnapshotManifestKey, opts.Fetcher)
		if err != nil {
			return -1, fmt.Errorf("open manifest snapshot: %w", err)
		}
		defer fc.Close()
		selfStream = fc
		totalSize = sz
		selfRef = opts.SnapshotRef
		if selfRef == "" {
			selfRef = "manifest://" + opts.SnapshotManifestKey
		}
		logf("manifest snapshot: key=%s bundle_size=%d", opts.SnapshotManifestKey, sz)
	}
	snapReaderAt = fetch.NewReaderAt(ctx, selfStream)

	zipReader, err := zip.NewReader(snapReaderAt, totalSize)
	if err != nil {
		return -1, fmt.Errorf("zip.NewReader on snapshot: %w", err)
	}
	entries := make(map[string][]byte)
	for _, f := range zipReader.File {
		rc, err := f.Open()
		if err != nil {
			return -1, fmt.Errorf("zip open %s: %w", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return -1, fmt.Errorf("zip read %s: %w", f.Name, err)
		}
		entries[f.Name] = body
	}
	for _, want := range []string{"config.json", "state.json", "snapshot.cfg"} {
		if _, ok := entries[want]; !ok {
			return -1, fmt.Errorf("snapshot bundle missing %s (produced by old sandbox-ctl?)", want)
		}
	}

	// snapshot.cfg carries the post-quiesce platform contract: capacity,
	// runtime_ref, base_ref, overlay.base. ApplyRules merges it with the
	// host sandbox.yaml per docs/sandbox.md §11.0 — capacity must match
	// exactly when host provides it, runtime/base are validated against
	// digest, network source validity is checked, overlay.diff is required.
	parsedSnap, err := ParseSnapshotCfg(entries["snapshot.cfg"])
	if err != nil {
		return -1, err
	}
	if err := canonicalizeSnapshotTarRefs(ctx, parsedSnap, opts); err != nil {
		return -1, fmt.Errorf("snapshot refs: %w", err)
	}
	merged, err := ApplyRules(opts.HostCfg, parsedSnap, opts.localSnapshotPath(), opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
	if err != nil {
		return -1, err
	}
	snapCfg := *merged
	snapshotHasNetwork, err := snapshotConfigHasNetwork(entries["config.json"])
	if err != nil {
		return -1, err
	}
	hostHasNetwork := snapCfg.Network.TAP != "" || snapCfg.Network.TapFD != nil
	if err := validateRestoreNetworkTopology(snapshotHasNetwork, hostHasNetwork); err != nil {
		return -1, err
	}
	// Metadata passthrough: inherit the parent snapshot's metadata so a
	// snapshot taken by this restored run carries it forward; an explicit
	// host-yaml metadata map overrides wholesale.
	if len(snapCfg.Metadata) == 0 {
		snapCfg.Metadata = parsedSnap.Metadata
	}

	// Record provenance so a snapshot taken by this restored run prepends this
	// bundle and extends the chain (§3.5): child.from_refs = [selfRef] ++
	// this.from_refs; child.base_from_refs = [this.overlay.base] ++ this.base_from_refs.
	// The parent's "top disk layer" + chain below it: overlay.base/overlay
	// .base_from_refs in overlay mode, root.base/root.base_from_refs in
	// single-disk mode (the captured diff is recorded at root level there).
	parentDiskBase, parentDiskChain := parsedSnap.Boot.Root.Base, parsedSnap.Boot.Root.BaseFromRefs
	if !parsedSnap.SingleDisk() {
		parentDiskBase = parsedSnap.Boot.Root.Overlay.Base
		parentDiskChain = parsedSnap.Boot.Root.Overlay.BaseFromRefs
	}
	snapCfg.SnapshotProvenance = config.SnapshotProvenance{
		ParentSnapshotRef:  selfRef,
		ParentFromRefs:     parsedSnap.FromRefs,
		ParentOverlayBase:  parentDiskBase,
		ParentBaseFromRefs: parentDiskChain,
	}
	// Local restore: record the parent's on-disk bundle + top-disk-layer paths
	// so a re-export merges this run's resident delta onto them (replace the
	// next-newest local layer, not stack a second one) — docs §3.5. Paths
	// resolve like openRefStream: the disk layer is a basename in the bundle dir.
	if localSnapshotPath := opts.localSnapshotPath(); localSnapshotPath != "" {
		if abs, err := filepath.Abs(localSnapshotPath); err == nil {
			snapCfg.SnapshotProvenance.ParentSnapshotPath = abs
		}
		path, err := resolveLocalMergePath(parentDiskBase, localSnapshotPath, opts.RefLocations)
		if err != nil {
			return -1, fmt.Errorf("resolve parent root layer: %w", err)
		}
		snapCfg.SnapshotProvenance.ParentOverlayPath = path
	}
	// Per-data-disk provenance (boot.disks[] order): the data-disk analogue of
	// the root fields above, so a snapshot by this restored run extends each
	// disk's chain.
	if len(parsedSnap.Boot.Disks) > 0 {
		pd := make([]config.DiskProvenance, len(parsedSnap.Boot.Disks))
		for i := range parsedSnap.Boot.Disks {
			n := &parsedSnap.Boot.Disks[i]
			top, chain := n.Base, n.BaseFromRefs
			if !n.single() {
				top, chain = n.Overlay.Base, n.Overlay.BaseFromRefs
			}
			pd[i] = config.DiskProvenance{OverlayBase: top, BaseFromRefs: chain}
			if localSnapshotPath := opts.localSnapshotPath(); localSnapshotPath != "" {
				path, err := resolveLocalMergePath(top, localSnapshotPath, opts.RefLocations)
				if err != nil {
					return -1, fmt.Errorf("resolve parent disk %d layer: %w", i, err)
				}
				pd[i].OverlayPath = path
			}
		}
		snapCfg.SnapshotProvenance.ParentDisks = pd
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

	// CH snapshot config is the Capacity authority. sandbox.cfg must describe
	// the same exact byte domain, but it is not used to infer CH total size.
	snapCap, err := chmemory.CapacityFromVMConfig(entries["config.json"])
	if err != nil {
		return -1, fmt.Errorf("snapshot CH capacity: %w", err)
	}
	declaredCap, err := snapCfg.CapacityMemoryBytes()
	if err != nil {
		return -1, fmt.Errorf("snapshot sandbox.cfg capacity: %w", err)
	}
	if declaredCap != snapCap {
		return -1, fmt.Errorf("snapshot Capacity mismatch: CH memory zones=%d sandbox.cfg=%d", snapCap, declaredCap)
	}
	balTarget, balCurrent, balOk, err := parseBalloonFromState(entries["state.json"])
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
	// state.json restored verbatim (vCPU regs, virtio queue indices —
	// nothing path-dependent).
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), entries["state.json"], 0o644); err != nil {
		return -1, err
	}
	// config.json captured paths (uffd_socket, per-disk vhost_socket, ch.sock
	// api, vsock) → this run's sockets (disks in device order).
	vsockSock := filepath.Join(runDir, "vsock.sock")
	rewritten, err := rewriteConfigPaths(entries["config.json"], pathRewrite{
		UffdSocket: uffdSock,
		DiskSocks:  diskSocks,
		APISock:    chSock,
		VsockSock:  vsockSock,
	})
	if err != nil {
		return -1, fmt.Errorf("rewrite config.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.json"), rewritten, 0o644); err != nil {
		return -1, err
	}

	// The CH-authoritative Capacity also sizes the memfd and UFFD source.
	capBytes := snapCap
	if capBytes > uint64(^uint(0)>>1) {
		return -1, fmt.Errorf("restore Capacity %d exceeds host addressable memory size", capBytes)
	}

	// Build the layered memory source: [self bundle] ++ from_refs (§3.5). A
	// non-resident page (hole) in an upper layer falls through to a lower
	// layer; a page hole in every layer (merged hole) → ZEROPAGE. Single layer
	// (no from_refs) degenerates to today's behaviour. The from_refs streams
	// live until the run exits (closed below); selfStream is closed at open.
	memLayers := []fetch.Stream{selfStream}
	for i, ref := range parsedSnap.FromRefs {
		s, err := openRefStream(ctx, ref, opts)
		if err != nil {
			return -1, fmt.Errorf("from_refs[%d]: %w", i, err)
		}
		defer s.Close()
		memLayers = append(memLayers, s)
	}
	source, err := uffd.NewStreamSnapshotSource(fetch.NewLayered(memLayers...), capBytes)
	if err != nil {
		return -1, fmt.Errorf("snapshot source: %w", err)
	}
	logf("snapshot source: %d memory layer(s)", len(memLayers))
	prefetch := startMemoryPrefetch(ctx, prefetchMode, opts.SnapshotManifestKey, selfStream, len(memLayers)-1, logf)
	// Close is a lifetime boundary for fetch.Stream. Register this after every
	// memory-layer Close defer so cancellation and join always run first.
	defer prefetch.Stop()

	// Reconstruct each logical disk (root + data disks, in order): layer the
	// captured base ([top] ++ base_from_refs) into a ro base, build a fresh
	// writable CoW on top, and (overlay mode) open the erofs base as the ro
	// device. The root's captured top + chain come from parsedSnap.Boot.Root;
	// data disks from parsedSnap.Boot.Disks[i].
	rootTop := parsedSnap.Boot.Root.Base
	if !snapCfg.SingleDisk() {
		rootTop = parsedSnap.Boot.Root.Overlay.Base
	}
	rootDiffURI, rootDiffTmpl := snapCfg.Boot.Root.Diff, snapCfg.Boot.Root.DiffTemplate
	if !snapCfg.SingleDisk() {
		rootDiffURI, rootDiffTmpl = snapCfg.Boot.Root.Overlay.Diff, snapCfg.Boot.Root.Overlay.DiffTemplate
	}
	rootDiffSize, err := snapCfg.DiffSizeBytes()
	if err != nil {
		return -1, err
	}
	rootDB, rootCleanup, err := reconstructDisk(ctx, opts, diffCustomerKey, snapCfg.SingleDisk(), rootTop, parentDiskChain,
		snapCfg.Boot.Root.Base, rootDiffURI, rootDiffTmpl, rootDiffSize, "overlay", logf)
	if err != nil {
		return -1, err
	}
	defer rootCleanup()
	disks := []sandbox.DiskBackend{rootDB}

	for i := range snapCfg.Boot.Disks {
		d := &snapCfg.Boot.Disks[i]
		sn := &parsedSnap.Boot.Disks[i]
		single := d.Single()
		top, chain := sn.Base, sn.BaseFromRefs
		diffURI, diffTmpl := d.Diff, d.DiffTemplate
		if !single {
			top, chain = sn.Overlay.Base, sn.Overlay.BaseFromRefs
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

		SnapCfg:         &snapCfg,
		ManifestCfg:     opts.ManifestCfg,
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
				return guestlink.OpenMUXViaRestoreContext(pc.Ctx, pc.Pinger.Client, 1, netSpec, snapCfg.ProtoFiles(), restoreDeadline)
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
// capturedTop/chain come from the snapshot.cfg node; diffURI/diffTemplate from
// the merged config (host override, else auto-default <sid>.<diskKey>.diff).
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
	top, err := openRefStream(ctx, capturedTop, opts)
	if err != nil {
		return fail(fmt.Errorf("%s: open base: %w", diskKey, err))
	}
	layers := []fetch.Stream{top}
	for i, ref := range chain {
		s, serr := openRefStream(ctx, ref, opts)
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
		r, _, rerr := sandbox.OpenBlockReader(ctx, erofsBaseURI, opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if rerr != nil {
			return fail(fmt.Errorf("%s: open erofs base: %w", diskKey, rerr))
		}
		db.Reader, db.BasePath = r, erofsBaseURI
		closers = append(closers, func() { r.Close() })
	}
	return db, cleanup, nil
}

// openRefStream resolves a from_refs / base_from_refs entry (§3.5) into a
// fetch.Stream. file:// refs are content-addressed basenames located relative
// to the snapshot bundle dir (local mode); manifest:// refs go through the
// fetcher. Shared by the memory and disk layered chains.
func openRefStream(ctx context.Context, raw string, opts Options) (fetch.Stream, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return nil, protectArtifactReadError(opts.LocalCodec, "parse local artifact ref", err)
	}
	if ref.Scheme == manifest.RefSchemeFile {
		if ref.Location != "" {
			stream, _, err := sandbox.OpenDiskStream(ctx, ref.String(), opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
			return stream, err
		}
		relativeDir := ""
		if localSnapshotPath := opts.localSnapshotPath(); localSnapshotPath != "" {
			relativeDir = filepath.Dir(localSnapshotPath)
		}
		path, err := opts.RefLocations.ResolveFile(ref, relativeDir)
		if err != nil {
			return nil, err
		}
		ref.Path = path
		s, _, err := sandbox.OpenDiskStream(ctx, ref.String(), opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		return s, err
	}
	s, _, err := sandbox.OpenDiskStream(ctx, ref.String(), opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
	return s, err
}

// canonicalizeSnapshotTarRefs replaces each selected local tarstream qualifier
// with the policy-normalized identity returned by the actual artifact. A base
// ref replaced by a host override is deferred to ApplyRules, which validates
// the host artifact against that snapshot identity. In auto mode this converts
// legacy sha256 qualifiers to hmac before provenance or a newly rendered child
// snapshot can observe them. Paths and locations keep their existing resolution
// semantics; manifest refs are unchanged.
func canonicalizeSnapshotTarRefs(ctx context.Context, cfg *SnapshotCfg, opts Options) error {
	if cfg == nil {
		return fmt.Errorf("nil snapshot config")
	}
	normalize := func(label string, target *string) error {
		if target == nil || *target == "" {
			return nil
		}
		ref, err := canonicalizeSnapshotTarRef(ctx, *target, opts)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		*target = ref
		return nil
	}
	normalizeList := func(label string, refs []string) error {
		for i := range refs {
			if err := normalize(fmt.Sprintf("%s[%d]", label, i), &refs[i]); err != nil {
				return err
			}
		}
		return nil
	}
	normalizeNode := func(label string, baseRef, base *string, chain []string, overlay *SnapOverlayCfg, baseOverridden bool) error {
		if !baseOverridden {
			if err := normalize(label+".base_ref", baseRef); err != nil {
				return err
			}
		}
		if err := normalize(label+".base", base); err != nil {
			return err
		}
		if err := normalizeList(label+".base_from_refs", chain); err != nil {
			return err
		}
		if overlay == nil {
			return nil
		}
		if err := normalize(label+".overlay.base", &overlay.Base); err != nil {
			return err
		}
		return normalizeList(label+".overlay.base_from_refs", overlay.BaseFromRefs)
	}
	if err := normalizeList("from_refs", cfg.FromRefs); err != nil {
		return err
	}
	rootBaseOverridden := opts.HostCfg != nil && opts.HostCfg.Boot.Root.Base != "" && !cfg.SingleDisk()
	if err := normalizeNode("boot.root", &cfg.Boot.Root.BaseRef, &cfg.Boot.Root.Base,
		cfg.Boot.Root.BaseFromRefs, cfg.Boot.Root.Overlay, rootBaseOverridden); err != nil {
		return err
	}
	for i := range cfg.Boot.Disks {
		node := &cfg.Boot.Disks[i]
		baseOverridden := opts.HostCfg != nil && i < len(opts.HostCfg.Boot.Disks) &&
			opts.HostCfg.Boot.Disks[i].Base != "" && !node.single()
		if err := normalizeNode(fmt.Sprintf("boot.disks[%d]", i), &node.BaseRef, &node.Base,
			node.BaseFromRefs, node.Overlay, baseOverridden); err != nil {
			return err
		}
	}
	return nil
}

func canonicalizeSnapshotTarRef(ctx context.Context, raw string, opts Options) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", fmt.Errorf("invalid artifact ref")
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		return ref.String(), nil
	}
	if ref.Scheme != manifest.RefSchemeFile {
		return "", fmt.Errorf("unsupported artifact ref scheme")
	}
	stream, err := openRefStream(ctx, raw, opts)
	if err != nil {
		return "", err
	}
	scheme, digest, digestErr := sourceDigest(stream)
	closeErr := stream.Close()
	if digestErr != nil {
		return "", digestErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	ref.DigestScheme, ref.Digest = scheme, digest
	if err := ref.Validate(); err != nil {
		return "", fmt.Errorf("invalid canonical artifact ref")
	}
	return ref.String(), nil
}

func openSnapshotArtifact(ctx context.Context, opts Options) (fetch.Stream, string, string, error) {
	if opts.SnapshotRef == "" {
		options, err := tarReadOptions(manifest.Ref{}, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return nil, "", "", err
		}
		stream, err := fetch.OpenTarStream(opts.SnapshotPath, options...)
		if err != nil {
			return nil, "", "", protectArtifactReadError(opts.LocalCodec, "open local snapshot", err)
		}
		scheme, digest, digestErr := sourceDigest(stream)
		if digestErr != nil {
			_ = stream.Close()
			return nil, "", "", digestErr
		}
		return stream, scheme, digest, nil
	}
	ref, err := manifest.ParseRef(opts.SnapshotRef)
	if err != nil {
		return nil, "", "", protectArtifactReadError(opts.LocalCodec, "parse snapshot ref", err)
	}
	if ref.Scheme != manifest.RefSchemeFile {
		return nil, "", "", fmt.Errorf("snapshot path cannot use %s ref", ref.Scheme)
	}
	if ref.Location != "" {
		resolved, err := opts.RefLocations.ResolveFile(ref, "")
		if err != nil {
			return nil, "", "", err
		}
		if filepath.Clean(resolved) != filepath.Clean(opts.SnapshotPath) {
			return nil, "", "", fmt.Errorf("located root ref does not resolve to snapshot path")
		}
	}
	return openTarArtifact(opts.SnapshotPath, ref, opts.LocalCodec, opts.LocalRequired)
}

func (o Options) localSnapshotPath() string {
	if o.SnapshotRef == "" {
		return o.SnapshotPath
	}
	ref, err := manifest.ParseRef(o.SnapshotRef)
	if err != nil || !ref.Portable() {
		return o.SnapshotPath
	}
	return ""
}

func resolveLocalMergePath(raw, snapshotPath string, locations config.RefLocations) (string, error) {
	if raw == "" {
		return "", nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", err
	}
	if ref.Scheme != manifest.RefSchemeFile {
		return "", nil
	}
	return locations.ResolveFile(ref, filepath.Dir(snapshotPath))
}

const maxSnapshotParentEntries = 1024

// preflightLocatedRefs validates every artifact referenced by the root
// snapshot.cfg before cgroup, run-directory, TAP, or VMM side effects. The
// root from_refs list is already the flattened memory chain; each entry is an
// opaque memory layer and its embedded historical snapshot.cfg is not walked.
func preflightLocatedRefs(ctx context.Context, opts Options) error {
	var stream fetch.Stream
	if opts.SnapshotPath != "" {
		opened, _, _, err := openSnapshotArtifact(ctx, opts)
		if err != nil {
			return err
		}
		stream = opened
	} else {
		opened, _, err := sandbox.OpenManifestStream(ctx, opts.SnapshotManifestKey, opts.Fetcher)
		if err != nil {
			return err
		}
		stream = opened
	}
	defer stream.Close()
	_, root, err := readSnapshotEntries(ctx, stream, int64(stream.Size()))
	if err != nil {
		return err
	}
	if root.Boot.RuntimeRef != "" {
		if _, err := parseSnapshotRuntimeRef(root.Boot.RuntimeRef); err != nil {
			return fmt.Errorf("snapshot.cfg.runtime_ref: %w", err)
		}
	}
	overrides, err := preflightHostBaseOverrides(ctx, root, opts)
	if err != nil {
		return err
	}
	for _, raw := range snapshotArtifactRefs(root, &overrides) {
		if err := preflightLocatedRef(ctx, raw, opts); err != nil {
			return err
		}
	}
	seenParents := make(map[string]struct{}, maxSnapshotParentEntries+1)
	for _, raw := range root.FromRefs {
		seenParents[raw] = struct{}{}
		if len(seenParents) > maxSnapshotParentEntries {
			return fmt.Errorf("snapshot parent graph exceeds %d entries", maxSnapshotParentEntries)
		}
	}
	for i, raw := range root.FromRefs {
		parent, err := openRefStream(ctx, raw, opts)
		if err != nil {
			return fmt.Errorf("snapshot memory layer %d: %w", i, err)
		}
		if err := parent.Close(); err != nil {
			return fmt.Errorf("snapshot memory layer %d: %w", i, err)
		}
	}
	return nil
}

type preflightBaseOverrides struct {
	root  bool
	disks map[int]bool
}

func preflightHostBaseOverrides(ctx context.Context, snap *SnapshotCfg, opts Options) (preflightBaseOverrides, error) {
	var overrides preflightBaseOverrides
	if opts.HostCfg == nil {
		return overrides, nil
	}
	if opts.HostCfg.Boot.Root.Base != "" && !snap.SingleDisk() {
		snapRef, err := manifest.ParseRef(snap.Boot.Root.BaseRef)
		if err != nil {
			return overrides, protectArtifactReadError(opts.LocalCodec, "snapshot.cfg.base_ref", err)
		}
		resolved, err := resolveAnyRef(opts.HostCfg.Boot.Root.Base, snapRef, opts.localSnapshotPath(), "boot.root.base", opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return overrides, err
		}
		if err := preflightResolvedManifestBase(ctx, resolved, opts); err != nil {
			return overrides, err
		}
		overrides.root = true
	}
	limit := len(opts.HostCfg.Boot.Disks)
	if len(snap.Boot.Disks) < limit {
		limit = len(snap.Boot.Disks)
	}
	for i := 0; i < limit; i++ {
		hostRef := opts.HostCfg.Boot.Disks[i].Base
		if hostRef == "" || snap.Boot.Disks[i].single() {
			continue
		}
		snapRef, err := manifest.ParseRef(snap.Boot.Disks[i].BaseRef)
		if err != nil {
			return overrides, protectArtifactReadError(opts.LocalCodec, fmt.Sprintf("snapshot.cfg.boot.disks[%d].base_ref", i), err)
		}
		resolved, err := resolveAnyRef(hostRef, snapRef, opts.localSnapshotPath(), fmt.Sprintf("boot.disks[%d].base", i), opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return overrides, err
		}
		if err := preflightResolvedManifestBase(ctx, resolved, opts); err != nil {
			return overrides, err
		}
		if overrides.disks == nil {
			overrides.disks = make(map[int]bool)
		}
		overrides.disks[i] = true
	}
	return overrides, nil
}

func preflightResolvedManifestBase(ctx context.Context, raw string, opts Options) error {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return err
	}
	if ref.Scheme != manifest.RefSchemeManifest {
		return nil
	}
	stream, _, err := sandbox.OpenDiskStream(ctx, ref.String(), opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
	if err != nil {
		return fmt.Errorf("manifest base preflight: %w", err)
	}
	return stream.Close()
}

func snapshotArtifactRefs(snap *SnapshotCfg, overrides *preflightBaseOverrides) []string {
	refs := []string{snap.Boot.Root.Base}
	if overrides == nil || !overrides.root {
		refs = append(refs, snap.Boot.Root.BaseRef)
	}
	refs = append(refs, snap.Boot.Root.BaseFromRefs...)
	if snap.Boot.Root.Overlay != nil {
		refs = append(refs, snap.Boot.Root.Overlay.Base)
		refs = append(refs, snap.Boot.Root.Overlay.BaseFromRefs...)
	}
	for i := range snap.Boot.Disks {
		n := &snap.Boot.Disks[i]
		refs = append(refs, n.Base)
		if overrides == nil || !overrides.disks[i] {
			refs = append(refs, n.BaseRef)
		}
		refs = append(refs, n.BaseFromRefs...)
		if n.Overlay != nil {
			refs = append(refs, n.Overlay.Base)
			refs = append(refs, n.Overlay.BaseFromRefs...)
		}
	}
	return refs
}

func preflightLocatedRef(ctx context.Context, raw string, opts Options) error {
	if raw == "" {
		return nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return protectArtifactReadError(opts.LocalCodec, "parse local artifact ref", err)
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		stream, _, err := sandbox.OpenDiskStream(ctx, ref.String(), opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return fmt.Errorf("manifest artifact: %w", err)
		}
		return stream.Close()
	}
	if ref.Scheme != manifest.RefSchemeFile || ref.Location == "" {
		return nil
	}
	stream, _, err := sandbox.OpenDiskStream(ctx, ref.String(), opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
	if err != nil {
		return fmt.Errorf("located file artifact: %w", err)
	}
	return stream.Close()
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

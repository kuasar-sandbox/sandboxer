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
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
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
	if err := opts.HostCfg.ValidateRestoreHostConfig(); err != nil {
		return -1, err
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

	// cgroup join (same semantics as cold-start lifecycle.go). No-cgroup
	// mode (no cgroup_path) is a no-op. See docs/sandbox.md §4.1.
	// Initial memory.high uses the configured allocatable; the value gets
	// bumped after we derive allocatable_at_snapshot from the bundle's
	// state.json balloon (below).
	cg, err := resctl.JoinCgroupForConfig(opts.HostCfg)
	if err != nil {
		return -1, fmt.Errorf("cgroup: %w", err)
	}
	if cg.Active() {
		logf("cgroup limits set: %s (CH joins on start; sandbox-ctl stays out)", cg.Path)
	}
	defer func() { _ = cg.Cleanup() }()

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

	// Restore-side controller hooks. Admit happens once we've derived
	// allocatable_at_snapshot from the bundle's state.json balloon section
	// (see deriveAllocatableAtSnapshot below).
	// Balloon is created later (snapCap unknown until snapCfg is parsed),
	// then late-injected via hooks.SetBalloon. Until then, hooks balloon-
	// related entry points (SettledRestore, OnAllocatableChanged) treat
	// Balloon-nil as no-op on the balloon side.
	hooks, err := resctl.NewControllerHooks(resctl.ControllerHookOptions{
		SocketPath: opts.HostCfg.Resources.Control.Controller,
		CgroupPath: opts.HostCfg.Resources.Control.CgroupPath,
		Logf:       logf,
	}, opts.HostCfg)
	if err != nil {
		return -1, fmt.Errorf("controller dial: %w", err)
	}
	defer hooks.Release("normal")

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
		fs, digest, err := openTarArtifact(opts.SnapshotPath)
		if err != nil {
			return -1, fmt.Errorf("open snapshot: %w", err)
		}
		if err := validateContentAddressedName(opts.SnapshotPath, digest); err != nil {
			fs.Close()
			return -1, fmt.Errorf("open snapshot: %w", err)
		}
		if opts.SnapshotRef != "" {
			ref, err := manifest.ParseRef(opts.SnapshotRef)
			if err != nil {
				fs.Close()
				return -1, fmt.Errorf("open snapshot: %w", err)
			}
			if ref.Digest != "" {
				if err := matchDigest(digest, ref.Digest); err != nil {
					fs.Close()
					return -1, fmt.Errorf("open snapshot: %w", err)
				}
			}
		}
		defer fs.Close()
		selfStream = fs
		totalSize = int64(fs.Size())
		if opts.SnapshotRef != "" {
			selfRef = opts.SnapshotRef
		} else {
			selfRef = fileSnapshotRef(opts.SnapshotPath) // §3.5: follows symlink → file://<sha256>.snapshot
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
	merged, err := ApplyRules(opts.HostCfg, parsedSnap, opts.localSnapshotPath(), opts.RefLocations)
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

	// Carry the runtime/base refs forward so a snapshot taken by this restored
	// run records them (the cold path reads their markers via
	// populateSnapshotRefs; here they are already matched against the parent
	// snapshot.cfg, so another marker read is unnecessary). Without this,
	// snapshots from a restored sandbox
	// would have empty runtime_ref/base_ref and could not themselves be restored.
	snapCfg.SnapshotRefs = config.SnapshotRefs{
		RuntimeRef: parsedSnap.Boot.RuntimeRef,
		BaseRef:    parsedSnap.Boot.Root.BaseRef,
	}
	if len(parsedSnap.Boot.Disks) > 0 {
		snapCfg.SnapshotRefs.DiskBaseRefs = make([]string, len(parsedSnap.Boot.Disks))
		for i := range parsedSnap.Boot.Disks {
			snapCfg.SnapshotRefs.DiskBaseRefs[i] = parsedSnap.Boot.Disks[i].BaseRef
		}
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
		if sc, val, ok := config.SchemeAndPath(parentDiskBase); ok && sc == "file" {
			if !filepath.IsAbs(val) {
				val = filepath.Join(filepath.Dir(localSnapshotPath), val)
			}
			snapCfg.SnapshotProvenance.ParentOverlayPath = val
		}
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
				if sc, val, ok := config.SchemeAndPath(top); ok && sc == "file" {
					if !filepath.IsAbs(val) {
						val = filepath.Join(filepath.Dir(localSnapshotPath), val)
					}
					pd[i].OverlayPath = val
				}
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

	// Derive allocatable_at_snapshot from CH state.json's balloon section
	// (no separate resource-state.json file — see §13). When the bundle
	// predates balloon use or balloon was disabled, parseBalloonFromState
	// returns ok=false and we fall back to yaml.allocatable as if it were
	// a cold start.
	snapCap, err := snapCfg.CapacityMemoryBytes()
	if err != nil {
		return -1, fmt.Errorf("snap sandbox.cfg capacity: %w", err)
	}
	balTarget, balCurrent, balOk, err := parseBalloonFromState(entries["state.json"])
	if err != nil {
		return -1, fmt.Errorf("parse balloon from state.json: %w", err)
	}
	allocAtSnap := deriveAllocatableAtSnapshot(snapCap, balTarget, balCurrent, balOk)

	// BalloonController, sole writer of /vm.resize. Created once we know
	// snapCap and allocAtSnap: target is seeded to `cap - allocAtSnap` so
	// the in-memory state matches what CH will load from state.json when
	// it starts with --restore. Subsequent SettledRestore decides whether
	// a runtime correction is needed (initialAlloc != allocAtSnap).
	var balloonCtl *resctl.BalloonController
	if allocAtSnap < snapCap {
		balloonCtl = resctl.NewBalloonController(chSock, snapCap, logf)
		balloonCtl.SetAllocatable(allocAtSnap)
		hooks.SetBalloon(balloonCtl)
	}

	yamlAlloc, err := opts.HostCfg.AllocatableMemoryBytes()
	if err != nil {
		return -1, err
	}

	// Static mode: take max(yaml, snapshot allocatable). When the snapshot
	// was captured under a controller (dynamic mode) at a burst-elevated
	// allocatable, restoring under static mode (A/B) preserves that
	// elevated working set rather than throttling the guest.
	initialAlloc := yamlAlloc
	if allocAtSnap > initialAlloc {
		initialAlloc = allocAtSnap
	}
	if hooks.Enabled() {
		// Dynamic mode: controller decides. Floor sent = yaml.allocatable
		// (controller's 2-tier fallback uses it if headroom can't fit
		// allocAtSnap).
		granted, err := hooks.Admit(opts.SandboxID, allocAtSnap)
		if err != nil {
			return -1, fmt.Errorf("controller admit: %w", err)
		}
		initialAlloc = granted
		logf("controller admit ok, restored allocatable=%d (snapshot allocatable=%d, balloon target/current=%d/%d)",
			granted, allocAtSnap, balTarget, balCurrent)
	} else if allocAtSnap > yamlAlloc {
		logf("static mode: bumping initial allocatable from yaml=%d to snapshot allocatable=%d (balloon target/current=%d/%d)",
			yamlAlloc, allocAtSnap, balTarget, balCurrent)
	}
	// Record initialAlloc in hooks' in-memory state. No external write
	// here: cgroup memory.high is deferred to SettledRestore (Issue 4 —
	// PSI throttling during uffd-driven replay), and balloon already
	// reflects allocAtSnap from the snapshot (any correction needed
	// when initialAlloc != allocAtSnap also happens in SettledRestore,
	// after vm.resume).
	if hooks != nil {
		hooks.SetAllocatableNow(initialAlloc)
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

	// Memory capacity → memfd size (the memfd itself is owned by
	// sandbox.ServeAndWait). The uffd SnapshotSource is the only
	// restore-specific input to the shared uffd handler: Sparse (file)
	// or Manifest (chunk-granular via cache-ctl) instead of ZeroSource.
	capBytes, err := snapCfg.CapacityMemoryBytes()
	if err != nil {
		return -1, err
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
			return -1, fmt.Errorf("from_refs[%d] %q: %w", i, ref, err)
		}
		defer s.Close()
		memLayers = append(memLayers, s)
	}
	source, err := uffd.NewStreamSnapshotSource(ctx, fetch.NewLayered(memLayers...), capBytes)
	if err != nil {
		return -1, fmt.Errorf("snapshot source: %w", err)
	}
	logf("snapshot source: %d memory layer(s)", len(memLayers))
	prefetch := startMemoryPrefetch(ctx, prefetchMode, selfStream, len(memLayers)-1, logf)
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
	rootDB, rootCleanup, err := reconstructDisk(ctx, opts, snapCfg.SingleDisk(), rootTop, parentDiskChain,
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
		db, dcleanup, derr := reconstructDisk(ctx, opts, single, top, chain, d.Base, diffURI, diffTmpl, dsz, fmt.Sprintf("disk%d", i), logf)
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
	// waitAPI → /vm.resume → restore{epoch} → SettledRestore.
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
		Balloon:       balloonCtl,
		Hooks:         hooks,

		TapFile:   tapFile, // nil in tap-name/no-network modes; non-nil tapfd is inherited at fd 4
		NetMAC:    netMAC,
		NetnsFile: netnsFile, // non-nil → launch CH inside the tap's netns

		SnapCfg:     &snapCfg,
		ManifestCfg: opts.ManifestCfg,
		Forwards:    opts.Forwards,
		Cgroup:      cg,

		BuildCmd: func(e sandbox.CmdEnv) (*exec.Cmd, func(), error) {
			// CH 51 `--restore source_url=file://<dir>` replaces
			// --kernel/--vsock; --console/--serial are restored from the
			// snapshot bundle (taken with `--console tty --serial off`),
			// so we don't repeat them. consoleArg is unused here.
			cmd := exec.CommandContext(ctx, opts.CHBinary)
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
			if err := chapi.WaitReady(ctx, pc.CHSock, opts.HostCfg.APIReadyDeadline()); err != nil {
				return fmt.Errorf("ch api not ready: %w", err)
			}
			if err := (chapi.Client{Sock: pc.CHSock, RespDeadline: opts.HostCfg.CHApiDeadline()}).Resume(); err != nil {
				return fmt.Errorf("vm.resume: %w", err)
			}
			pc.Logf("VM resumed, vCPU running")

			tRestore := time.Now()
			// 0 = no forced timeout: DialRaw needs a finite value, so fall back
			// to noForcedTimeout (effective-infinity; cancellation still flows
			// via ctx → CH teardown closing the vsock conn).
			restoreDeadline := opts.HostCfg.RestoreDeadline()
			if restoreDeadline <= 0 {
				restoreDeadline = config.NoForcedTimeout
			}
			muxConn, muxSpec, err := guestlink.OpenMUXViaRestore(pc.Pinger.Client, 1, netSpec, snapCfg.ProtoFiles(), restoreDeadline)
			if err != nil {
				return fmt.Errorf("notify restore: %w (guest agent unreachable)", err)
			}
			if err := pc.EstablishMUX(muxConn, muxSpec); err != nil {
				return fmt.Errorf("stdio MUX bridge: %w", err)
			}
			pc.Logf("restore notify acked in %dµs (stdio MUX re-established: tty=%v); starting ping ticker",
				time.Since(tRestore).Microseconds(), muxSpec.TTY)
			pc.Pinger.Start(pc.Ctx)
			// Balloon reconcile: idempotent — if initialAlloc ==
			// allocAtSnap, target matches what CH loaded from state.json.
			if pc.Balloon != nil {
				if err := pc.Balloon.Start(pc.Ctx); err != nil {
					pc.Logf("balloon: start: %v", err)
				}
			}
			// Restore-path settled trigger (docs/sandbox.md §10.1):
			// restore_ack is the controller's equivalent of cold-start
			// hello. Writes memory.high (deferred from JoinCgroup —
			// Issue 4) using allocatable_now; corrects balloon only when
			// initialAlloc != allocAtSnap.
			if pc.Hooks != nil {
				if err := pc.Hooks.SettledRestore(allocAtSnap); err != nil {
					pc.Logf("settled-restore: %v (continuing)", err)
				}
				if pc.Hooks.Enabled() {
					pc.Hooks.StartHeartbeat(pc.Ctx, 5*time.Second)
					pc.Hooks.StartSensor(pc.Ctx, 64<<20)
				}
			}
			return nil
		},
	})
}

// reconstructDisk rebuilds one logical disk for restore: the read-only base is
// [captured top] ++ chain (§3.5) layered into a Stream; a FRESH writable CoW is
// built on top (single mode: this IS the disk; overlay mode: the ext4 upper).
// In overlay mode the erofs base (erofsBaseURI) is opened as the ro device.
// capturedTop/chain come from the snapshot.cfg node; diffURI/diffTemplate from
// the merged config (host override, else auto-default <sid>.<diskKey>.diff).
// The returned cleanup closes the readers/CoW and removes an auto-created diff.
func reconstructDisk(ctx context.Context, opts Options, single bool, capturedTop string, chain []string, erofsBaseURI, diffURI, diffTemplate string, diffSize int64, diskKey string, logf func(string, ...any)) (sandbox.DiskBackend, func(), error) {
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
	logf("disk %s image: %s", diskKey, capturedTop)
	top, err := openRefStream(ctx, capturedTop, opts)
	if err != nil {
		return fail(fmt.Errorf("%s: open base: %w", diskKey, err))
	}
	layers := []fetch.Stream{top}
	for i, ref := range chain {
		s, serr := openRefStream(ctx, ref, opts)
		if serr != nil {
			return fail(fmt.Errorf("%s base_from_refs[%d] %q: %w", diskKey, i, ref, serr))
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
	createSize, err := sandbox.PrepareDiff(diffPath, diffTemplate, baseReader.Size(), diffSize)
	if err != nil {
		return fail(fmt.Errorf("%s: prepare diff: %w", diskKey, err))
	}
	cow, err := vhost.OpenBlockCOW(diffPath, baseReader, createSize)
	if err != nil {
		return fail(fmt.Errorf("%s: open BlockCOW: %w", diskKey, err))
	}
	closers = append(closers, func() { cow.Close() })
	db.Cow, db.DiffPath = cow, diffPath

	// Overlay mode: the ro erofs base device.
	if !single {
		r, _, rerr := sandbox.OpenBlockReader(ctx, erofsBaseURI, opts.Fetcher, opts.RefLocations)
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
		return nil, err
	}
	if ref.Scheme == manifest.RefSchemeFile {
		relativeDir := ""
		if localSnapshotPath := opts.localSnapshotPath(); localSnapshotPath != "" {
			relativeDir = filepath.Dir(localSnapshotPath)
		}
		path, err := opts.RefLocations.ResolveFile(ref, relativeDir)
		if err != nil {
			return nil, err
		}
		s, digest, err := openTarArtifact(path)
		if err != nil {
			return nil, err
		}
		if ref.Digest != "" {
			if err := matchDigest(digest, ref.Digest); err != nil {
				s.Close()
				return nil, err
			}
			if ref.Location != "" {
				if err := validateContentAddressedName(path, digest); err != nil {
					s.Close()
					return nil, err
				}
			}
		} else if err := validateContentAddressedName(path, digest); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}
	s, _, err := sandbox.OpenDiskStream(ctx, ref.String(), opts.Fetcher, opts.RefLocations)
	return s, err
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

// preflightLocatedRefs walks snapshot.cfg plus its memory parents and verifies
// every named location before cgroup, run-directory, TAP, or VMM side effects.
func preflightLocatedRefs(ctx context.Context, opts Options) error {
	var stream fetch.Stream
	if opts.SnapshotPath != "" {
		opened, _, err := openTarArtifact(opts.SnapshotPath)
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
	pending := []*SnapshotCfg{root}
	seenParents := map[string]bool{}
	for len(pending) > 0 {
		snap := pending[0]
		pending = pending[1:]
		if err := preflightLocatedRef(snap.Boot.RuntimeRef, opts, readRuntimeBundleDigest); err != nil {
			return err
		}
		refs := snapshotArtifactRefs(snap)
		for _, raw := range refs {
			if err := preflightLocatedRef(raw, opts, readTarArtifactDigest); err != nil {
				return err
			}
		}
		for _, raw := range snap.FromRefs {
			if seenParents[raw] {
				continue
			}
			seenParents[raw] = true
			if len(seenParents) > 1024 {
				return fmt.Errorf("snapshot parent graph exceeds 1024 entries")
			}
			parent, err := openRefStream(ctx, raw, opts)
			if err != nil {
				return fmt.Errorf("snapshot parent %s: %w", raw, err)
			}
			_, parentCfg, readErr := readSnapshotEntries(ctx, parent, int64(parent.Size()))
			closeErr := parent.Close()
			if readErr != nil {
				return fmt.Errorf("snapshot parent %s: %w", raw, readErr)
			}
			if closeErr != nil {
				return closeErr
			}
			pending = append(pending, parentCfg)
		}
	}
	return nil
}

func snapshotArtifactRefs(snap *SnapshotCfg) []string {
	refs := []string{snap.Boot.Root.BaseRef, snap.Boot.Root.Base}
	refs = append(refs, snap.Boot.Root.BaseFromRefs...)
	if snap.Boot.Root.Overlay != nil {
		refs = append(refs, snap.Boot.Root.Overlay.Base)
		refs = append(refs, snap.Boot.Root.Overlay.BaseFromRefs...)
	}
	for i := range snap.Boot.Disks {
		n := &snap.Boot.Disks[i]
		refs = append(refs, n.BaseRef, n.Base)
		refs = append(refs, n.BaseFromRefs...)
		if n.Overlay != nil {
			refs = append(refs, n.Overlay.Base)
			refs = append(refs, n.Overlay.BaseFromRefs...)
		}
	}
	return refs
}

func preflightLocatedRef(raw string, opts Options, readDigest func(string) (string, error)) error {
	if raw == "" {
		return nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return err
	}
	if ref.Scheme != manifest.RefSchemeFile || ref.Location == "" {
		return nil
	}
	path, err := opts.RefLocations.ResolveFile(ref, "")
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("located ref %s: %w", ref.String(), err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("located ref %s: location target is not a regular file", ref.String())
	}
	digest, err := readDigest(path)
	if err != nil {
		return fmt.Errorf("located ref %s: %w", ref.String(), err)
	}
	if ref.Digest != "" {
		if err := matchDigest(digest, ref.Digest); err != nil {
			return fmt.Errorf("located ref %s: %w", ref.String(), err)
		}
	}
	if err := validateContentAddressedName(path, digest); err != nil {
		return fmt.Errorf("located ref %s: %w", ref.String(), err)
	}
	return nil
}

// fileSnapshotRef returns the content-addressed ref for a file-mode snapshot
// bundle: it follows a <sid>.snapshot symlink to the real <sha256>.snapshot
// and returns file://<basename>. Recorded into a child snapshot's from_refs.
func fileSnapshotRef(path string) string {
	real := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		real = resolved
	}
	return "file://" + filepath.Base(real)
}

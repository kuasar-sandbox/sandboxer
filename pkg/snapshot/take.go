// Package snapshot implements the sandbox-ctl export/snapshot path: one
// quiesced disk capture creates Sandbox E, while memory snapshots additionally
// call CH /vm.snapshot and create Snapshot S as the operation root.
//
// The ctl.sock wire protocol + listener that carries snapshot_request
// from `sandbox-ctl snapshot` to the run process lives in
// pkg/ctl (shared with the exec path).
//
// See sandbox.md §6 for the file format and timing.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/chapi"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// Quiescer abstracts the vhost backend's pause/resume hooks. A typical
// caller passes a callback that calls srv0.Quiesce(); srv1.Quiesce()
// and the inverse for Resume.
type Quiescer interface {
	Quiesce()
	Resume()
}

// Sources gathers the inputs Take needs.
type Sources struct {
	Context    context.Context
	SandboxID  string // <sid> for output filename / symlink
	APISock    string // CH api socket
	MemfdFD    int    // memfd backing the zone (read-only here; CH is paused)
	MemfdSize  int64  // ramSize
	StagingDir string // CH /vm.snapshot dest for config.json/state.json (caller creates+removes)

	// Diffs are the logical disks' writable diffs to capture, in order: Diffs[0]
	// is the root, Diffs[1:] are the boot.disks[] data disks (boot.disks[]
	// order). Take captures every diff once; data refs and the root payload are
	// recorded in Sandbox E, never duplicated in snapshot.cfg.
	Diffs []DiskDiff
	// PortableConfig is immutable C0. ParentSandboxRef materializes the old
	// self when E is regenerated; MemoryFromRefs is the independent S chain.
	PortableConfig   *config.PortableSandboxConfig
	ParentSandboxRef string
	MemoryFromRefs   []string

	// CHApiDeadline bounds each CH API call (pause/snapshot/resume); 0 = no
	// forced. From config.SandboxConfig.CHApiDeadline() (timeouts.ch_api).
	CHApiDeadline time.Duration

	// MergeBaseSnapshot is the parent local snapshot's already-resolved,
	// scheme-qualified file ref (memory section = [0,MemfdSize)). When set, Take flattens
	// this run's resident memory delta ONTO it and absorbs the merged result as
	// the new top — replacing the next-newest local layer instead of stacking
	// (docs/sandbox.md §11.1). Per-disk overlay flattening is driven by
	// DiskDiff.MergeBase.
	// Empty ⇒ no merge (stack via the parent refs).
	MergeBaseSnapshot string

	Quiescer      Quiescer
	Logf          func(string, ...any)
	LocalCodec    tarstream.Codec
	LocalRequired bool
	// MergeBaseOpener opens a resolved file:// tarstream or Manifest Bundle
	// selector. It is supplied by the lifecycle when Bundle-aware provenance is
	// possible; nil selects the ordinary local tarstream opener.
	MergeBaseOpener MergeBaseOpener
	// MemoryMergeBaseOpener opens a parent Snapshot S and exposes only its
	// memory prefix. Keeping it separate prevents either ZIP tail from entering
	// a merge and keeps disk parents payload-only.
	MemoryMergeBaseOpener MergeBaseOpener
}

// DiskDiff is one logical disk's writable diff to capture. SnapshotView is the
// live COW's upper-only logical source; Path is retained for diagnostics and
// lifecycle bookkeeping, never reopened as snapshot data. Owned marks an
// auto-created diff eligible for cleanup. MergeBase, when set (local-parent
// re-export), is the parent's already-resolved, scheme-qualified local overlay
// ref this disk's delta is flattened onto (replace, not stack).
type DiskDiff struct {
	Path         string
	Owned        bool
	MergeBase    string
	SnapshotView func() (io.ReadSeeker, []sparse.Extent, error)
}

// Outputs describes what was produced. Refs are scheme-tagged
// (scheme-qualified file ref | manifest://<key>); Path is the local file (file mode
// only, "" for upload). The handler maps these into the ctl.Response.
type Outputs struct {
	SnapshotRef  string
	SnapshotPath string
	SandboxRef   string
	SandboxPath  string

	MemorySize       uint64
	MemoryResident   uint64
	WallclockPauseMs int64
	WallclockDumpMs  int64
}

// Take runs the snapshot sequence and streams Sandbox E plus Snapshot S through
// the sink without staging either large logical source in /run. Only CH's small
// config.json/state.json files land in StagingDir. Every disk is captured once;
// E is emitted before CH memory state, and S is committed last with sandbox_ref
// pointing to E.
//
// Caller responsibility: create/remove StagingDir and transfer ownership of a
// sink (FileSink for --output, IngestSink for --upload, or BundleSink). Take
// closes it on every return path.
func Take(s Sources, sink ArtifactSink, resumeAfter bool) (_ *Outputs, retErr error) {
	sinkOpen := sink != nil
	defer func() {
		if sinkOpen {
			retErr = errors.Join(retErr, closeArtifactSink(sink))
		}
	}()
	logf := s.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if s.SandboxID == "" {
		return nil, fmt.Errorf("snapshot: empty SandboxID")
	}
	if s.APISock == "" {
		return nil, fmt.Errorf("snapshot: empty CH API socket")
	}
	if s.StagingDir == "" {
		return nil, fmt.Errorf("snapshot: empty staging directory")
	}
	if s.MemfdFD < 0 {
		return nil, fmt.Errorf("snapshot: invalid memfd")
	}
	if s.PortableConfig == nil {
		return nil, fmt.Errorf("snapshot: immutable C0 is required")
	}
	if err := s.PortableConfig.Validate(); err != nil {
		return nil, fmt.Errorf("snapshot C0: %w", err)
	}
	if len(s.Diffs) != 1+len(s.PortableConfig.Boot.Disks) {
		return nil, fmt.Errorf("snapshot: runtime disk count %d does not match C0 disk count %d", len(s.Diffs), 1+len(s.PortableConfig.Boot.Disks))
	}
	for i := range s.Diffs {
		if s.Diffs[i].SnapshotView == nil {
			return nil, fmt.Errorf("snapshot: disk %d has no SnapshotView", i)
		}
	}
	if sink == nil {
		return nil, fmt.Errorf("snapshot: nil sink")
	}
	if s.MemfdSize <= 0 {
		return nil, fmt.Errorf("snapshot: memory size must be positive")
	}
	if s.Quiescer == nil {
		return nil, fmt.Errorf("snapshot: nil backend Quiescer")
	}
	ctx := s.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &Outputs{MemorySize: uint64(s.MemfdSize)}
	ch := chapi.Client{Sock: s.APISock, RespDeadline: s.CHApiDeadline}

	// T2a: pause CH.
	pauseStart := time.Now()
	if err := ch.Pause(); err != nil {
		return nil, fmt.Errorf("CH pause: %w", err)
	}
	pausedAt := time.Now()
	resumed := false
	succeeded := false
	defer func() {
		// A failed snapshot always leaves the sandbox running, so undo the CH
		// pause even on the destroy-on-success path. Successful destroy mode is
		// handled by the caller via /vmm.shutdown and deliberately stays paused.
		if !resumed && (!succeeded || resumeAfter) {
			_ = ch.Resume()
		}
	}()
	backendsResumed := false
	defer func() {
		if !backendsResumed {
			s.Quiescer.Resume()
		}
	}()

	// T2b: quiesce backends (steady state before the dump).
	s.Quiescer.Quiesce()

	// T3: capture all disks once and emit Sandbox E while the same CH/backend
	// freeze remains held. E is a dependency, not yet the operation root.
	dumpStart := time.Now()
	sandboxOut, err := captureSandboxAtFreeze(ctx, ExportSources{
		SandboxID: s.SandboxID, PortableConfig: s.PortableConfig,
		ParentSandboxRef: s.ParentSandboxRef, Diffs: s.Diffs,
		LocalCodec: s.LocalCodec, LocalRequired: s.LocalRequired,
		MergeBaseOpener: s.MergeBaseOpener,
	}, sink, false)
	if err != nil {
		return nil, err
	}
	out.SandboxRef, out.SandboxPath = sandboxOut.SandboxRef, sandboxOut.SandboxPath

	// T4: CH /vm.snapshot → staging dir. CH writes only config.json + state.json
	// there (small); the multi-GiB memory + disk never touch the staging tmpfs —
	// they stream straight to the sink.
	if err := ch.Snapshot("file://" + s.StagingDir); err != nil {
		return nil, fmt.Errorf("CH snapshot: %w", err)
	}
	configJSON, err := os.ReadFile(filepath.Join(s.StagingDir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read config.json: %w", err)
	}
	stateJSON, err := os.ReadFile(filepath.Join(s.StagingDir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("read state.json: %w", err)
	}

	// T5: the memory-only snapshot.cfg points at E and carries only the memory
	// parent chain.
	snapshotCfg, err := MarshalConfig(&Config{
		Version: SnapshotConfigVersion, SandboxRef: out.SandboxRef,
		FromRefs: append([]string(nil), s.MemoryFromRefs...),
	})
	if err != nil {
		return nil, fmt.Errorf("build snapshot.cfg: %w", err)
	}
	// T6: [memory][ZIP] bundle → sink, streamed from the memfd (CH paused, so
	// the mapping is stable); only resident pages are read/transferred.
	memHoles, err := WalkHoles(s.MemfdFD, s.MemfdSize)
	if err != nil {
		return nil, fmt.Errorf("memory holes: %w", err)
	}
	var memSrc io.ReadSeeker = memfdReader(s.MemfdFD, s.MemfdSize)
	memSrcHoles := memHoles
	memoryBaseOpen := false
	var closeMemoryBase func() error
	defer func() {
		if memoryBaseOpen {
			if closeErr := closeMemoryBase(); closeErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close memory merge base: %w", closeErr))
			}
		}
	}()
	if s.MergeBaseSnapshot != "" {
		base, baseHoles, berr := openMergeBaseWithOpener(ctx, s.MergeBaseSnapshot, s.MemfdSize, s.LocalCodec, s.LocalRequired, s.MemoryMergeBaseOpener)
		if berr != nil {
			return nil, fmt.Errorf("merge memory base: %w", berr)
		}
		closeMemoryBase = base.Close
		memoryBaseOpen = true
		memSrc, memSrcHoles = mergeSparse(memSrc, memHoles, base, baseHoles, s.MemfdSize)
	}
	out.MemoryResident = residentBytes(s.MemfdSize, memSrcHoles) // bytes actually written (merged)
	memorySource := &seekerSource{rs: memSrc, size: uint64(s.MemfdSize), holes: memSrcHoles}
	snapshotSource, err := snapshotfile.BuildSource(memorySource, configJSON, stateJSON, snapshotCfg)
	if err != nil {
		return nil, fmt.Errorf("build Snapshot S: %w", err)
	}
	out.SnapshotRef, out.SnapshotPath, err = sink.AbsorbSnapshot(ctx, snapshotSource)
	var memoryBaseCloseErr error
	if memoryBaseOpen {
		memoryBaseCloseErr = closeMemoryBase()
		memoryBaseOpen = false
	}
	if err != nil || memoryBaseCloseErr != nil {
		return nil, fmt.Errorf("absorb Snapshot S: %w", errors.Join(err, memoryBaseCloseErr))
	}
	if err := sink.CommitSnapshot(ctx, out.SnapshotRef, out.SnapshotPath); err != nil {
		return nil, fmt.Errorf("commit Snapshot S: %w", err)
	}
	closeErr := closeArtifactSink(sink)
	sinkOpen = false
	if closeErr != nil {
		return nil, closeErr
	}
	dumpEnd := time.Now()

	// T8: resume (destroy path handled by caller).
	if resumeAfter {
		s.Quiescer.Resume()
		backendsResumed = true
		if err := ch.Resume(); err != nil {
			return nil, fmt.Errorf("CH resume: %w", err)
		}
		resumed = true
	}

	out.WallclockPauseMs = pausedAt.Sub(pauseStart).Milliseconds()
	out.WallclockDumpMs = dumpEnd.Sub(dumpStart).Milliseconds()
	logf("snapshot: sandbox=%s snapshot=%s memory_resident=%d", out.SandboxRef, out.SnapshotRef, out.MemoryResident)
	succeeded = true
	return out, nil
}

// absorbOverlay streams one disk's diff to the sink, optionally flattening it
// onto the parent's local overlay (merge, replacing the parent layer).
func absorbOverlay(ctx context.Context, sink ArtifactSink, d DiskDiff, merging bool, codec tarstream.Codec, required bool) (string, string, error) {
	return absorbOverlayWithOpener(ctx, sink, d, merging, codec, required, nil)
}

func absorbOverlayWithOpener(ctx context.Context, sink ArtifactSink, d DiskDiff, merging bool, codec tarstream.Codec, required bool, opener MergeBaseOpener) (string, string, error) {
	if d.SnapshotView == nil {
		return "", "", fmt.Errorf("snapshot diff %s has no snapshot view", d.Path)
	}
	diff, overlayHoles, err := d.SnapshotView()
	if err != nil {
		return "", "", fmt.Errorf("snapshot view %s: %w", d.Path, err)
	}
	size, err := seekerSize(diff)
	if err != nil {
		return "", "", fmt.Errorf("snapshot view size %s: %w", d.Path, err)
	}
	var src io.ReadSeeker = diff
	holes := overlayHoles
	if merging {
		base, baseHoles, berr := openMergeBaseWithOpener(ctx, d.MergeBase, size, codec, required, opener)
		if berr != nil {
			return "", "", fmt.Errorf("merge overlay base: %w", berr)
		}
		src, holes = mergeSparse(diff, overlayHoles, base, baseHoles, size)
		ref, path, absorbErr := sink.AbsorbOverlay(ctx, src, holes)
		closeErr := base.Close()
		if absorbErr != nil || closeErr != nil {
			return "", "", errors.Join(absorbErr, closeErr)
		}
		return ref, path, nil
	}
	return sink.AbsorbOverlay(ctx, src, holes)
}

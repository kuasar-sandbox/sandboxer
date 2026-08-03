// Package snapshot implements the sandbox-ctl snapshot path: CH
// /vm.snapshot orchestration, sparse memfd capture, [memory][ZIP]
// bundle composition into tarstream artifacts, and manifest-store
// upload.
//
// The ctl.sock wire protocol + listener that carries snapshot_request
// from `sandbox-ctl snapshot` to the run process lives in
// pkg/ctl (shared with the exec path).
//
// See sandbox.md §6 for the file format and timing.
package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/chapi"
)

// Quiescer abstracts the vhost backend's pause/resume hooks. A typical
// caller passes a callback that calls srv0.Quiesce(); srv1.Quiesce()
// and the inverse for Resume.
type Quiescer interface {
	Quiesce()
	Resume()
}

// Sources gathers the inputs Take needs.
//
// SnapshotCfg is rendered late (Take calls it with the final overlay.base
// ref — file://<sha>.overlay or manifest://<key>) because that ref depends
// on how the overlay was absorbed by the sink.
type Sources struct {
	SandboxID  string // <sid> for output filename / symlink
	APISock    string // CH api socket
	MemfdFD    int    // memfd backing the zone (read-only here; CH is paused)
	MemfdSize  int64  // ramSize
	StagingDir string // CH /vm.snapshot dest for config.json/state.json (caller creates+removes)

	// Diffs are the logical disks' writable diffs to capture, in order: Diffs[0]
	// is the root, Diffs[1:] are the boot.disks[] data disks (boot.disks[]
	// order). Take absorbs each as a separate overlay artifact; SnapshotCfg
	// receives the resulting refs in the same order.
	Diffs []DiskDiff

	// CHApiDeadline bounds each CH API call (pause/snapshot/resume); 0 = no
	// forced. From config.SandboxConfig.CHApiDeadline() (timeouts.ch_api).
	CHApiDeadline time.Duration

	// MergeBaseSnapshot is the parent LOCAL <sha>.snapshot abs path this run was
	// restored from (memory section = [0,MemfdSize)). When set, Take flattens
	// this run's resident memory delta ONTO it and absorbs the merged result as
	// the new top — replacing the next-newest local layer instead of stacking
	// (docs §3.5). Per-disk overlay flattening is driven by DiskDiff.MergeBase.
	// Empty ⇒ no merge (stack via the parent refs).
	MergeBaseSnapshot string

	// SnapshotCfg renders snapshot.cfg given the final overlay refs (one per
	// logical disk, in Diffs order).
	SnapshotCfg func(overlayRefs []string) ([]byte, error)

	Quiescer Quiescer
	Logf     func(string, ...any)
}

// DiskDiff is one logical disk's writable diff to capture. SnapshotView is the
// live COW's upper-only logical source; Path is retained for diagnostics and
// lifecycle bookkeeping, never reopened as snapshot data. Owned marks an
// auto-created diff eligible for cleanup. MergeBase, when set (local-parent
// re-export), is the parent's local overlay file path this disk's delta is
// flattened onto (replace, not stack).
type DiskDiff struct {
	Path         string
	Owned        bool
	MergeBase    string
	SnapshotView func() (io.ReadSeeker, []sparse.Extent, error)
}

// Outputs describes what was produced. Refs are scheme-tagged
// (file://<sha>.ext | manifest://<key>); Path is the local file (file mode
// only, "" for upload). The handler maps these into the ctl.Response.
type Outputs struct {
	OverlayRefs  []string // one per logical disk, in Diffs order
	OverlayPaths []string // local file path per disk (file mode; "" for upload)
	SnapshotRef  string
	SnapshotPath string

	MemorySize       uint64
	MemoryResident   uint64
	WallclockPauseMs int64
	WallclockDumpMs  int64
}

// Take runs the snapshot sequence (§6.2 T2-T8) and streams the two large
// artifacts (blk1 overlay, memory+ZIP bundle) through the sink — never staging
// them in the /run tmpfs. Only CH's small config.json/state.json land in
// StagingDir. The overlay is absorbed first so snapshot.cfg can carry its final
// overlay.base ref.
//
// Caller responsibility: create/remove StagingDir; supply the sink
// (fileSink for --output, ingestSink for --upload) and the SnapshotCfg builder.
func Take(s Sources, sink SnapshotSink, resumeAfter bool) (*Outputs, error) {
	logf := s.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if s.SandboxID == "" {
		return nil, fmt.Errorf("snapshot: empty SandboxID")
	}
	if s.SnapshotCfg == nil {
		return nil, fmt.Errorf("snapshot: nil SnapshotCfg builder")
	}
	if sink == nil {
		return nil, fmt.Errorf("snapshot: nil sink")
	}
	ctx := context.Background()
	out := &Outputs{MemorySize: uint64(s.MemfdSize)}
	ch := chapi.Client{Sock: s.APISock, RespDeadline: s.CHApiDeadline}

	// T2a: pause CH.
	pauseStart := time.Now()
	if err := ch.Pause(); err != nil {
		return nil, fmt.Errorf("CH pause: %w", err)
	}
	pausedAt := time.Now()
	resumed := false
	defer func() {
		// resume_after=false (destroy mode) is handled by the caller via
		// /vm.shutdown after Take returns; here we only resume on the
		// resume_after=true path.
		if !resumed && resumeAfter {
			_ = ch.Resume()
		}
	}()
	defer s.Quiescer.Resume() // unconditional

	// T2b: quiesce backends (steady state before the dump).
	s.Quiescer.Quiesce()

	// T3: CH /vm.snapshot → staging dir. CH writes only config.json + state.json
	// there (small); the multi-GiB memory + disk never touch the staging tmpfs —
	// they stream straight to the sink.
	dumpStart := time.Now()
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

	// T4: overlays → sink, one per logical disk (root + data disks), in order.
	// Done first so snapshot.cfg below carries the final overlay.base refs.
	// Per-disk flattening is independent from memory flattening. A working-set
	// snapshot deliberately leaves MergeBaseSnapshot empty so its memory self
	// layer stays separate, while every local disk still carries MergeBase and
	// is flattened as before.
	out.OverlayRefs = make([]string, len(s.Diffs))
	out.OverlayPaths = make([]string, len(s.Diffs))
	for i, d := range s.Diffs {
		ref, path, err := absorbOverlay(ctx, sink, d, d.MergeBase != "")
		if err != nil {
			return nil, fmt.Errorf("disk %d: %w", i, err)
		}
		out.OverlayRefs[i], out.OverlayPaths[i] = ref, path
	}

	// T5: snapshot.cfg (final overlay refs) → ZIP trailer.
	snapshotCfg, err := s.SnapshotCfg(out.OverlayRefs)
	if err != nil {
		return nil, fmt.Errorf("build snapshot.cfg: %w", err)
	}
	zipBytes, err := BuildZIP(map[string][]byte{
		"config.json":  configJSON,
		"state.json":   stateJSON,
		"snapshot.cfg": snapshotCfg,
	})
	if err != nil {
		return nil, fmt.Errorf("build zip: %w", err)
	}

	// T6: [memory][ZIP] bundle → sink, streamed from the memfd (CH paused, so
	// the mapping is stable); only resident pages are read/transferred.
	memHoles, err := WalkHoles(s.MemfdFD, s.MemfdSize)
	if err != nil {
		return nil, fmt.Errorf("memory holes: %w", err)
	}
	var memSrc io.ReadSeeker = memfdReader(s.MemfdFD, s.MemfdSize)
	memSrcHoles := memHoles
	if s.MergeBaseSnapshot != "" {
		base, baseHoles, berr := openMergeBase(s.MergeBaseSnapshot, s.MemfdSize)
		if berr != nil {
			return nil, fmt.Errorf("merge memory base: %w", berr)
		}
		defer base.Close()
		memSrc, memSrcHoles = mergeSparse(memSrc, memHoles, base, baseHoles, s.MemfdSize)
	}
	out.MemoryResident = residentBytes(s.MemfdSize, memSrcHoles) // bytes actually written (merged)
	out.SnapshotRef, out.SnapshotPath, err = sink.AbsorbBundle(
		ctx, memSrc, memSrcHoles, bytes.NewReader(zipBytes))
	if err != nil {
		return nil, fmt.Errorf("absorb bundle: %w", err)
	}
	dumpEnd := time.Now()

	// T8: resume (destroy path handled by caller).
	if resumeAfter {
		if err := ch.Resume(); err != nil {
			return nil, fmt.Errorf("CH resume: %w", err)
		}
		resumed = true
	}

	out.WallclockPauseMs = pausedAt.Sub(pauseStart).Milliseconds()
	out.WallclockDumpMs = dumpEnd.Sub(dumpStart).Milliseconds()
	logf("snapshot: overlays=%v snapshot=%s memory_resident=%d", out.OverlayRefs, out.SnapshotRef, out.MemoryResident)
	return out, nil
}

// absorbOverlay streams one disk's diff to the sink, optionally flattening it
// onto the parent's local overlay (merge, replacing the parent layer).
func absorbOverlay(ctx context.Context, sink SnapshotSink, d DiskDiff, merging bool) (string, string, error) {
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
		base, baseHoles, berr := openMergeBase(d.MergeBase, size)
		if berr != nil {
			return "", "", fmt.Errorf("merge overlay base: %w", berr)
		}
		defer base.Close()
		src, holes = mergeSparse(diff, overlayHoles, base, baseHoles, size)
	}
	return sink.AbsorbOverlay(ctx, src, holes)
}

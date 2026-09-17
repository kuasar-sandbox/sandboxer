package snapshot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/chapi"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

// ExportSources is the Sandbox-artifact lifecycle input. It deliberately has no
// memfd or CH snapshot staging fields, making calls to /vm.snapshot and memory
// scanning structurally impossible.
type ExportSources struct {
	SandboxID        string
	APISock          string
	CHApiDeadline    time.Duration
	PortableConfig   *config.PortableSandboxConfig
	ParentSandboxRef string
	Diffs            []DiskDiff // root first, then data disks
	Quiescer         Quiescer
	Logf             func(string, ...any)
	LocalCodec       tarstream.Codec
	LocalRequired    bool
	MergeBaseOpener  MergeBaseOpener
}

type SandboxOutput struct {
	SandboxRef       string
	SandboxPath      string
	DataRefs         []string
	DataPaths        []string
	PortableConfig   *config.PortableSandboxConfig
	WallclockPauseMs int64
	WallclockDumpMs  int64
}

// Export pauses CH, quiesces every block backend, captures one Sandbox E and
// commits E as the operation root. The entire sink read stays inside the
// quiesced window so BlockCOW SnapshotView cannot change underneath it.
func Export(ctx context.Context, sources ExportSources, sink ArtifactSink, resumeAfter bool) (_ *SandboxOutput, retErr error) {
	sinkOpen := sink != nil
	defer func() {
		if sinkOpen {
			retErr = errors.Join(retErr, closeArtifactSink(sink))
		}
	}()
	if err := validateExportSources(sources, sink); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ch := chapi.Client{Sock: sources.APISock, RespDeadline: sources.CHApiDeadline}
	pauseStart := time.Now()
	if err := ch.Pause(); err != nil {
		return nil, fmt.Errorf("export CH pause: %w", err)
	}
	pausedAt := time.Now()
	resumed := false
	succeeded := false
	defer func() {
		if !resumed && (!succeeded || resumeAfter) {
			_ = ch.Resume()
		}
	}()
	sources.Quiescer.Quiesce()
	if err := checkDiskCapture(ctx, sources.Diffs); err != nil {
		sources.Quiescer.Resume()
		return nil, err
	}
	backendsResumed := false
	defer func() {
		if !backendsResumed {
			sources.Quiescer.Resume()
		}
	}()

	dumpStart := time.Now()
	out, err := captureSandboxAtFreeze(ctx, sources, sink, true)
	if err != nil {
		return nil, err
	}
	closeErr := closeArtifactSink(sink)
	sinkOpen = false
	if closeErr != nil {
		return nil, closeErr
	}
	if resumeAfter {
		sources.Quiescer.Resume()
		backendsResumed = true
		if err := ch.Resume(); err != nil {
			return nil, fmt.Errorf("export CH resume: %w", err)
		}
		resumed = true
	}
	out.WallclockPauseMs = pausedAt.Sub(pauseStart).Milliseconds()
	out.WallclockDumpMs = time.Since(dumpStart).Milliseconds()
	if err := checkDiskCapture(ctx, sources.Diffs); err != nil {
		return nil, err
	}
	succeeded = true
	return out, nil
}

func closeArtifactSink(sink ArtifactSink) error {
	if sink == nil {
		return nil
	}
	if err := sink.Close(); err != nil {
		return fmt.Errorf("close artifact sink: %w", err)
	}
	return nil
}

func validateExportSources(sources ExportSources, sink ArtifactSink) error {
	if sources.SandboxID == "" {
		return errors.New("export: empty SandboxID")
	}
	if sources.APISock == "" {
		return errors.New("export: empty CH API socket")
	}
	if sources.PortableConfig == nil {
		return errors.New("export: immutable C0 is required")
	}
	if err := sources.PortableConfig.Validate(); err != nil {
		return fmt.Errorf("export C0: %w", err)
	}
	if len(sources.Diffs) != 1+len(sources.PortableConfig.Boot.Disks) {
		return fmt.Errorf("export: runtime disk count %d does not match C0 disk count %d", len(sources.Diffs), 1+len(sources.PortableConfig.Boot.Disks))
	}
	for i := range sources.Diffs {
		if sources.Diffs[i].SnapshotView == nil {
			return fmt.Errorf("export: disk %d has no SnapshotView", i)
		}
	}
	if sources.ParentSandboxRef != "" {
		ref, err := manifest.ParseRef(sources.ParentSandboxRef)
		if err != nil {
			return fmt.Errorf("export: parent Sandbox ref: %w", err)
		}
		if ref.Scheme == manifest.RefSchemeFile &&
			(filepath.IsAbs(ref.Path) || filepath.Base(ref.Path) != ref.Path || strings.ContainsAny(ref.Path, `/\`) || ref.Digest == "") {
			return errors.New("export: parent Sandbox file ref must be a basename content identity")
		}
	}
	merged := make([]bool, len(sources.Diffs))
	for i := range sources.Diffs {
		merged[i] = sources.Diffs[i].MergeBase != ""
	}
	if err := ValidateExportGraph(sources.PortableConfig, sources.ParentSandboxRef, merged); err != nil {
		return fmt.Errorf("export prospective C1: %w", err)
	}
	if sources.Quiescer == nil {
		return errors.New("export: nil backend Quiescer")
	}
	if sink == nil {
		return errors.New("export: nil artifact sink")
	}
	return nil
}

// ValidateExportGraph builds the exact prospective C1 shape with bounded,
// collision-free placeholder refs for data-disk outputs. It lets lifecycle
// handlers reject predictable layer/config limits before guest quiesce, while
// the final C1 is still built from the real content identities at the freeze
// point. Root payload identity remains self and therefore needs no placeholder.
func ValidateExportGraph(portable *config.PortableSandboxConfig, parentSandboxRef string, merged []bool) error {
	if portable == nil {
		return errors.New("portable export: nil C0")
	}
	if len(merged) != 1+len(portable.Boot.Disks) {
		return fmt.Errorf("portable export: got %d merge decisions, want %d", len(merged), 1+len(portable.Boot.Disks))
	}

	used := make(map[string]struct{}, config.MaxPortableArtifactRefs+len(portable.Boot.Disks)+1)
	addRoot := func(root *config.PortableRootConfig) {
		if root == nil {
			return
		}
		used[root.Base] = struct{}{}
		for _, raw := range root.BaseFromRefs {
			used[raw] = struct{}{}
		}
		if root.Overlay != nil {
			used[root.Overlay.Base] = struct{}{}
			for _, raw := range root.Overlay.BaseFromRefs {
				used[raw] = struct{}{}
			}
		}
	}
	addRoot(&portable.Boot.Root)
	for i := range portable.Boot.Disks {
		addRoot(&portable.Boot.Disks[i].PortableRootConfig)
	}
	used[parentSandboxRef] = struct{}{}

	dataRefs := make([]string, len(portable.Boot.Disks))
	for i := range dataRefs {
		for salt := 0; ; salt++ {
			digest := sha256.Sum256([]byte(fmt.Sprintf("sandboxer prospective C1 data disk %d salt %d", i, salt)))
			digestHex := fmt.Sprintf("%x", digest[:])
			// The plaintext FileSink identity is the longest V1 data-artifact
			// ref emitted by any sink. Manifest and Bundle refs are shorter, and
			// local HMAC uses a shorter digest-scheme name. Using this shape makes
			// the canonical-size check below an upper bound for every carrier.
			candidate := "file://" + digestHex + ".overlay@digest:" + digestHex
			if _, exists := used[candidate]; exists {
				continue
			}
			dataRefs[i] = candidate
			used[candidate] = struct{}{}
			break
		}
	}
	c1, err := portable.Exported(parentSandboxRef, dataRefs, merged)
	if err != nil {
		return err
	}
	_, err = config.MarshalPortableSandboxConfig(c1)
	return err
}

// captureSandboxAtFreeze assumes CH is paused and all backends are quiesced.
// Data disks are emitted first. The root SnapshotView is opened exactly once,
// wrapped with ZIP(sandbox.runtime.cfg=C1), and emitted last as E.
func captureSandboxAtFreeze(ctx context.Context, sources ExportSources, sink ArtifactSink, commitRoot bool) (_ *SandboxOutput, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &SandboxOutput{
		DataRefs:  make([]string, max(0, len(sources.Diffs)-1)),
		DataPaths: make([]string, max(0, len(sources.Diffs)-1)),
	}
	for i := 1; i < len(sources.Diffs); i++ {
		view, holes, cleanup, err := prepareDiskCapture(ctx, sources.Diffs[i], sources.LocalCodec, sources.LocalRequired, sources.MergeBaseOpener)
		if err != nil {
			return nil, fmt.Errorf("export data disk %d SnapshotView: %w", i-1, err)
		}
		ref, path, err := sink.AbsorbOverlay(ctx, view, holes)
		cleanupErr := cleanup()
		if err != nil {
			return nil, fmt.Errorf("export data disk %d: %w", i-1, errors.Join(err, cleanupErr))
		}
		if cleanupErr != nil {
			return nil, fmt.Errorf("export data disk %d cleanup: %w", i-1, cleanupErr)
		}
		out.DataRefs[i-1], out.DataPaths[i-1] = ref, path
	}
	merged := make([]bool, len(sources.Diffs))
	for i := range sources.Diffs {
		merged[i] = sources.Diffs[i].MergeBase != ""
	}
	c1, err := sources.PortableConfig.Exported(sources.ParentSandboxRef, out.DataRefs, merged)
	if err != nil {
		return nil, fmt.Errorf("export C1: %w", err)
	}
	runtimeConfig, err := config.MarshalPortableSandboxConfig(c1)
	if err != nil {
		return nil, err
	}
	rootView, rootHoles, rootCleanup, err := prepareDiskCapture(ctx, sources.Diffs[0], sources.LocalCodec, sources.LocalRequired, sources.MergeBaseOpener)
	if err != nil {
		return nil, fmt.Errorf("export root SnapshotView: %w", err)
	}
	cleanupPending := true
	defer func() {
		if cleanupPending {
			retErr = errors.Join(retErr, rootCleanup())
		}
	}()
	rootSource, err := newSeekerSource(rootView, rootHoles)
	if err != nil {
		return nil, fmt.Errorf("export root source: %w", err)
	}
	sandboxSource, err := sandboxfile.BuildSourceContext(ctx, rootSource, nil, runtimeConfig)
	if err != nil {
		return nil, err
	}
	out.SandboxRef, out.SandboxPath, err = sink.AbsorbSandbox(ctx, sandboxSource)
	cleanupErr := rootCleanup()
	cleanupPending = false
	if err != nil {
		return nil, fmt.Errorf("export Sandbox E: %w", errors.Join(err, cleanupErr))
	}
	if cleanupErr != nil {
		return nil, fmt.Errorf("export root cleanup: %w", cleanupErr)
	}
	if err := checkDiskCapture(ctx, sources.Diffs); err != nil {
		return nil, err
	}
	if commitRoot {
		if err := sink.CommitSandbox(ctx, out.SandboxRef, out.SandboxPath); err != nil {
			return nil, fmt.Errorf("commit Sandbox E: %w", err)
		}
	}
	if err := checkDiskCapture(ctx, sources.Diffs); err != nil {
		return nil, err
	}
	out.PortableConfig = c1
	return out, nil
}

func prepareDiskCapture(ctx context.Context, d DiskDiff, codec tarstream.Codec, required bool, opener MergeBaseOpener) (io.ReadSeeker, []sparse.Extent, func() error, error) {
	if d.SnapshotView == nil {
		return nil, nil, nil, errors.New("nil SnapshotView")
	}
	diff, holes, err := d.SnapshotView()
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := func() error { return nil }
	if d.MergeBase == "" {
		return diff, holes, cleanup, nil
	}
	size, err := seekerSize(diff)
	if err != nil {
		return nil, nil, nil, err
	}
	base, baseHoles, err := openMergeBaseWithOpener(ctx, d.MergeBase, size, codec, required, opener)
	if err != nil {
		return nil, nil, nil, err
	}
	merged, mergedHoles := mergeSparse(diff, holes, base, baseHoles, size)
	return merged, mergedHoles, base.Close, nil
}

func newSeekerSource(reader io.ReadSeeker, holes []sparse.Extent) (sparse.Source, error) {
	if reader == nil {
		return nil, errors.New("nil sparse reader")
	}
	size, err := seekerSize(reader)
	if err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, fmt.Errorf("invalid logical size %d", size)
	}
	return &seekerSource{rs: reader, size: uint64(size), holes: append([]sparse.Extent(nil), holes...)}, nil
}

// A disk may fail in the background after its last snapshot read, or while
// another disk/memory is being captured. Check all disks at publication edges.
func checkDiskCapture(ctx context.Context, disks []DiskDiff) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	for i, d := range disks {
		if d.CheckError != nil {
			if err := d.CheckError(); err != nil {
				return fmt.Errorf("snapshot disk %d: %w", i, err)
			}
		}
	}
	return nil
}

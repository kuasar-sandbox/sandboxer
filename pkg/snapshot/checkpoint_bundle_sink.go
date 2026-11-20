package snapshot

import (
	"context"
	"errors"
	"io"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

// CheckpointBundleSink has the same publication boundary as FileSink: Absorb
// commits an immutable local carrier, and Commit only publishes the operation
// alias. Separate carriers let a local checkpoint publish its small descriptor
// before its frozen memory payload is written. This does not flatten parents or
// change the portable, single-Bundle sink used by export.
type CheckpointBundleSink struct {
	outDir, sandboxID string
	cfg               *manifest.Config
	keyFn             ingest.CustomerKeyFunc
	admission         store.WriteAdmission
	logf              func(string, ...any)
}

func NewCheckpointBundleSink(outDir, sandboxID string, cfg *manifest.Config, keyFn ingest.CustomerKeyFunc, admission store.WriteAdmission, logf func(string, ...any)) (*CheckpointBundleSink, error) {
	// Validate configuration and output access before any VM is retired. The
	// probe owns no source and never publishes an artifact or alias.
	probe, err := NewPlannedBundleSink(outDir, sandboxID, cfg, keyFn, admission, nil, logf)
	if err != nil {
		return nil, err
	}
	if err := probe.Close(); err != nil {
		return nil, err
	}
	return &CheckpointBundleSink{outDir, sandboxID, cfg, keyFn, admission, logf}, nil
}

func (s *CheckpointBundleSink) absorb(ctx context.Context, source sparse.Source) (ref, path string, retErr error) {
	sink, err := NewPlannedBundleSink(s.outDir, s.sandboxID, s.cfg, s.keyFn, s.admission, nil, s.logf)
	if err != nil {
		return "", "", err
	}
	defer func() { retErr = errors.Join(retErr, sink.Close()) }()
	ref, _, err = sink.AbsorbImageSource(ctx, source)
	if err != nil {
		return "", "", err
	}
	key, err := manifest.ParseKeyRef(ref)
	if err != nil {
		return "", "", err
	}
	if err := sink.finalize(ctx, key, HexKey(key)+".bundle", ""); err != nil {
		return "", "", err
	}
	return CommittedArtifactRef(sink, ref, "")
}

func (s *CheckpointBundleSink) AbsorbOverlay(ctx context.Context, diff io.ReadSeeker, holes []sparse.Extent) (string, string, error) {
	size, err := seekerSize(diff)
	if err != nil {
		return "", "", err
	}
	return s.absorb(ctx, &seekerSource{rs: diff, size: uint64(size), holes: holes})
}
func (s *CheckpointBundleSink) AbsorbOverlaySource(ctx context.Context, source sparse.Source) (string, string, error) {
	return s.absorb(ctx, source)
}
func (s *CheckpointBundleSink) AbsorbImageSource(ctx context.Context, source sparse.Source) (string, string, error) {
	return s.absorb(ctx, source)
}
func (s *CheckpointBundleSink) AbsorbSandbox(ctx context.Context, source sparse.Source) (string, string, error) {
	return s.absorb(ctx, source)
}
func (s *CheckpointBundleSink) AbsorbSnapshot(ctx context.Context, source sparse.Source) (string, string, error) {
	return s.absorb(ctx, source)
}
func (s *CheckpointBundleSink) CommitSandbox(ctx context.Context, _ string, path string) error {
	return commitArtifactAlias(ctx, s.outDir, s.sandboxID, "sandbox", path)
}
func (s *CheckpointBundleSink) CommitSnapshot(ctx context.Context, _ string, path string) error {
	return commitArtifactAlias(ctx, s.outDir, s.sandboxID, "snapshot", path)
}
func (*CheckpointBundleSink) Close() error { return nil }

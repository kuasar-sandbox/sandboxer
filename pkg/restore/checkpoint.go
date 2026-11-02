package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
)

// prepareRestoreCheckpoint pins the complete source set before acquiring the
// target directory. The full release is registered before disk closers by Run;
// source pins are also released as soon as the target disks no longer need them.
func prepareRestoreCheckpoint(ctx context.Context, opts Options, source *openedSandboxSource) (_ Options, _ *openedSandboxSource, release func() error, retErr error) {
	var closes []func() error
	release = func() error {
		var err error
		for i := len(closes) - 1; i >= 0; i-- {
			err = errors.Join(err, closes[i]())
		}
		return err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, release())
		}
	}()
	storage, err := opts.borrowStorage()
	if err != nil {
		return opts, source, release, err
	}
	closes = append(closes, storage.Close)
	var pins *artifact.PinnedDiffCheckpoints
	if source.Root.Checkpoint != nil {
		if source.RelativeDir == "" {
			return opts, source, release, errors.New("restore: local Checkpoint has no carrier directory")
		}
		pins, err = storage.PinDiffCheckpoints(ctx, source.RelativeDir, source.Root.Checkpoint)
		if err != nil {
			return opts, source, release, err
		}
		closes = append(closes, pins.Close)
	}
	directory := filepath.Join(sandbox.DefaultBaseDir(opts.BaseRoot, opts.PathID), "checkpoint")
	if opts.EnableCheckpoint {
		owner, err := diffstore.LockDirectory(directory)
		if err != nil {
			return opts, source, release, err
		}
		closes = append(closes, owner.Close)
		opts.checkpointOwner = owner
	}
	if pins == nil {
		return opts, source, release, nil
	}
	if opts.EnableCheckpoint {
		ids, err := pins.ReopenIDs(directory, source.Root.Portable)
		if err != nil {
			return opts, source, release, err
		}
		if len(ids) != 0 {
			opts.checkpointReopen = ids
			opts.checkpointReleaseSources = pins.Close
			return opts, source, release, nil
		}
	}
	// Materialization is kept in a unique persistent-disk directory for the
	// running VM's lower graph. It is removed after every dependent disk closes.
	baseDir := sandbox.DefaultBaseDir(opts.BaseRoot, opts.PathID)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return opts, source, release, err
	}
	materialDir, err := os.MkdirTemp(baseDir, ".checkpoint-restore-")
	if err != nil {
		return opts, source, release, err
	}
	closes = append(closes, func() error { return os.RemoveAll(materialDir) })
	result, err := storage.MaterializePinnedCheckpoint(ctx, source.SelectedRef, opts.RefLocations, materialDir, pins)
	if err != nil {
		return opts, source, release, fmt.Errorf("restore: materialize Checkpoint: %w", err)
	}
	// MaterializePinnedCheckpoint includes Publisher.Close: all source reads
	// have finished and its output is independent. Keep the output directory,
	// readers, storage and target owner alive until the dependent disks close.
	if err := pins.Close(); err != nil {
		return opts, source, release, fmt.Errorf("restore: release Checkpoint sources: %w", err)
	}
	ref, err := manifest.ParseRef(result.SandboxRef)
	if err != nil {
		return opts, source, release, err
	}
	ref.Path = filepath.Join(materialDir, ref.Path)
	materialized, err := openReferencedSandbox(ctx, ref.String(), opts)
	if err != nil {
		return opts, source, release, err
	}
	closes = append(closes, materialized.Root.Close)
	return opts, materialized, release, nil
}

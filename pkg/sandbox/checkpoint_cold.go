package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

func prepareColdCheckpoint(ctx context.Context, opts RunOptions) (_ RunOptions, release func() error, retErr error) {
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
	var storage *artifact.ProcessStorage
	var pins *artifact.PinnedDiffCheckpoints
	var err error
	if opts.SourceBinding != nil && opts.SourceBinding.Checkpoint != nil {
		if opts.checkpointStorage != nil {
			storage = opts.checkpointStorage.Borrow()
		} else {
			storage, err = artifact.NewProcessStorageWithLocalAccess(opts.ManifestCfg, opts.CustomerKeyFn, opts.LocalCodec, opts.LocalRequired, opts.Fetcher)
			if err != nil {
				return opts, release, err
			}
		}
		closes = append(closes, storage.Close)
		pins, err = storage.PinDiffCheckpoints(ctx, opts.SourceBinding.RelativeDir, opts.SourceBinding.Checkpoint)
		if err != nil {
			return opts, release, err
		}
		closes = append(closes, pins.Close)
		opts.checkpointReleaseSources = pins.Close
	}
	directory := filepath.Join(DefaultBaseDir(opts.BaseRoot, opts.PathID), "checkpoint")
	if opts.EnableCheckpoint {
		opts.checkpointOwner, err = diffstore.LockDirectory(directory)
		if err != nil {
			return opts, release, err
		}
		closes = append(closes, opts.checkpointOwner.Close)
	}
	var ids []string
	if pins != nil && opts.EnableCheckpoint {
		ids, err = pins.ReopenIDs(directory, opts.PortableConfig)
		if err != nil {
			return opts, release, err
		}
	}
	if pins != nil && len(ids) == 0 {
		baseDir := DefaultBaseDir(opts.BaseRoot, opts.PathID)
		if err := os.MkdirAll(baseDir, 0755); err != nil {
			return opts, release, err
		}
		materialDir, err := os.MkdirTemp(baseDir, ".checkpoint-cold-")
		if err != nil {
			return opts, release, err
		}
		closes = append(closes, func() error { return os.RemoveAll(materialDir) })
		result, err := storage.MaterializePinnedCheckpoint(ctx, opts.SourceBinding.RuntimeRef, opts.RefLocations, materialDir, pins)
		if err != nil {
			return opts, release, err
		}
		// MaterializePinnedCheckpoint includes Publisher.Close. Its ordinary
		// artifacts now own all source bytes; keep their directory and storage,
		// but stop retaining the source versions for the runtime lifetime.
		if err := pins.Close(); err != nil {
			return opts, release, err
		}
		opts.checkpointReleaseSources = nil
		ref, err := manifest.ParseRef(result.SandboxRef)
		if err != nil {
			return opts, release, err
		}
		ref.Path = filepath.Join(materialDir, ref.Path)
		opened, err := storage.OpenFileWithLocations(ctx, ref.Path, ref, opts.RefLocations)
		if err != nil {
			return opts, release, err
		}
		root, err := sandboxfile.Open(ctx, opened)
		if err != nil {
			return opts, release, err
		}
		c0, err := materializedColdGraph(opts.Cfg, opts.PortableConfig, root.Portable)
		if closeErr := root.Close(); err != nil || closeErr != nil {
			return opts, release, errors.Join(err, closeErr)
		}
		opts.PortableConfig = c0
		portableRef := ref
		portableRef.Path = filepath.Base(ref.Path)
		opts.SourceBinding = &RunSourceBinding{SandboxRef: portableRef.String(), RuntimeRef: ref.String(), RelativeDir: materialDir}
		opts.Fetcher = storage.Fetcher()
		opts.BundleReader = nil
		opts.BundleFetcher = nil
	}
	if opts.EnableCheckpoint {
		opts.checkpointPlan, err = PrepareCheckpointDiskPlan(opts.Cfg, nil, directory, ids)
		if err != nil {
			return opts, release, err
		}
		if opts.PortableConfig != nil {
			parent := ""
			if opts.SourceBinding != nil {
				parent = opts.SourceBinding.SandboxRef
			}
			bases, err := opts.PortableConfig.Checkpointed(parent)
			if err != nil {
				return opts, release, err
			}
			if err := opts.checkpointPlan.Bind(bases); err != nil {
				return opts, release, err
			}
		}
	}
	return opts, release, nil
}

func bindRuntimeDiskGraph(cfg *config.SandboxConfig, portable *config.PortableSandboxConfig, source *RunSourceBinding) error {
	if portable != nil && portable.IsCheckpoint() {
		return config.BindCheckpointDiskGraph(cfg, source.RelativeDir)
	}
	return config.BindPortableDiskGraph(cfg, source.RuntimeRef, source.RelativeDir)
}

// Materialization changes only immutable disk bindings. The effective C0 and
// runtime already contain the caller's persistent and instance-only overrides.
func materializedColdGraph(runtime *config.SandboxConfig, effective, materialized *config.PortableSandboxConfig) (*config.PortableSandboxConfig, error) {
	c0, err := effective.WithMaterializedDisks(materialized)
	if err != nil {
		return nil, err
	}
	if len(runtime.Boot.Disks) != len(c0.Boot.Disks) {
		return nil, errors.New("materialized cold disk topology mismatch")
	}
	bind := func(dst *config.RootConfig, src config.PortableRootConfig) {
		dst.Base, dst.BaseFromRefs = src.Base, append([]string(nil), src.BaseFromRefs...)
		if src.Overlay != nil {
			if dst.Overlay == nil {
				dst.Overlay = &config.OverlayConfig{}
			}
			dst.Overlay.Base, dst.Overlay.BaseFromRefs = src.Overlay.Base, append([]string(nil), src.Overlay.BaseFromRefs...)
		}
	}
	bind(&runtime.Boot.Root, c0.Boot.Root)
	for i, d := range c0.Boot.Disks {
		bind(&runtime.Boot.Disks[i].RootConfig, d.PortableRootConfig)
	}
	return c0, nil
}

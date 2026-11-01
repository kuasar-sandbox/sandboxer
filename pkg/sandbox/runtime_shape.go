package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

// DeriveRuntimeOptions supplies the artifact access, source provenance, and
// host paths needed to derive the same disk shape that Launch will use.
// Launch contributes access and provenance only; cfg remains authoritative for
// the effective runtime configuration and root/data-disk layout.
type DeriveRuntimeOptions struct {
	Launch LaunchSpec
	// EnableCheckpoint selects the same versioned writer policy as RuntimeSpec.
	// Candidate source writers are inspected without taking directory ownership;
	// Launch rechecks them after acquiring the owner. No writer is opened here.
	EnableCheckpoint bool
	// SandboxID enables inspection of the default root/data DIFF paths. BaseRoot
	// and PathID then default exactly as in StartRuntime. Without SandboxID no
	// default DIFF is inspected: a future generated identity is not yet known.
	// A supplied PathID is sufficient to identify the checkpoint directory,
	// whose writer filenames do not use SandboxID. Explicit checkpoint DIFFs
	// require a known directory. These values are retained in RuntimeSpec.
	BaseRoot, SandboxID, PathID string
}

// DeriveRuntimeSpec derives a host-bound cold configuration using storage and
// locations. Use DeriveRuntimeSpecWithOptions for a portable source, a scoped
// Bundle fetcher, checkpoint versions, or an existing default DIFF.
func DeriveRuntimeSpec(ctx context.Context, cfg *config.SandboxConfig, storage *artifact.ProcessStorage, locations config.RefLocations) (RuntimeSpec, error) {
	if storage == nil {
		return RuntimeSpec{}, errors.New("derive runtime spec: config and storage required")
	}
	return DeriveRuntimeSpecWithOptions(ctx, cfg, DeriveRuntimeOptions{Launch: LaunchSpec{Storage: storage, RefLocations: locations}})
}

// DeriveRuntimeSpecWithOptions reads the disk capacities needed by StartRuntime.
// It clones cfg before binding self and relative source refs, so cfg can still
// be used to construct LaunchSpec without binding the graph a second time.
//
// Storage, Fetcher, and Bundle selectors are borrowed, as in Launch. When Storage
// is absent, Credentials creates storage owned only by this call; without either
// one, local-only storage is used and an explicit Fetcher is still honored. The
// caller keeps source-scoped Fetchers and their Bundle readers open for this call
// and for any later Launch that reuses them. All temporary readers and checkpoint
// version pins are released before return. Derivation does not start a VM, open
// a writable DIFF, materialize a checkpoint, or create target run/base directories.
func DeriveRuntimeSpecWithOptions(ctx context.Context, cfg *config.SandboxConfig, opts DeriveRuntimeOptions) (_ RuntimeSpec, retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return RuntimeSpec{}, err
	}
	clone, err := cloneSDKConfig(cfg)
	if err != nil {
		return RuntimeSpec{}, err
	}
	opts, err = normalizeDeriveRuntimeOptions(opts)
	if err != nil {
		return RuntimeSpec{}, err
	}
	if opts.Launch.Storage != nil && opts.Launch.Credentials != nil {
		return RuntimeSpec{}, errors.New("derive runtime spec: Storage and Credentials are mutually exclusive")
	}
	if opts.Launch.Storage == nil {
		var credentials ArtifactCredentials
		if opts.Launch.Credentials != nil {
			credentials = *opts.Launch.Credentials
		}
		opts.Launch.ManifestConfig = credentials.Config
		opts.Launch.Storage, err = artifact.NewProcessStorageWithCustomerKey(credentials.Config, credentials.CustomerKey)
		if err != nil {
			return RuntimeSpec{}, err
		}
		defer func() { retErr = errors.Join(retErr, opts.Launch.Storage.Close()) }()
	}
	if source := opts.Launch.SourceBinding; source != nil {
		if source.Checkpoint != nil {
			if err := source.Checkpoint.Validate(opts.Launch.PortableConfig); err != nil {
				return RuntimeSpec{}, fmt.Errorf("derive checkpoint shape: %w", err)
			}
		}
		if err := bindRuntimeDiskGraph(clone, opts.Launch.PortableConfig, source); err != nil {
			return RuntimeSpec{}, fmt.Errorf("Sandbox source binding: %w", err)
		}
	}
	var checkpointSizes []int64
	var pins *artifact.PinnedDiffCheckpoints
	if source := opts.Launch.SourceBinding; source != nil && source.Checkpoint != nil {
		pins, err = opts.Launch.Storage.PinDiffCheckpoints(ctx, source.RelativeDir, source.Checkpoint)
		if err != nil {
			return RuntimeSpec{}, fmt.Errorf("derive checkpoint shape: %w", err)
		}
		defer func() { retErr = errors.Join(retErr, pins.Close()) }()
		checkpointSizes, err = runtimeCheckpointSizes(clone, opts.Launch.PortableConfig, pins)
		if err != nil {
			return RuntimeSpec{}, err
		}
	}
	if opts.EnableCheckpoint {
		if err := deriveCheckpointTargets(clone, opts, pins); err != nil {
			return RuntimeSpec{}, fmt.Errorf("derive checkpoint shape: %w", err)
		}
	}
	return deriveBoundRuntimeSpec(ctx, clone, opts, checkpointSizes)
}

func normalizeDeriveRuntimeOptions(opts DeriveRuntimeOptions) (DeriveRuntimeOptions, error) {
	if opts.SandboxID != "" {
		if err := validateSandboxID(opts.SandboxID); err != nil {
			return opts, err
		}
		pathID, err := ResolvePathID(opts.SandboxID, opts.PathID)
		if err != nil {
			return opts, err
		}
		opts.PathID = pathID
	} else if opts.PathID != "" {
		if err := ValidatePathID(opts.PathID); err != nil {
			return opts, err
		}
	}
	if opts.BaseRoot == "" && (opts.SandboxID != "" || opts.EnableCheckpoint && opts.PathID != "") {
		opts.BaseRoot = DefaultBaseRoot
	}
	return opts, nil
}

// deriveCheckpointTargets mirrors PrepareCheckpointDiskPlan's selection on an
// owned config clone. New writers have no existing file and use template/base
// capacity. A current same-directory source can instead reuse its exact DIFF,
// whose capacity takes precedence over a template. This is a read-only hint:
// prepareColdCheckpoint repeats ReopenIDs while holding directory ownership.
func deriveCheckpointTargets(cfg *config.SandboxConfig, opts DeriveRuntimeOptions, pins *artifact.PinnedDiffCheckpoints) error {
	var directory string
	var reopen []string
	if opts.PathID != "" {
		var err error
		directory, err = filepath.Abs(filepath.Join(DefaultBaseDir(opts.BaseRoot, opts.PathID), "checkpoint"))
		if err != nil {
			return err
		}
		if pins != nil {
			if info, err := os.Stat(directory); err == nil {
				if !info.IsDir() {
					return errors.New("checkpoint target is not a directory")
				}
				reopen, err = pins.ReopenIDs(directory, opts.Launch.PortableConfig)
				if err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	roots := []*config.RootConfig{&cfg.Boot.Root}
	for i := range cfg.Boot.Disks {
		roots = append(roots, &cfg.Boot.Disks[i].RootConfig)
	}
	for i, root := range roots {
		target := &root.Diff
		if root.Overlay != nil {
			target = &root.Overlay.Diff
		}
		if *target != "" && directory == "" {
			return errors.New("explicit checkpoint DIFF requires SandboxID or PathID")
		}
		if err := validateCheckpointDiffTarget(*target, directory, len(reopen) != 0); err != nil {
			return err
		}
		*target = ""
		if len(reopen) != 0 {
			*target = "file://" + filepath.Join(directory, "diffs", reopen[i]+".diff")
		}
	}
	return nil
}

// deriveBoundRuntimeSpec consumes an already-bound configuration. Run has
// already bound/materialized its source and selected checkpoint writers, so it
// calls this core directly with explicit paths and no additional source pins.
// Callers supply a config clone when the returned spec must not alias cfg.
func deriveBoundRuntimeSpec(ctx context.Context, cfg *config.SandboxConfig, opts DeriveRuntimeOptions, checkpointSizes []int64) (RuntimeSpec, error) {
	var err error
	opts, err = normalizeDeriveRuntimeOptions(opts)
	if err != nil {
		return RuntimeSpec{}, err
	}
	launch := opts.Launch
	opener := FileStreamOpener(func(ctx context.Context, path string, ref manifest.Ref) (fetch.Stream, error) {
		return openLaunchFile(ctx, launch, path, ref, launch.RefLocations)
	})
	var diffOpts []diffstore.DiskOption
	if launchCodec(launch) != nil {
		keyFn := launchKey(launch)
		if keyFn == nil {
			return RuntimeSpec{}, errors.New("diff encryption requires customer key")
		}
		key, err := keyFn()
		if err != nil {
			return RuntimeSpec{}, err
		}
		diffOpts = append(diffOpts, diffstore.WithDiffEncryption(key, launchRequired(launch)))
		clear(key[:])
	}
	defaultBaseDir := DefaultBaseDir(opts.BaseRoot, opts.PathID)
	derive := func(root *config.RootConfig, name string, index int) (RuntimeDiskSpec, error) {
		d := RuntimeDiskSpec{Name: name, Overlay: root.Overlay != nil}
		if err := ctx.Err(); err != nil {
			return d, err
		}
		base, lowers := root.Base, root.BaseFromRefs
		diffURI, template := root.Diff, root.DiffTemplate
		if root.Overlay != nil {
			reader, _, err := OpenRootImageBlockReaderWithOpener(ctx, root.Base, launchFetcher(launch), launch.RefLocations, launchCodec(launch), launchRequired(launch), opener)
			if err != nil {
				return d, err
			}
			d.BaseCapacity = reader.Size()
			if err := reader.Close(); err != nil {
				return d, err
			}
			base, lowers = root.Overlay.Base, root.Overlay.BaseFromRefs
			diffURI, template = root.Overlay.Diff, root.Overlay.DiffTemplate
		}
		var baseSize int64
		if base != "" {
			reader, _, err := OpenLayeredBlockReaderWithOpener(ctx, append([]string{base}, lowers...), launchFetcher(launch), launch.RefLocations, launchCodec(launch), launchRequired(launch), opener)
			if err != nil {
				return d, err
			}
			baseSize = reader.Size()
			if err := reader.Close(); err != nil {
				return d, err
			}
		}
		if checkpointSizes != nil {
			if baseSize > checkpointSizes[index] {
				return d, fmt.Errorf("checkpoint disk %q logical size %d is smaller than immutable base size %d", name, checkpointSizes[index], baseSize)
			}
			baseSize = checkpointSizes[index]
		}
		if diffURI == "" && !opts.EnableCheckpoint && opts.SandboxID != "" {
			if index == 0 {
				diffURI = DefaultDiffURIForBaseDir(defaultBaseDir, opts.SandboxID)
			} else {
				diffURI = DefaultDiskDiffURI(defaultBaseDir, opts.SandboxID, fmt.Sprintf("disk%d", index-1))
			}
		}
		capacity, err := runtimeWritableCapacity(diffURI, template, baseSize, diffOpts)
		d.WritableCapacity = capacity
		return d, err
	}
	root, err := derive(&cfg.Boot.Root, "root", 0)
	if err != nil {
		return RuntimeSpec{}, fmt.Errorf("derive root shape: %w", err)
	}
	shapes := []RuntimeDiskSpec{root}
	for i, disk := range cfg.Boot.Disks {
		shape, err := derive(&disk.RootConfig, disk.Name, i+1)
		if err != nil {
			return RuntimeSpec{}, fmt.Errorf("derive disk %q shape: %w", disk.Name, err)
		}
		shapes = append(shapes, shape)
	}
	if err := ctx.Err(); err != nil {
		return RuntimeSpec{}, err
	}
	spec := runtimeSpecFromConfig(cfg, shapes)
	spec.EnableCheckpoint = opts.EnableCheckpoint
	spec.BaseRoot, spec.SandboxID, spec.PathID = opts.BaseRoot, opts.SandboxID, opts.PathID
	return spec, nil
}

func runtimeWritableCapacity(diffURI, template string, baseSize int64, opts []diffstore.DiskOption) (int64, error) {
	if diffURI != "" {
		scheme, path, ok := config.SchemeAndPath(diffURI)
		if !ok || scheme != "file" {
			return 0, fmt.Errorf("invalid diff URI %q", diffURI)
		}
		// Share Launch's existing/template/base precedence, including rejection
		// of empty targets and stat errors. PrepareDiff never creates a writer.
		init, err := PrepareDiff(path, template, baseSize)
		if err != nil {
			return 0, err
		}
		if init.Existing {
			return diffstore.DiffSourceCapacity(path, false, opts...)
		}
	}
	if template != "" {
		scheme, path, ok := config.SchemeAndPath(template)
		if !ok || scheme != "file" {
			return 0, fmt.Errorf("invalid diff template %q", template)
		}
		return diffstore.DiffSourceCapacity(path, true, opts...)
	}
	if baseSize > 0 {
		if err := diffstore.ValidateWritableSize(baseSize); err != nil {
			return 0, err
		}
		return baseSize, nil
	}
	return 0, errors.New("no source for writable disk capacity")
}

func runtimeCheckpointSizes(cfg *config.SandboxConfig, portable *config.PortableSandboxConfig, pins *artifact.PinnedDiffCheckpoints) ([]int64, error) {
	if len(cfg.Boot.Disks) != len(portable.Boot.Disks) {
		return nil, errors.New("derive checkpoint shape: disk topology mismatch")
	}
	if pins.Root.Info().BaseID != artifact.DiffCheckpointBaseID(portable.Boot.Root) {
		return nil, errors.New("derive checkpoint shape: root base binding mismatch")
	}
	sizes := []int64{int64(pins.Root.Size())}
	for i, disk := range cfg.Boot.Disks {
		base := portable.Boot.Disks[i]
		if disk.Name != base.Name {
			return nil, errors.New("derive checkpoint shape: disk topology mismatch")
		}
		view := pins.Disks[disk.Name]
		if view == nil || view.Info().BaseID != artifact.DiffCheckpointBaseID(base.PortableRootConfig) {
			return nil, fmt.Errorf("derive checkpoint shape: disk %q base binding mismatch", disk.Name)
		}
		sizes = append(sizes, int64(view.Size()))
	}
	return sizes, nil
}

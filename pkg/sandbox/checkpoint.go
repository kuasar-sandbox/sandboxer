package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// checkpointLayer resolves the actual selected carrier before deciding whether
// it belongs to this VM's checkpoint. Located sources always mark an external
// boundary, even when their files happen to be readable on this host.
func checkpointLayer(ctx context.Context, raw, relativeDir string, opts RunOptions, memory bool) (string, bool, error) {
	if opts.checkpointDir == "" || raw == "" {
		return "", false, nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", false, err
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		lookup := sandboxBundleLookup(opts)
		if memory {
			lookup = memoryBundleLookup(opts)
		}
		if lookup.err != nil {
			return "", false, lookup.err
		}
		if lookup.fetcher == nil {
			return "", false, nil
		}
		key, err := manifest.ParseKeyRef(ref.Path)
		if err != nil {
			return "", false, err
		}
		local := false
		err = readretry.Do(ctx, func() error {
			selected, err := lookup.fetcher.SelectManifest(ctx, key)
			if err == nil {
				local = selected.Reader != nil
			}
			return err
		})
		if err != nil || !local {
			return "", false, err
		}
		physical, found, err := bundleManifestMergeRefWithLookup(ctx, raw, opts, lookup)
		if err != nil || !found {
			return "", false, err
		}
		ref, err = manifest.ParseRef(physical)
		if err != nil {
			return "", false, err
		}
	}
	if ref.Scheme != manifest.RefSchemeFile || ref.Location != "" {
		return "", false, nil
	}
	path, err := opts.RefLocations.ResolveFile(ref, relativeDir)
	if err != nil {
		return "", false, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	owned, err := filepath.Abs(opts.checkpointDir)
	if err != nil {
		return "", false, err
	}
	// Do not inspect external paths merely to see whether they can be merged.
	if filepath.Dir(path) != owned {
		return "", false, nil
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, err
	}
	realDir, err := filepath.EvalSymlinks(owned)
	if err != nil {
		return "", false, err
	}
	if realDir != owned || filepath.Dir(real) != owned {
		return "", false, nil
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || ref.Digest == "" {
		return "", false, fmt.Errorf("checkpoint layer lacks regular content identity")
	}
	ref.Path = real
	return ref.String(), true, nil
}

func checkpointPrefix(ctx context.Context, refs []string, runtimeTop, relativeDir string, opts RunOptions, memory bool) ([]string, error) {
	var prefix []string
	for i, raw := range refs {
		if i == 0 && runtimeTop != "" {
			// A located logical top cannot become local just because RuntimeRef is an
			// absolute host path prepared for its reader.
			logical, err := manifest.ParseRef(raw)
			if err != nil {
				return nil, err
			}
			if logical.Location != "" {
				break
			}
			raw = runtimeTop
		}
		resolved, local, err := checkpointLayer(ctx, raw, relativeDir, opts, memory)
		if err != nil {
			return nil, err
		}
		if !local {
			break
		}
		prefix = append(prefix, resolved)
	}
	return prefix, nil
}

type checkpointStream struct {
	fetch.Stream
	once sync.Once
	err  error
}

func (s *checkpointStream) Close() error {
	s.once.Do(func() { s.err = s.Stream.Close() })
	return s.err
}

type checkpointSourceStream struct {
	sparse.Source
	close func() error
}

func (s *checkpointSourceStream) Close() error { return s.close() }

type memoryHistory struct {
	refs                                  []string
	memory                                *checkpointStream
	historical                            fetch.Stream
	fullMemory                            sparse.Source
	proofCandidates                       sparse.Source // bitmap-only union, diagnostics only; never used to trim
	base                                  string
	configJSON, stateJSON, snapshotConfig []byte
}

func (h *memoryHistory) Close() error {
	if h == nil {
		return nil
	}
	if h.historical != nil {
		return h.historical.Close()
	}
	if h.memory != nil {
		return h.memory.Close()
	}
	return nil
}

// validateMergeBase borrows the composed memory for map-only preflight. The
// validator closes its view; history retains ownership for Take and cleanup.
func (h *memoryHistory) validateMergeBase(ctx context.Context, size uint64) error {
	return snapshot.ValidateMergeBaseWithOpener(ctx, h.base, int64(size), nil, false,
		func(context.Context, string) (fetch.Stream, error) {
			return &checkpointSourceStream{Source: h.memory, close: func() error { return nil }}, nil
		})
}

// prepareMemoryHistory reads only immutable host artifacts. It never reads the
// guest memfd, so forming historical memory cannot fault pages into this round's
// working set. The returned source streams straight into the final sink before
// freeze; there is no full-image buffer or intermediate artifact.
func prepareMemoryHistory(ctx context.Context, opts RunOptions, merge bool, size uint64) (_ *memoryHistory, retErr error) {
	h := &memoryHistory{}
	b := opts.MemoryBinding
	if b == nil {
		return h, nil
	}
	all := prependRef(b.SnapshotRef, b.FromRefs)
	prefix, err := checkpointPrefix(ctx, all, b.RuntimeRef, b.RelativeDir, opts, true)
	if err != nil {
		return nil, err
	}
	if len(prefix) == 0 {
		h.refs, err = memoryRefsForSnapshot(b)
		return h, err
	}
	h.refs = append([]string(nil), all[len(prefix):]...)
	var layers []fetch.Stream
	var candidates []fetch.Stream
	var diagnostic []fetch.Stream
	unproved := false
	var top *snapshotfile.Root
	defer func() {
		if retErr != nil {
			for _, layer := range layers {
				retErr = errors.Join(retErr, layer.Close())
			}
		}
	}()
	for i, raw := range prefix {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return nil, err
		}
		opened, err := artifact.OpenFileWithLocations(ctx, ref.Path, ref, opts.ManifestCfg, opts.CustomerKeyFn, opts.Fetcher, opts.RefLocations, opts.LocalCodec, opts.LocalRequired)
		if err != nil {
			return nil, err
		}
		root, err := snapshotfile.Open(ctx, opened)
		if err != nil {
			return nil, err
		}
		layers = append(layers, root.Memory)
		cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
		if err != nil {
			return nil, err
		}
		hinted, err := snapshot.DirtyMemory(root.Memory, root.MemoryDirty)
		if err != nil {
			return nil, err
		}
		diagnostic = append(diagnostic, &checkpointSourceStream{Source: hinted, close: func() error { return nil }})
		candidate := hinted
		if root.MemoryDirty != nil && !memoryParentViewEqual(cfg.FromRefs, all[i+1:]) {
			candidate = root.Memory
			unproved = true
			if opts.memoryLogf != nil {
				opts.memoryLogf("dirty memory fallback: parent view unproved for history layer %s; retaining all explicit content", raw)
			}
		}
		candidates = append(candidates, &checkpointSourceStream{Source: candidate, close: func() error { return nil }})
		if !merge && len(prefix) == 1 && root.MemoryDirty == nil {
			// Legacy/all-dirty single parent is already the desired H.
			if err := root.Close(); err != nil {
				return nil, err
			}
			h.refs, err = memoryRefsForSnapshot(b)
			return h, err
		}
		if root.Memory.Size() != size {
			return nil, fmt.Errorf("checkpoint memory layer size %d, expected %d", root.Memory.Size(), size)
		}
		if i == 0 {
			top = root
		}
	}
	full := fetch.NewLayered(layers...)
	selected, err := snapshot.SelectMemory(full, fetch.NewLayered(candidates...))
	if err != nil {
		return nil, err
	}
	h.fullMemory = full
	if unproved {
		h.proofCandidates = fetch.NewLayered(diagnostic...)
	}
	h.memory = &checkpointStream{Stream: &checkpointSourceStream{Source: selected, close: full.Close}}
	if merge {
		h.base = prefix[0]
		if err := h.validateMergeBase(ctx, size); err != nil {
			return nil, err
		}
		return h, nil
	}
	cfg, err := snapshot.ParseConfig(top.SnapshotConfig)
	if err != nil {
		return nil, err
	}
	// Historical S contributes memory only. Its execution state is never selected
	// by restore. Keep a strict existing S layout without retaining old disk edges.
	cfg.FromRefs = append([]string(nil), h.refs...)
	encoded, err := snapshot.MarshalConfig(cfg)
	if err != nil {
		return nil, err
	}
	source, err := snapshotfile.BuildSource(h.memory, top.ConfigJSON, top.StateJSON, encoded)
	if err != nil {
		return nil, err
	}
	h.configJSON, h.stateJSON, h.snapshotConfig = top.ConfigJSON, top.StateJSON, encoded
	h.historical = &checkpointSourceStream{Source: source, close: h.memory.Close}
	return h, nil
}

// prepareCheckpointDisks flattens the full same-device local prefix regardless
// of the memory working-set flag. Only the writable chain is changed; an EROFS
// immutable base remains a separate device and dependency.
func prepareCheckpointDisks(ctx context.Context, opts RunOptions, disks []SnapDiskRef) (RunOptions, []snapshot.DiskDiff, []bool, snapshot.MergeBaseOpener, error) {
	c0, err := opts.PortableConfig.Clone()
	if err != nil {
		return opts, nil, nil, nil, err
	}
	if len(disks) != 1+len(c0.Boot.Disks) {
		return opts, nil, nil, nil, errors.New("checkpoint disk count does not match C0")
	}
	diffs := make([]snapshot.DiskDiff, len(disks))
	merged := make([]bool, len(disks))
	parents := make(map[string][]string)
	pureGroups := make(map[string]func(context.Context) (fetch.Stream, error))
	baseOpen := newDiskMergeBaseOpener(opts)
	opener := func(ctx context.Context, raw string) (fetch.Stream, error) {
		if group, ok := pureGroups[raw]; ok {
			return group(ctx)
		}
		refs, ok := parents[raw]
		if !ok {
			return baseOpen(ctx, raw)
		}
		layers := make([]fetch.Stream, 0, len(refs))
		for _, ref := range refs {
			stream, err := baseOpen(ctx, ref)
			if err != nil {
				for _, s := range layers {
					err = errors.Join(err, s.Close())
				}
				return nil, err
			}
			layers = append(layers, stream)
		}
		return fetch.NewLayered(layers...), nil
	}
	for i, d := range disks {
		if i > 0 && c0.Boot.Disks[i-1].Unbound() && d.Slot && d.SnapshotView == nil {
			continue
		}
		if d.SnapshotView == nil || d.Size <= 0 {
			return opts, nil, nil, nil, fmt.Errorf("disk %d has invalid SnapshotView/size", i)
		}
		diffs[i] = snapshot.DiskDiff{Path: d.DiffPath, Owned: d.OwnedDiff, SnapshotView: d.SnapshotView, WaitReady: d.WaitReady, CheckError: d.CheckError, DiffID: d.DiffID, Checkpoint: d.Checkpoint, DeleteCheckpoint: d.DeleteCheckpoint, WaitMaterialized: d.WaitMaterialized, Export: d.Export}
		if opts.namedCheckpoint || c0.IsCheckpoint() && (i == 0 || !c0.Boot.Disks[i-1].PureBlock) {
			continue
		}
		raw, _, err := currentDiskParentBinding(opts, i)
		if err != nil {
			return opts, nil, nil, nil, err
		}
		root := &c0.Boot.Root
		if i > 0 {
			root = &c0.Boot.Disks[i-1].PortableRootConfig
		}
		lowers := &root.BaseFromRefs
		if root.Overlay != nil {
			lowers = &root.Overlay.BaseFromRefs
		}
		if raw == "" {
			continue
		}
		logical := raw
		if i == 0 && opts.SourceBinding != nil {
			logical = opts.SourceBinding.SandboxRef
		}
		if i > 0 {
			logical = root.Base
			if root.Overlay != nil {
				logical = root.Overlay.Base
			}
		}
		refs := append([]string{logical}, (*lowers)...)
		relativeDir := ""
		if opts.SourceBinding != nil {
			relativeDir = opts.SourceBinding.RelativeDir
		}
		// Pure disk layers have an explicit block type. Never probe the tail as
		// a Sandbox artifact, even when the guest happens to write ZIP bytes there.
		if i > 0 && c0.Boot.Disks[i-1].PureBlock {
			if len(*lowers) < config.MaxPortableLayerRefs {
				continue
			}
			actual := opts.Cfg.Boot.Disks[i-1].SourceDefinition()
			key := fmt.Sprintf("pure-disk-%d", i)
			pureGroups[key] = func(ctx context.Context) (fetch.Stream, error) {
				access, err := artifact.NewProcessStorageWithLocalAccess(opts.ManifestCfg, opts.CustomerKeyFn, opts.LocalCodec, opts.LocalRequired, opts.Fetcher)
				if err != nil {
					return nil, err
				}
				var streams []fetch.Stream
				fail := func(e error) (fetch.Stream, error) {
					for _, s := range streams {
						e = errors.Join(e, s.Close())
					}
					return nil, errors.Join(e, access.Close())
				}
				for _, layer := range append([]string{actual.Base}, actual.BaseFromRefs...) {
					ref, err := manifest.ParseRef(layer)
					if err != nil {
						return fail(err)
					}
					var stream fetch.Stream
					if ref.Scheme == manifest.RefSchemeManifest {
						stream, _, err = OpenManifestStream(ctx, ref.Path, access.Fetcher())
					} else {
						var path string
						path, err = opts.RefLocations.ResolveFile(ref, relativeDir)
						if err == nil {
							stream, err = access.OpenFileWithLocations(ctx, path, ref, opts.RefLocations)
						}
					}
					if err != nil {
						return fail(err)
					}
					streams = append(streams, stream)
				}
				size := uint64(d.Size)
				holes, err := sparse.NewSource(bytes.NewReader(nil), size, []sparse.Extent{{Size: size}})
				if err != nil {
					return fail(err)
				}
				streams = append(streams, &checkpointSourceStream{Source: holes, close: func() error { return nil }})
				combined := fetch.NewLayered(streams...)
				return &checkpointSourceStream{Source: combined, close: func() error { return errors.Join(combined.Close(), access.Close()) }}, nil
			}
			diffs[i].MergeBase = key
			merged[i] = true
			*lowers = nil
			// Materialization adds the captured top to this base graph. Its
			// compacted payload already contains all original parents.
			if c0.IsCheckpoint() {
				root.Base = ""
			}
			continue
		}
		prefix, err := checkpointPrefix(ctx, refs, raw, relativeDir, opts, false)
		if err != nil {
			return opts, nil, nil, nil, err
		}
		if len(prefix) == 0 {
			continue
		}
		key := fmt.Sprintf("checkpoint-disk-%d", i)
		parents[key] = prefix
		diffs[i].MergeBase = key
		merged[i] = true
		*lowers = append([]string(nil), (*lowers)[len(prefix)-1:]...)
		size := d.Size
		if err := snapshot.ValidateMergeBaseWithOpener(ctx, key, size, opts.LocalCodec, opts.LocalRequired, opener); err != nil {
			return opts, nil, nil, nil, err
		}
	}
	if opts.namedCheckpoint {
		parent := ""
		if opts.SourceBinding != nil {
			parent = opts.SourceBinding.SandboxRef
		}
		c0, err = c0.Checkpointed(parent)
		if err != nil {
			return opts, nil, nil, nil, err
		}
	}
	opts.PortableConfig = c0
	return opts, diffs, merged, opener, nil
}

// Reused dependencies already in this checkpoint need no repack, temporary
// full-image copy or rehash. Keep their precise physical selector. This is only
// an output optimization; cleanup authority still belongs to committed Pause.
func retainedCheckpointDependency(ctx context.Context, dep artifactDependency, opts RunOptions, outputDir string) (string, bool, error) {
	if opts.checkpointDir == "" || outputDir == "" {
		return "", false, nil
	}
	out, err := filepath.Abs(outputDir)
	if err != nil {
		return "", false, err
	}
	owned, err := filepath.Abs(opts.checkpointDir)
	if err != nil {
		return "", false, err
	}
	if out != owned {
		return "", false, nil
	}
	raw, relative := dep.raw, ""
	memory := dep.role == dependencyMemorySnapshot
	if memory && opts.MemoryBinding != nil {
		relative = opts.MemoryBinding.RelativeDir
		if raw == opts.MemoryBinding.SnapshotRef && opts.MemoryBinding.RuntimeRef != "" {
			logical, err := manifest.ParseRef(raw)
			if err != nil {
				return "", false, err
			}
			if logical.Location != "" {
				return "", false, nil
			}
			raw = opts.MemoryBinding.RuntimeRef
		}
	} else if opts.SourceBinding != nil {
		relative = opts.SourceBinding.RelativeDir
		if raw == opts.SourceBinding.SandboxRef && opts.SourceBinding.RuntimeRef != "" {
			logical, err := manifest.ParseRef(raw)
			if err != nil {
				return "", false, err
			}
			if logical.Location != "" {
				return "", false, nil
			}
			raw = opts.SourceBinding.RuntimeRef
		}
	}
	physical, local, err := checkpointLayer(ctx, raw, relative, opts, memory)
	if err != nil || !local {
		return "", false, err
	}
	ref, err := manifest.ParseRef(physical)
	if err != nil {
		return "", false, err
	}
	ref.Path = filepath.Base(ref.Path)
	return ref.String(), true, nil
}

// Reference identities bind complete S content and ordered parent declarations.
// Physical path/location rewrites do not change identity. An unqualified file
// or a different identity scheme cannot establish equivalence. This compares
// the entire declared suffix, never a shared parent or sibling relationship.
func memoryParentViewEqual(declared, actual []string) bool {
	if len(declared) != len(actual) {
		return false
	}
	identity := func(raw string) string {
		r, err := manifest.ParseRef(raw)
		if err != nil {
			return ""
		}
		if r.Scheme == manifest.RefSchemeManifest {
			return "manifest:" + r.Path
		}
		if r.Digest == "" {
			return ""
		}
		return r.DigestScheme + ":" + r.Digest
	}
	for i, raw := range declared {
		id := identity(raw)
		if id == "" || id != identity(actual[i]) {
			return false
		}
	}
	return true
}

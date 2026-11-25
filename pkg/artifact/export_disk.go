package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

var (
	ErrDiskNotFound = errors.New("data disk not found")
	ErrDiskUnbound  = errors.New("data disk is unbound")
	ErrDiskLayout   = errors.New("disk export requires a single writable filesystem")
)

type ExportDiskRequest struct {
	SourceRef    string
	SourceDir    string
	DiskName     string
	RefLocations config.RefLocations
	OutputDir    string
	Upload       bool
	// ToRefLocation selects one named Bundle destination instead of the
	// Manifest Store when Upload is true. Values are absolute directories.
	ToRefLocation config.RefLocations
}

type savedDiskLayer struct {
	fetch.Stream
	// Published pure block layers need no open unless capacity must be read or
	// the complete view must be compacted. A retained layer may have no Stream.
	portable string
	size     uint64
}

// ExportDisk publishes one named data disk at a committed E or paired S/E
// point. It never takes an online checkpoint or consumes an existing one.
func (s *ProcessStorage) ExportDisk(ctx context.Context, req ExportDiskRequest) (out DiskV1, retErr error) {
	if req.SourceRef == "" || req.DiskName == "" || req.Upload == (req.OutputDir != "") {
		return out, errors.New("disk export requires source, disk name and exactly one destination")
	}
	if len(req.ToRefLocation) > 1 || len(req.ToRefLocation) != 0 && !req.Upload {
		return out, errors.New("disk export accepts one target ref location with upload")
	}
	for name, directory := range req.ToRefLocation {
		if name == "" {
			return out, errors.New("disk export target ref location name is required")
		}
		probe := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: "probe.bundle", Location: name}
		if err := probe.Validate(); err != nil {
			return out, err
		}
		if !filepath.IsAbs(directory) {
			return out, fmt.Errorf("publish location directory must be absolute: %q", directory)
		}
		directory = filepath.Clean(directory)
		if previous, exists := req.RefLocations[name]; exists && previous != directory {
			return out, fmt.Errorf("publish target ref location %q conflicts with input ref location: %q != %q", name, directory, previous)
		}
		// The target can also resolve source refs, as in publish. Keep the
		// caller's input mappings unchanged and defer all destination I/O.
		req.RefLocations = maps.Clone(req.RefLocations)
		if req.RefLocations == nil {
			req.RefLocations = make(config.RefLocations)
		}
		req.RefLocations[name] = directory
	}
	if req.SourceDir != "" && !filepath.IsAbs(req.SourceDir) {
		return out, errors.New("source directory must be absolute")
	}
	p := newPublisher(s, req.RefLocations, nil, nil)
	err := p.withDiskTable(ctx, req, func(cfg *config.PortableSandboxConfig, cp *sandboxfile.Checkpoint, scope publishScope) error {
		layers, e := p.openSavedDisk(ctx, cfg, cp, scope, req.DiskName)
		if e != nil {
			return e
		}
		out, e = s.publishSavedDisk(ctx, req, layers)
		return e
	})
	if err != nil {
		return DiskV1{}, err
	}
	return out, nil
}

func (p *Publisher) withDiskTable(ctx context.Context, req ExportDiskRequest, read func(*config.PortableSandboxConfig, *sandboxfile.Checkpoint, publishScope) error) (retErr error) {
	if !strings.Contains(req.SourceRef, "://") && !filepath.IsAbs(req.SourceRef) {
		return errors.New("source path must be absolute; use a file reference with source directory for relative artifacts")
	}
	raw, scope, err := p.normalizeRoot(req.SourceRef)
	if err != nil {
		return err
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return err
	}
	if ref.Scheme == manifest.RefSchemeFile && !filepath.IsAbs(ref.Path) && ref.Location == "" && req.SourceDir == "" {
		return errors.New("relative source file ref requires source directory")
	}
	if req.SourceDir != "" {
		scope.relativeDir = req.SourceDir
	}
	// A borrowed Bundle can select a named parent even when its own carrier
	// has no path. Keep that selection for the shared publication ref policy.
	scope.bindings = make(map[string]string)
	root, scope, err := p.open(ctx, raw, scope)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	cfg, cp, sandboxErr := sandboxfile.ReadDiskTable(ctx, root)
	if sandboxErr != nil {
		if readretry.IsTerminal(sandboxErr) {
			return sandboxErr
		}
		metadata, e := snapshotfile.ReadConfig(ctx, root)
		if e != nil {
			return fmt.Errorf("read disk export source E or S: %w", errors.Join(sandboxErr, e))
		}
		sc, e := snapshot.ParseConfig(metadata)
		if e != nil {
			return e
		}
		canonical, e := snapshot.MarshalConfig(sc)
		if e != nil || !bytes.Equal(canonical, metadata) {
			return errors.New("disk export: noncanonical snapshot configuration")
		}
		paired, child, e := p.open(ctx, sc.SandboxRef, scope)
		if e != nil {
			return e
		}
		defer func() { retErr = errors.Join(retErr, paired.Close()) }()
		scope = child
		cfg, cp, err = sandboxfile.ReadDiskTable(ctx, paired)
		if err != nil {
			return err
		}
	}
	return read(cfg, cp, scope)
}

func closeDiskLayers(layers []savedDiskLayer) error {
	var err error
	for _, l := range layers {
		if l.Stream != nil {
			err = errors.Join(err, l.Close())
		}
	}
	return err
}

func (p *Publisher) openSavedDisk(ctx context.Context, cfg *config.PortableSandboxConfig, cp *sandboxfile.Checkpoint, scope publishScope, name string) (layers []savedDiskLayer, retErr error) {
	index := slices.IndexFunc(cfg.Boot.Disks, func(d config.PortableDiskConfig) bool { return d.Name == name })
	if index < 0 {
		return layers, fmt.Errorf("%w: %s", ErrDiskNotFound, name)
	}
	disk := cfg.Boot.Disks[index]
	if disk.Unbound() {
		return nil, ErrDiskUnbound
	}
	if disk.Overlay != nil {
		return layers, fmt.Errorf("%w: %s", ErrDiskLayout, name)
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, closeDiskLayers(layers))
			layers = nil
		}
	}()
	if cp != nil {
		// Older checkpoints permit a different ordering from boot.disks[].
		j := slices.IndexFunc(cp.Disks, func(cd sandboxfile.CheckpointNamedDisk) bool { return cd.Name == disk.Name })
		if j < 0 {
			return layers, errors.New("checkpoint data disk missing")
		}
		cd := cp.Disks[j]
		if cd.DiffID == "" {
			return layers, ErrDiskUnbound
		}
		if scope.relativeDir == "" {
			return layers, errors.New("named checkpoint requires local DIFF directory")
		}
		view, e := p.storage.openCheckpointVersion(ctx, scope.relativeDir, cp.CheckpointID, cd.DiffID)
		if e != nil {
			return layers, e
		}
		layers = append(layers, savedDiskLayer{Stream: view, size: view.Size()})
		if view.Info().BaseID != DiffCheckpointBaseID(disk.PortableRootConfig) {
			return layers, fmt.Errorf("checkpoint disk %s base binding mismatch", disk.Name)
		}
	}
	if disk.Base != "" {
		refs := append([]string{disk.Base}, disk.BaseFromRefs...)
		compact := len(layers)+len(refs) > config.MaxPortableLayerRefs+1
		for _, raw := range refs {
			portable, keep, e := p.keepSavedDiskLayer(ctx, raw, scope)
			if e != nil {
				return layers, e
			}
			if keep && disk.PureBlock && !compact && (len(layers) != 0 || disk.Binding != nil) {
				layer := savedDiskLayer{portable: portable}
				if len(layers) == 0 {
					layer.size = uint64(disk.Binding.SizeBytes)
				}
				layers = append(layers, layer)
				continue
			}
			st, _, e := p.open(ctx, raw, scope)
			if e != nil {
				return layers, e
			}
			ref, _ := manifest.ParseRef(raw)
			layer := savedDiskLayer{Stream: st}
			if keep {
				layer.portable = portable
			}
			if !disk.PureBlock {
				payload, wrapped, e := sandboxfile.PayloadIfSandbox(ctx, st, ref.Scheme == manifest.RefSchemeFile && filepath.Ext(ref.Path) == ".sandbox")
				if e != nil {
					return layers, e
				}
				layer.Stream = payload
				if wrapped {
					layer.portable = ""
				}
			}
			layer.size = layer.Size()
			layers = append(layers, layer)
		}
	}
	if len(layers) > 0 && disk.Binding != nil && int64(layers[0].size) != disk.Binding.SizeBytes {
		return layers, errors.New("saved disk capacity does not match bound disk")
	}
	if len(layers) == 0 {
		return layers, errors.New("saved disk has no block source")
	}
	return layers, nil
}

// keepSavedDiskLayer uses the same provenance selection as E/S publication.
// A Manifest found in a named Bundle becomes that Bundle's located selector;
// one found only in a private or borrowed Bundle must still be materialized.
func (p *Publisher) keepSavedDiskLayer(ctx context.Context, raw string, scope publishScope) (string, bool, error) {
	portable, keep, err := p.keepPortable(ctx, raw, scope)
	if err != nil {
		return "", false, err
	}
	if !keep {
		bound := scopedRef(raw, scope)
		if bound == raw {
			return "", false, nil
		}
		portable, keep, err = p.keepPortable(ctx, bound, scope)
		if err != nil || !keep {
			return "", false, err
		}
	}
	ref, err := manifest.ParseRef(portable)
	if err != nil {
		return "", false, err
	}
	// DiskV1 is an immutable disk definition. An identityless named file must
	// be read and published before its bytes can be retained independently.
	if ref.Scheme == manifest.RefSchemeFile && ref.DigestScheme == "" {
		return "", false, nil
	}
	return portable, true, nil
}

func (s *ProcessStorage) publishSavedDisk(ctx context.Context, req ExportDiskRequest, layers []savedDiskLayer) (out DiskV1, retErr error) {
	defer func() { retErr = errors.Join(retErr, closeDiskLayers(layers)) }()
	size := int64(layers[0].size)
	if err := diffstore.ValidateWritableSize(size); err != nil {
		return DiskV1{}, err
	}
	for i, l := range layers[1:] {
		if l.Stream != nil && l.size > uint64(size) {
			return DiskV1{}, fmt.Errorf("disk parent %d exceeds top logical size", i)
		}
	}
	if len(layers) > config.MaxPortableLayerRefs+1 {
		// A fresh writable overlay on an already maximum-depth chain can add one
		// layer. Compact at export, preserving holes, explicit zeros and short
		// parents. The joined stream owns the original children.
		all := make([]fetch.Stream, len(layers))
		for i, l := range layers {
			all[i] = l.Stream
		}
		layers = []savedDiskLayer{{Stream: fetch.NewLayered(all...), size: uint64(size)}}
	}
	refs, err := s.deliverDiskLayers(ctx, req, layers)
	if err != nil {
		return out, err
	}
	return DiskV1{Kind: "disk", Version: 1, Size: &size, Base: refs[0], BaseFromRefs: refs[1:]}, nil
}

func (s *ProcessStorage) deliverDiskLayers(ctx context.Context, req ExportDiskRequest, layers []savedDiskLayer) (refs []string, retErr error) {
	refs = make([]string, 0, len(layers))
	if req.Upload && len(req.ToRefLocation) == 0 {
		var pub *Publisher
		defer func() { retErr = errors.Join(retErr, pub.Close()) }()
		for _, l := range layers {
			ref := l.portable
			if ref == "" {
				var err error
				if pub == nil {
					pub, err = NewManifestPublisher(s, s.ManifestConfig(), req.RefLocations, nil)
					if err != nil {
						return nil, err
					}
				}
				ref, err = pub.target.Put(ctx, RoleOverlay, l.Stream)
				if err != nil {
					return nil, err
				}
			}
			refs = append(refs, ref)
		}
		return refs, nil
	}
	// A local or named Bundle contains only newly materialized layers. Ingest
	// each stream once so sequential checkpoint sources remain usable.
	// DiskV1 retains the complete ordered graph, including unchanged parents.
	var materialized []string
	var materializedIndexes []int
	var bundle *snapshot.BundleSink
	defer func() {
		if bundle != nil {
			retErr = errors.Join(retErr, bundle.Close())
		}
	}()
	directory, location := req.OutputDir, ""
	for name, target := range req.ToRefLocation {
		directory, location = target, name
	}
	dir, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	for i, l := range layers {
		if l.portable != "" {
			refs = append(refs, l.portable)
			continue
		}
		if bundle == nil {
			if err = ensurePublishDirectory(dir); err != nil {
				return nil, err
			}
			bundle, err = snapshot.NewBundleSink(ctx, dir, "disk", s.ManifestConfig(), s.CustomerKeyFunc(), nil)
			if err != nil {
				return nil, err
			}
		}
		r, _, e := bundle.AbsorbOverlaySource(ctx, l.Stream)
		if e != nil {
			return nil, e
		}
		refs = append(refs, r)
		materialized = append(materialized, r)
		materializedIndexes = append(materializedIndexes, i)
	}
	if bundle == nil {
		return refs, nil
	}
	path, err := bundle.CommitDisk(ctx, materialized)
	if err != nil {
		return nil, err
	}
	if location != "" {
		path = filepath.Base(path)
	}
	for _, i := range materializedIndexes {
		sel, _ := manifest.ParseRef(refs[i])
		refs[i] = (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path, DigestScheme: "manifest", Digest: sel.Path, Location: location}).String()
	}
	return refs, nil
}

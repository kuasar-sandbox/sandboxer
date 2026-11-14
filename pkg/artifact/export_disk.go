package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
}

type savedDiskLayer struct {
	fetch.Stream
	// portable is set only if the source was opened directly from the remote
	// Store and its immutable pure block interpretation is known.
	portable string
}

// ExportDisk publishes one named data disk at a committed E or paired S/E
// point. It never takes an online checkpoint or consumes an existing one.
func (s *ProcessStorage) ExportDisk(ctx context.Context, req ExportDiskRequest) (out DiskV1, retErr error) {
	if req.SourceRef == "" || req.DiskName == "" || req.Upload == (req.OutputDir != "") {
		return out, errors.New("disk export requires source, disk name and exactly one destination")
	}
	if req.SourceDir != "" && !filepath.IsAbs(req.SourceDir) {
		return out, errors.New("source directory must be absolute")
	}
	if s.ManifestConfig() == nil {
		return out, errors.New("disk export requires a manifest configuration")
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
		err = errors.Join(err, l.Close())
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
		layers = append(layers, savedDiskLayer{Stream: view})
		if view.Info().BaseID != DiffCheckpointBaseID(disk.PortableRootConfig) {
			return layers, fmt.Errorf("checkpoint disk %s base binding mismatch", disk.Name)
		}
	}
	if disk.Base != "" {
		for _, raw := range append([]string{disk.Base}, disk.BaseFromRefs...) {
			st, _, e := p.open(ctx, raw, scope)
			if e != nil {
				return layers, e
			}
			ref, _ := manifest.ParseRef(raw)
			layer := savedDiskLayer{Stream: st}
			if !disk.PureBlock {
				payload, wrapped, e := sandboxfile.PayloadIfSandbox(ctx, st, ref.Scheme == manifest.RefSchemeFile && filepath.Ext(ref.Path) == ".sandbox")
				if e != nil {
					return layers, e
				}
				layer.Stream = payload
				if !wrapped && scope.current == nil && ref.Scheme == manifest.RefSchemeManifest {
					layer.portable = raw
				}
			} else if scope.current == nil && ref.Scheme == manifest.RefSchemeManifest {
				layer.portable = raw
			}
			layers = append(layers, layer)
		}
	}
	if len(layers) > 0 && disk.Binding != nil && int64(layers[0].Size()) != disk.Binding.SizeBytes {
		return layers, errors.New("saved disk capacity does not match bound disk")
	}
	if len(layers) == 0 {
		return layers, errors.New("saved disk has no block source")
	}
	return layers, nil
}

func (s *ProcessStorage) publishSavedDisk(ctx context.Context, req ExportDiskRequest, layers []savedDiskLayer) (out DiskV1, retErr error) {
	defer func() { retErr = errors.Join(retErr, closeDiskLayers(layers)) }()
	size := int64(layers[0].Size())
	if err := diffstore.ValidateWritableSize(size); err != nil {
		return DiskV1{}, err
	}
	for i, l := range layers[1:] {
		if l.Size() > uint64(size) {
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
		layers = []savedDiskLayer{{Stream: fetch.NewLayered(all...)}}
	}
	refs, err := s.deliverDiskLayers(ctx, req, layers)
	if err != nil {
		return out, err
	}
	return DiskV1{Kind: "disk", Version: 1, Size: &size, Base: refs[0], BaseFromRefs: refs[1:]}, nil
}

func (s *ProcessStorage) deliverDiskLayers(ctx context.Context, req ExportDiskRequest, layers []savedDiskLayer) (refs []string, retErr error) {
	refs = make([]string, 0, len(layers))
	if req.Upload {
		pub, err := NewManifestPublisher(s, s.ManifestConfig(), req.RefLocations, nil)
		if err != nil {
			return nil, err
		}
		defer func() { retErr = errors.Join(retErr, pub.Close()) }()
		for _, l := range layers {
			ref := l.portable
			if ref == "" {
				ref, err = pub.target.Put(ctx, RoleOverlay, l.Stream)
				if err != nil {
					return nil, err
				}
			}
			refs = append(refs, ref)
		}
		return refs, nil
	}
	dir, err := filepath.Abs(req.OutputDir)
	if err != nil {
		return nil, err
	}
	if err = ensurePublishDirectory(dir); err != nil {
		return nil, err
	}
	bundle, err := snapshot.NewBundleSink(ctx, dir, "disk", s.ManifestConfig(), s.CustomerKeyFunc(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, bundle.Close()) }()
	for _, l := range layers {
		r, _, e := bundle.AbsorbOverlaySource(ctx, l.Stream)
		if e != nil {
			return nil, e
		}
		refs = append(refs, r)
	}
	path, err := bundle.CommitDisk(ctx, refs)
	if err != nil {
		return nil, err
	}
	for i, r := range refs {
		sel, _ := manifest.ParseRef(r)
		refs[i] = (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path, DigestScheme: "manifest", Digest: sel.Path}).String()
	}
	return refs, nil
}

package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// SnapshotCfg is a process-local compatibility projection for the current
// orchestrator task-preparation API. It is never decoded from or encoded into
// snapshot.cfg: the on-disk V1 schema remains snapshot.Config and contains
// only sandbox_ref plus memory from_refs. Read derives every disk, resource,
// launch and metadata field below from the referenced Sandbox E.
type SnapshotCfg struct {
	Version    int    `yaml:"version"`
	SandboxRef string `yaml:"sandbox_ref"`
	Resources  struct {
		Capacity struct {
			CPU    int    `yaml:"cpu"`
			Memory string `yaml:"memory"`
		} `yaml:"capacity"`
	} `yaml:"resources"`
	Metadata map[string]string `yaml:"metadata,omitempty"`
	FromRefs []string          `yaml:"from_refs"`
	Launch   struct {
		CgroupControl bool `yaml:"cgroup_control"`
	} `yaml:"launch"`
	Boot struct {
		RuntimeRef string `yaml:"runtime_ref"`
		Root       struct {
			BaseRef      string          `yaml:"base_ref,omitempty"`
			Overlay      *SnapOverlayCfg `yaml:"overlay,omitempty"`
			Base         string          `yaml:"base,omitempty"`
			BaseFromRefs []string        `yaml:"base_from_refs,omitempty"`
		} `yaml:"root"`
		Disks []SnapDiskNode `yaml:"disks,omitempty"`
	} `yaml:"boot"`
}

// SnapDiskNode is one derived data-disk graph in Artifact order.
type SnapDiskNode struct {
	BaseRef      string          `yaml:"base_ref,omitempty"`
	Overlay      *SnapOverlayCfg `yaml:"overlay,omitempty"`
	Base         string          `yaml:"base,omitempty"`
	BaseFromRefs []string        `yaml:"base_from_refs,omitempty"`
}

// SnapOverlayCfg is one derived immutable upper/lower chain.
type SnapOverlayCfg struct {
	Base         string   `yaml:"base"`
	BaseFromRefs []string `yaml:"base_from_refs"`
}

// ArtifactRefs returns the disk artifacts named by the referenced Sandbox E.
// It excludes memory FromRefs and the node-provided runtime artifact.
func (c *SnapshotCfg) ArtifactRefs() []string {
	if c == nil {
		return nil
	}
	refs := make([]string, 0)
	appendRef := func(raw string) {
		if raw != "" {
			refs = append(refs, raw)
		}
	}
	appendNode := func(baseRef, base string, baseFromRefs []string, overlay *SnapOverlayCfg) {
		appendRef(baseRef)
		appendRef(base)
		for _, raw := range baseFromRefs {
			appendRef(raw)
		}
		if overlay != nil {
			appendRef(overlay.Base)
			for _, raw := range overlay.BaseFromRefs {
				appendRef(raw)
			}
		}
	}
	appendNode(c.Boot.Root.BaseRef, c.Boot.Root.Base, c.Boot.Root.BaseFromRefs, c.Boot.Root.Overlay)
	for i := range c.Boot.Disks {
		disk := &c.Boot.Disks[i]
		appendNode(disk.BaseRef, disk.Base, disk.BaseFromRefs, disk.Overlay)
	}
	return refs
}

// SnapshotCfgReadOptions supplies trusted local ref resolution inputs.
type SnapshotCfgReadOptions struct {
	RefLocations config.RefLocations
	RelativeDir  string
}

// SnapshotCfgDocument carries the derived compatibility projection and the
// original canonical new-schema snapshot.cfg bytes.
type SnapshotCfgDocument struct {
	Config *SnapshotCfg
	Raw    []byte
}

// SnapshotCfgReader owns process-local artifact storage for one or more
// sequential root reads. Callers must Close it.
type SnapshotCfgReader struct {
	storage     *artifact.ProcessStorage
	manifestCfg *config.ManifestConfig
}

// NewSnapshotCfgReader creates the reader used by current orchestrator task
// preparation. The customer key remains process-bound through ProcessStorage.
func NewSnapshotCfgReader(manifestCfg *config.ManifestConfig) (*SnapshotCfgReader, error) {
	storage, err := artifact.NewProcessStorage(manifestCfg)
	if err != nil {
		return nil, err
	}
	return &SnapshotCfgReader{storage: storage, manifestCfg: manifestCfg}, nil
}

func (r *SnapshotCfgReader) Close() error {
	if r == nil || r.storage == nil {
		return nil
	}
	return r.storage.Close()
}

// Read opens strict Snapshot S, follows its sandbox_ref while S remains open,
// and derives a compatibility view from Sandbox E. It never accepts the old
// disk-bearing snapshot.cfg schema.
func (r *SnapshotCfgReader) Read(ctx context.Context, rootRef string, opts SnapshotCfgReadOptions) (*SnapshotCfgDocument, error) {
	opened, root, memoryCfg, err := r.openSnapshot(ctx, rootRef, opts)
	if err != nil {
		return nil, err
	}
	sandboxSource, err := openReferencedSandbox(ctx, memoryCfg.SandboxRef, opened.opts)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("snapshot.cfg sandbox_ref: %w", err), root.Close())
	}

	projection, projectErr := projectSnapshotCfg(memoryCfg, sandboxSource.Root.Portable)
	document := &SnapshotCfgDocument{Config: projection, Raw: append([]byte(nil), root.SnapshotConfig...)}
	closeErr := errors.Join(sandboxSource.Root.Close(), root.Close())
	if projectErr != nil || closeErr != nil {
		return nil, errors.Join(projectErr, closeErr)
	}
	return document, nil
}

// ReadRaw returns only canonical new-schema snapshot.cfg bytes. It still
// rejects old/unknown/non-canonical Snapshot roots, but does not open E.
func (r *SnapshotCfgReader) ReadRaw(ctx context.Context, rootRef string, opts SnapshotCfgReadOptions) ([]byte, error) {
	_, root, _, err := r.openSnapshot(ctx, rootRef, opts)
	if err != nil {
		return nil, err
	}
	raw := append([]byte(nil), root.SnapshotConfig...)
	if err := root.Close(); err != nil {
		return nil, err
	}
	return raw, nil
}

func (r *SnapshotCfgReader) openSnapshot(ctx context.Context, rootRef string, readOpts SnapshotCfgReadOptions) (*openedRootSnapshot, *snapshotfile.Root, *snapshot.Config, error) {
	if r == nil || r.storage == nil {
		return nil, nil, nil, errors.New("snapshot.cfg reader is not initialized")
	}
	if strings.TrimSpace(rootRef) == "" {
		return nil, nil, nil, errors.New("snapshot root ref is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	openOpts, err := r.openOptions(rootRef, readOpts)
	if err != nil {
		return nil, nil, nil, err
	}
	opened, err := openRootSnapshot(ctx, openOpts)
	if err != nil {
		return nil, nil, nil, err
	}
	root, err := snapshotfile.Open(ctx, opened.stream)
	if err != nil {
		return nil, nil, nil, err
	}
	memoryCfg, err := snapshot.ParseConfig(root.SnapshotConfig)
	if err != nil {
		return nil, nil, nil, errors.Join(fmt.Errorf("unsupported snapshot format/version: %w", err), root.Close())
	}
	canonical, err := snapshot.MarshalConfig(memoryCfg)
	if err != nil {
		return nil, nil, nil, errors.Join(err, root.Close())
	}
	if !bytes.Equal(canonical, root.SnapshotConfig) {
		return nil, nil, nil, errors.Join(errors.New("snapshot.cfg is not canonically encoded"), root.Close())
	}
	return opened, root, memoryCfg, nil
}

func (r *SnapshotCfgReader) openOptions(rootRef string, readOpts SnapshotCfgReadOptions) (Options, error) {
	opts := Options{
		ManifestCfg:   r.manifestCfg,
		Fetcher:       r.storage.Fetcher(),
		CustomerKeyFn: r.storage.CustomerKeyFunc(),
		LocalCodec:    r.storage.LocalCodec(),
		LocalRequired: r.storage.LocalRequired(),
		RefLocations:  readOpts.RefLocations,
	}
	if strings.HasPrefix(rootRef, "manifest://") || strings.HasPrefix(rootRef, "file://") {
		ref, err := manifest.ParseRef(rootRef)
		if err != nil {
			return Options{}, err
		}
		switch ref.Scheme {
		case manifest.RefSchemeManifest:
			if _, err := manifest.ParseKeyRef(ref.Path); err != nil {
				return Options{}, err
			}
			opts.SnapshotManifestKey = ref.Path
			opts.SnapshotRef = ref.String()
		case manifest.RefSchemeFile:
			path, err := readOpts.RefLocations.ResolveFile(ref, readOpts.RelativeDir)
			if err != nil {
				return Options{}, err
			}
			opts.SnapshotPath = path
			opts.SnapshotRef = ref.String()
		default:
			return Options{}, fmt.Errorf("unsupported snapshot root scheme %q", ref.Scheme)
		}
		return opts, nil
	}
	path := rootRef
	if !filepath.IsAbs(path) && readOpts.RelativeDir != "" {
		path = filepath.Join(readOpts.RelativeDir, path)
	}
	opts.SnapshotPath = path
	return opts, nil
}

func projectSnapshotCfg(memoryCfg *snapshot.Config, portable *config.PortableSandboxConfig) (*SnapshotCfg, error) {
	if memoryCfg == nil || portable == nil {
		return nil, errors.New("snapshot compatibility projection requires S and E configs")
	}
	if err := memoryCfg.Validate(); err != nil {
		return nil, err
	}
	if err := portable.Validate(); err != nil {
		return nil, err
	}
	projected := &SnapshotCfg{
		Version:    memoryCfg.Version,
		SandboxRef: memoryCfg.SandboxRef,
		Metadata:   cloneSnapshotMetadata(portable.Metadata),
		FromRefs:   append([]string(nil), memoryCfg.FromRefs...),
	}
	projected.Resources.Capacity.CPU = portable.Resources.Capacity.CPU
	projected.Resources.Capacity.Memory = portable.Resources.Capacity.Memory
	projected.Launch.CgroupControl = portable.Launch.CgroupControl
	projected.Boot.RuntimeRef = portable.Boot.Runtime

	root := projectPortableDisk(portable.Boot.Root, memoryCfg.SandboxRef)
	projected.Boot.Root.BaseRef = root.BaseRef
	projected.Boot.Root.Overlay = root.Overlay
	projected.Boot.Root.Base = root.Base
	projected.Boot.Root.BaseFromRefs = root.BaseFromRefs
	projected.Boot.Disks = make([]SnapDiskNode, len(portable.Boot.Disks))
	for i := range portable.Boot.Disks {
		projected.Boot.Disks[i] = projectPortableDisk(portable.Boot.Disks[i].PortableRootConfig, memoryCfg.SandboxRef)
	}
	return projected, nil
}

func projectPortableDisk(root config.PortableRootConfig, sandboxRef string) SnapDiskNode {
	materialize := func(raw string) string {
		if raw == "self" {
			return sandboxRef
		}
		return raw
	}
	projected := SnapDiskNode{}
	if root.Overlay == nil {
		projected.Base = materialize(root.Base)
		projected.BaseFromRefs = append([]string(nil), root.BaseFromRefs...)
		return projected
	}
	projected.BaseRef = materialize(root.Base)
	projected.Overlay = &SnapOverlayCfg{
		Base:         materialize(root.Overlay.Base),
		BaseFromRefs: append([]string(nil), root.Overlay.BaseFromRefs...),
	}
	return projected
}

func cloneSnapshotMetadata(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

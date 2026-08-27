package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"golang.org/x/sys/unix"
)

type LogicalRole string

const (
	RoleOverlay  LogicalRole = "overlay"
	RoleSandbox  LogicalRole = "sandbox"
	RoleSnapshot LogicalRole = "snapshot"
)

type PublishResult struct {
	Role LogicalRole
	Ref  string
}

// Publisher is the sole E/S graph publisher. Input carrier lookup and target
// writing are separate from logical roles; no artifact registry or metadata
// entry participates in role detection.
type Publisher struct {
	storage   *ProcessStorage
	locations config.RefLocations
	target    publishTarget
	logf      func(string, ...any)

	diskMemo   map[string]string
	memoryMemo map[string]string
	visiting   map[string]struct{}
}

type publishTarget interface {
	Put(context.Context, LogicalRole, sparse.Source) (string, error)
	Close() error
}

type manifestPublishTarget struct {
	ing   ingest.Ingester
	close func() error
	logf  func(string, ...any)
}

func (t *manifestPublishTarget) Put(ctx context.Context, role LogicalRole, source sparse.Source) (string, error) {
	result, err := t.ing.Ingest(ctx, source, ingest.IngestOption{})
	if err != nil {
		return "", fmt.Errorf("publish %s to manifest store: %w", role, err)
	}
	ref := "manifest://" + manifest.HexKey(result.ManifestKey)
	if t.logf != nil {
		t.logf("publish: %s -> %s (stored=%d dedup=%d)", role, ref, result.StoredChunks, result.DedupChunks)
	}
	return ref, nil
}

func (t *manifestPublishTarget) Close() error {
	if t == nil || t.close == nil {
		return nil
	}
	return t.close()
}

type filePublishTarget struct {
	sink     *snapshot.FileSink
	location string
	logf     func(string, ...any)
}

func (t *filePublishTarget) Put(ctx context.Context, role LogicalRole, source sparse.Source) (string, error) {
	var ref string
	var err error
	switch role {
	case RoleOverlay:
		ref, _, err = t.sink.AbsorbOverlaySource(ctx, source)
	case RoleSandbox:
		ref, _, err = t.sink.AbsorbSandbox(ctx, source)
	case RoleSnapshot:
		ref, _, err = t.sink.AbsorbSnapshot(ctx, source)
	default:
		return "", fmt.Errorf("unsupported publish role %q", role)
	}
	if err != nil {
		return "", err
	}
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		return "", err
	}
	parsed.Location = t.location
	if err := parsed.Validate(); err != nil {
		return "", err
	}
	ref = parsed.String()
	if t.logf != nil {
		t.logf("publish: %s -> %s", role, ref)
	}
	return ref, nil
}

func (*filePublishTarget) Close() error { return nil }

// NewManifestPublisher publishes every selected logical object through one
// manifest ingester. Roots are written last by Publish.
func NewManifestPublisher(storage *ProcessStorage, cfg *config.ManifestConfig, locations config.RefLocations, logf func(string, ...any)) (*Publisher, error) {
	if storage == nil || cfg == nil {
		return nil, errors.New("publish: process storage and manifest config are required")
	}
	if storage.CustomerKeyFunc() == nil {
		return nil, errors.New("publish: customer key resolver is required")
	}
	ing, err := cfg.NewIngester(storage.CustomerKeyFunc(), nil)
	if err != nil {
		return nil, err
	}
	return newPublisher(storage, locations, &manifestPublishTarget{ing: ing, close: ing.Close, logf: logf}, logf), nil
}

// NewLocationPublisher writes content-addressed tarstreams into one trusted
// named location using the configured local crypto policy.
func NewLocationPublisher(storage *ProcessStorage, location, directory string, locations config.RefLocations, logf func(string, ...any)) (*Publisher, error) {
	if storage == nil {
		return nil, errors.New("publish: process storage is required")
	}
	probe := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: "probe.sandbox", Location: location}
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(directory) {
		return nil, fmt.Errorf("publish location directory must be absolute: %q", directory)
	}
	if err := ensurePublishDirectory(directory); err != nil {
		return nil, err
	}
	sink := snapshot.NewFileSink(filepath.Clean(directory), "publish", storage.LocalCodec(), storage.LocalRequired(), logf)
	return newPublisher(storage, locations, &filePublishTarget{sink: sink, location: location, logf: logf}, logf), nil
}

func newPublisher(storage *ProcessStorage, locations config.RefLocations, target publishTarget, logf func(string, ...any)) *Publisher {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Publisher{
		storage: storage, locations: locations, target: target, logf: logf,
		diskMemo: make(map[string]string), memoryMemo: make(map[string]string), visiting: make(map[string]struct{}),
	}
}

func (p *Publisher) Close() error {
	if p == nil || p.target == nil {
		return nil
	}
	return p.target.Close()
}

type publishScope struct {
	fetcher     fetch.Fetcher
	relativeDir string
}

// Publish auto-detects exactly one strict logical root and publishes its graph.
func (p *Publisher) Publish(ctx context.Context, input string) (PublishResult, error) {
	if p == nil || p.storage == nil || p.target == nil {
		return PublishResult{}, errors.New("publish: publisher is not initialized")
	}
	rootRef, scope, err := p.normalizeRoot(input)
	if err != nil {
		return PublishResult{}, err
	}
	stream, childScope, err := p.open(ctx, rootRef, scope)
	if err != nil {
		return PublishResult{}, err
	}
	sandboxRoot, sandboxErr := sandboxfile.Open(ctx, stream)
	if sandboxErr == nil {
		ref, err := p.publishSandboxRoot(ctx, rootRef, childScope, sandboxRoot)
		return PublishResult{Role: RoleSandbox, Ref: ref}, err
	}
	stream, childScope, err = p.open(ctx, rootRef, scope)
	if err != nil {
		return PublishResult{}, errors.Join(sandboxErr, err)
	}
	snapshotRoot, snapshotErr := snapshotfile.Open(ctx, stream)
	if snapshotErr != nil {
		return PublishResult{}, fmt.Errorf("publish: logical root is neither a strict .sandbox nor .snapshot: %w", errors.Join(sandboxErr, snapshotErr))
	}
	ref, err := p.publishSnapshotRoot(ctx, rootRef, childScope, snapshotRoot)
	return PublishResult{Role: RoleSnapshot, Ref: ref}, err
}

func (p *Publisher) publishSandboxRoot(ctx context.Context, identity string, scope publishScope, root *sandboxfile.Root) (string, error) {
	defer root.Close()
	if err := p.enter("sandbox:" + identity); err != nil {
		return "", err
	}
	defer p.leave("sandbox:" + identity)
	refs := portableDiskRefs(root.Portable)
	replacements := make(map[string]string, len(refs))
	for i := len(refs) - 1; i >= 0; i-- {
		raw := refs[i].raw
		if _, done := replacements[raw]; done {
			continue
		}
		published, err := p.publishDisk(ctx, raw, scope, refs[i].rootImage)
		if err != nil {
			return "", fmt.Errorf("publish Sandbox dependency %q: %w", raw, err)
		}
		replacements[raw] = published
	}
	rewritten, err := root.Portable.RewriteDiskArtifactRefs(replacements)
	if err != nil {
		return "", err
	}
	runtimeConfig, err := config.MarshalPortableSandboxConfig(rewritten)
	if err != nil {
		return "", err
	}
	source, err := sandboxfile.BuildSourceContext(ctx, root.Payload, root.ImageConfig, runtimeConfig)
	if err != nil {
		return "", err
	}
	return p.target.Put(ctx, RoleSandbox, source)
}

func (p *Publisher) publishSnapshotRoot(ctx context.Context, identity string, scope publishScope, root *snapshotfile.Root) (string, error) {
	defer root.Close()
	if err := p.enter("snapshot:" + identity); err != nil {
		return "", err
	}
	defer p.leave("snapshot:" + identity)
	cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
	if err != nil {
		return "", fmt.Errorf("unsupported snapshot format/version: %w", err)
	}
	canonical, err := snapshot.MarshalConfig(cfg)
	if err != nil || string(canonical) != string(root.SnapshotConfig) {
		if err != nil {
			return "", err
		}
		return "", errors.New("snapshot.cfg is not canonically encoded")
	}
	for i := len(cfg.FromRefs) - 1; i >= 0; i-- {
		published, err := p.publishMemoryLayer(ctx, cfg.FromRefs[i], scope)
		if err != nil {
			return "", fmt.Errorf("publish Snapshot from_refs[%d]: %w", i, err)
		}
		cfg.FromRefs[i] = published
	}
	sandboxStream, sandboxScope, err := p.open(ctx, cfg.SandboxRef, scope)
	if err != nil {
		return "", fmt.Errorf("publish Snapshot sandbox_ref: %w", err)
	}
	sandboxRoot, err := sandboxfile.Open(ctx, sandboxStream)
	if err != nil {
		return "", fmt.Errorf("publish Snapshot sandbox_ref: %w", err)
	}
	cfg.SandboxRef, err = p.publishSandboxRoot(ctx, cfg.SandboxRef, sandboxScope, sandboxRoot)
	if err != nil {
		return "", err
	}
	newConfig, err := snapshot.MarshalConfig(cfg)
	if err != nil {
		return "", err
	}
	source, err := snapshotfile.BuildSource(root.Memory, root.ConfigJSON, root.StateJSON, newConfig)
	if err != nil {
		return "", err
	}
	return p.target.Put(ctx, RoleSnapshot, source)
}

func (p *Publisher) publishDisk(ctx context.Context, raw string, scope publishScope, rootImage bool) (string, error) {
	key := scopeKey(scope, fmt.Sprintf("%t\x00%s", rootImage, raw))
	if ref := p.diskMemo[key]; ref != "" {
		return ref, nil
	}
	stream, _, err := p.open(ctx, raw, scope)
	if err != nil {
		return "", err
	}
	parsed, parseErr := manifest.ParseRef(raw)
	requireSandbox := parseErr == nil && parsed.Scheme == manifest.RefSchemeFile && filepath.Ext(parsed.Path) == ".sandbox"
	payload, _, err := sandboxfile.PayloadIfSandbox(ctx, stream, requireSandbox)
	if err != nil {
		return "", err
	}
	if rootImage {
		image, imageErr := sandboxfile.OpenEROFSArtifact(ctx, payload)
		if imageErr != nil {
			return "", imageErr
		}
		payload = image.Payload
	}
	defer payload.Close()
	ref, err := p.target.Put(ctx, RoleOverlay, payload)
	if err == nil {
		p.diskMemo[key] = ref
	}
	return ref, err
}

func (p *Publisher) publishMemoryLayer(ctx context.Context, raw string, scope publishScope) (string, error) {
	key := scopeKey(scope, raw)
	if ref := p.memoryMemo[key]; ref != "" {
		return ref, nil
	}
	stream, _, err := p.open(ctx, raw, scope)
	if err != nil {
		return "", err
	}
	root, err := snapshotfile.Open(ctx, stream)
	if err != nil {
		return "", err
	}
	defer root.Close()
	cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
	if err != nil {
		return "", fmt.Errorf("unsupported snapshot format/version: %w", err)
	}
	canonical, err := snapshot.MarshalConfig(cfg)
	if err != nil || string(canonical) != string(root.SnapshotConfig) {
		if err != nil {
			return "", err
		}
		return "", errors.New("memory Snapshot snapshot.cfg is not canonical")
	}
	ref, err := p.target.Put(ctx, RoleSnapshot, root.FullStream)
	if err == nil {
		p.memoryMemo[key] = ref
	}
	return ref, err
}

func (p *Publisher) open(ctx context.Context, raw string, scope publishScope) (fetch.Stream, publishScope, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return nil, scope, err
	}
	switch ref.Scheme {
	case manifest.RefSchemeManifest:
		fetcher := scope.fetcher
		if fetcher == nil {
			fetcher = p.storage.Fetcher()
		}
		if fetcher == nil {
			return nil, scope, errors.New("manifest artifact requires manifest configuration")
		}
		key, err := manifest.ParseKeyRef(ref.Path)
		if err != nil {
			return nil, scope, err
		}
		stream, err := fetcher.OpenManifest(ctx, key)
		return stream, scope, err
	case manifest.RefSchemeFile:
		path, err := p.locations.ResolveFile(ref, scope.relativeDir)
		if err != nil {
			return nil, scope, err
		}
		if !filepath.IsAbs(path) {
			path, err = filepath.Abs(path)
			if err != nil {
				return nil, scope, err
			}
		}
		opened, err := p.storage.OpenFileWithLocations(ctx, path, ref, p.locations)
		if err != nil {
			return nil, scope, err
		}
		child := publishScope{fetcher: scope.fetcher, relativeDir: filepath.Dir(path)}
		if _, bundle := opened.RootManifestKey(); bundle {
			child.fetcher = opened.ScopedFetcher()
		}
		return opened, child, nil
	default:
		return nil, scope, fmt.Errorf("unsupported artifact ref scheme %q", ref.Scheme)
	}
}

func (p *Publisher) normalizeRoot(input string) (string, publishScope, error) {
	if strings.TrimSpace(input) == "" {
		return "", publishScope{}, errors.New("publish: input is required")
	}
	if strings.HasPrefix(input, "file://") || strings.HasPrefix(input, "manifest://") {
		ref, err := manifest.ParseRef(input)
		return ref.String(), publishScope{fetcher: p.storage.Fetcher()}, err
	}
	path, err := filepath.Abs(input)
	if err != nil {
		return "", publishScope{}, err
	}
	return (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path}).String(), publishScope{fetcher: p.storage.Fetcher(), relativeDir: filepath.Dir(path)}, nil
}

type portableDiskDependency struct {
	raw       string
	rootImage bool
}

func portableDiskRefs(cfg *config.PortableSandboxConfig) []portableDiskDependency {
	var refs []portableDiskDependency
	appendOne := func(raw string, rootImage bool) {
		if raw != "" && raw != "self" {
			refs = append(refs, portableDiskDependency{raw: raw, rootImage: rootImage})
		}
	}
	appendRoot := func(root *config.PortableRootConfig) {
		appendOne(root.Base, root.Overlay != nil)
		for _, raw := range root.BaseFromRefs {
			appendOne(raw, false)
		}
		if root.Overlay != nil {
			appendOne(root.Overlay.Base, false)
			for _, raw := range root.Overlay.BaseFromRefs {
				appendOne(raw, false)
			}
		}
	}
	appendRoot(&cfg.Boot.Root)
	for i := range cfg.Boot.Disks {
		appendRoot(&cfg.Boot.Disks[i].PortableRootConfig)
	}
	return refs
}

func (p *Publisher) enter(key string) error {
	if _, exists := p.visiting[key]; exists {
		return fmt.Errorf("publish graph cycle at %s", key)
	}
	p.visiting[key] = struct{}{}
	return nil
}

func (p *Publisher) leave(key string) { delete(p.visiting, key) }

func scopeKey(scope publishScope, raw string) string { return scope.relativeDir + "\x00" + raw }

func ensurePublishDirectory(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create publish location: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("publish location must be a real directory, not a symlink or non-directory")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), path)
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

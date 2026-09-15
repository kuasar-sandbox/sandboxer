package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	storeclient "github.com/kuasar-sandbox/accelerator/pkg/store/client"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

type LogicalRole string

const (
	RoleImage    LogicalRole = "image"
	RoleOverlay  LogicalRole = "overlay"
	RoleSandbox  LogicalRole = "sandbox"
	RoleSnapshot LogicalRole = "snapshot"
)

type PublishResult struct {
	Role LogicalRole
	Ref  string
}

// Publisher publishes logical image/E/S roots and their selected graphs. Input
// carrier lookup and target writing are separate from logical roles; no
// artifact registry or metadata entry participates in role detection.
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

type bundlePublishPlan struct {
	path              string
	role              LogicalRole
	root              store.ContentKey
	opened            *OpenedFile
	exactRoot         manifestbundle.ExactManifest
	exactDependencies []manifestbundle.ExactManifest
	// selectedSources records the exact Bundle ref chosen for each selected
	// dependency. An empty ref means the current root Bundle.
	selectedSources map[store.ContentKey]string
	// locatedExact are verified Bundle dependencies whose named refs remain
	// unchanged instead of being copied into the destination location.
	locatedExact       map[store.ContentKey]struct{}
	remoteDependencies int
	keyFn              ingest.CustomerKeyFunc
	decryptor          manifestcrypto.Decryptor
}

type bundlePublishTarget interface {
	PutBundle(context.Context, bundlePublishPlan) (string, error)
}

type manifestPublishTarget struct {
	ing    ingest.Ingester
	client *storeclient.Client
	logf   func(string, ...any)
}

type publishStoreObject struct {
	generation store.Generation
	partition  store.Partition
	key        store.ContentKey
}

type publishPutFlight struct {
	done  chan struct{}
	isNew bool
	err   error
}

// deduplicatingStoreWriter preserves the configured Store pool for distinct
// objects while serializing concurrent writes of one physical object. The
// filesystem Store accepts content-addressed dedup, but two same-key streams
// can both pass Exists before either commit becomes visible. Coalescing at the
// publisher boundary prevents that race without reducing graph upload
// parallelism or changing the accelerator Store contract.
type deduplicatingStoreWriter struct {
	inner ingest.StoreWriter

	mu      sync.Mutex
	flights map[publishStoreObject]*publishPutFlight
	onWait  func() // deterministic test barrier; nil in production
}

func newDeduplicatingStoreWriter(inner ingest.StoreWriter) *deduplicatingStoreWriter {
	return &deduplicatingStoreWriter{
		inner: inner, flights: make(map[publishStoreObject]*publishPutFlight),
	}
}

func (w *deduplicatingStoreWriter) AdmitWrite(ctx context.Context) (store.WriteAdmission, error) {
	return w.inner.AdmitWrite(ctx)
}

func (w *deduplicatingStoreWriter) PoolSize() int {
	if sized, ok := w.inner.(interface{ PoolSize() int }); ok {
		return sized.PoolSize()
	}
	return 1
}

func (w *deduplicatingStoreWriter) Put(ctx context.Context, admission store.WriteAdmission, partition store.Partition, key store.ContentKey, data []byte) (bool, error) {
	object := publishStoreObject{generation: admission.Generation, partition: partition, key: key}
	w.mu.Lock()
	if flight := w.flights[object]; flight != nil {
		onWait := w.onWait
		w.mu.Unlock()
		if onWait != nil {
			onWait()
		}
		select {
		case <-flight.done:
			// Only the leader can have created the physical object. Followers
			// are dedup hits even when the leader reports isNew=true.
			return false, flight.err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	flight := &publishPutFlight{done: make(chan struct{})}
	w.flights[object] = flight
	w.mu.Unlock()

	flight.isNew, flight.err = w.inner.Put(ctx, admission, partition, key, data)
	w.mu.Lock()
	delete(w.flights, object)
	close(flight.done)
	w.mu.Unlock()
	return flight.isNew, flight.err
}

type manifestPublishStoreWriter struct {
	client     *storeclient.Client
	generation store.Generation
}

func (w *manifestPublishStoreWriter) AdmitWrite(ctx context.Context) (store.WriteAdmission, error) {
	if w.generation == "" {
		return w.client.AdmitWrite(ctx)
	}
	return w.client.AdmitWriteFor(ctx, w.generation)
}

func (w *manifestPublishStoreWriter) Put(ctx context.Context, admission store.WriteAdmission, partition store.Partition, key store.ContentKey, data []byte) (bool, error) {
	return w.client.Put(ctx, admission, partition, key, data)
}

func (w *manifestPublishStoreWriter) PoolSize() int { return w.client.PoolSize() }

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
	if t == nil || t.client == nil {
		return nil
	}
	return t.client.Close()
}

func (t *manifestPublishTarget) PutBundle(ctx context.Context, plan bundlePublishPlan) (string, error) {
	if plan.keyFn == nil || plan.decryptor == nil || plan.exactRoot.Reader == nil {
		return "", errors.New("publish Bundle to manifest store: exact source verification is unavailable")
	}
	customerKey, err := plan.keyFn()
	if err != nil {
		return "", err
	}
	defer clear(customerKey[:])
	if err := manifestbundle.UploadExactManifests(
		ctx,
		plan.exactRoot,
		plan.exactDependencies,
		customerKey,
		plan.decryptor,
		t.client,
		manifestbundle.VerifyOptions{},
	); err != nil {
		return "", fmt.Errorf("publish Bundle exact upload: %w", err)
	}
	ref := "manifest://" + manifest.HexKey(plan.root)
	if t.logf != nil {
		t.logf("publish: exact Bundle %s -> %s (bundled_dependencies=%d remote_dependencies=%d)",
			plan.role, ref, len(plan.exactDependencies), plan.remoteDependencies)
	}
	return ref, nil
}

// NewManifestPublisher publishes every selected logical object through one
// manifest ingester. Root Manifest objects are written after their Chunks by
// both Publish and PublishSource.
func NewManifestPublisher(storage *ProcessStorage, cfg *config.ManifestConfig, locations config.RefLocations, logf func(string, ...any)) (*Publisher, error) {
	if storage == nil || cfg == nil {
		return nil, errors.New("publish: process storage and manifest config are required")
	}
	if storage.CustomerKeyFunc() == nil {
		return nil, errors.New("publish: customer key resolver is required")
	}
	if cfg.Store.Endpoint == "" {
		return nil, errors.New("manifest: store.endpoint required for ingest")
	}
	timeout, err := optionalDuration(cfg.Store.Timeout, "store.timeout")
	if err != nil {
		return nil, err
	}
	client, err := storeclient.New(cfg.Store.Endpoint, cfg.Store.Pool, timeout)
	if err != nil {
		return nil, fmt.Errorf("manifest: dial store: %w", err)
	}
	generation := store.Generation(cfg.Manifest.WriteGeneration)
	if generation != "" {
		if err := store.ValidateGeneration(generation); err != nil {
			return nil, errors.Join(fmt.Errorf("manifest: write_generation: %w", err), client.Close())
		}
	}
	writer := newDeduplicatingStoreWriter(&manifestPublishStoreWriter{client: client, generation: generation})
	ing, err := cfg.NewIngesterWithWriter(storage.CustomerKeyFunc(), nil, writer)
	if err != nil {
		return nil, errors.Join(err, client.Close())
	}
	return newPublisher(storage, locations, &manifestPublishTarget{
		ing: ing, client: client, logf: logf,
	}, logf), nil
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
	target := newLocationPublishTarget(
		location,
		filepath.Clean(directory),
		storage.LocalCodec(),
		storage.LocalRequired(),
		logf,
	)
	return newPublisher(storage, locations, target, logf), nil
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

// PublishSource publishes one already-assembled logical image or Sandbox
// source directly to this Publisher's target. The caller retains ownership of
// source and must keep it valid until PublishSource returns. Dependencies named
// by a Sandbox source must already be portable; this method deliberately does
// not infer a graph from filenames or extensions.
func (p *Publisher) PublishSource(ctx context.Context, role LogicalRole, source sparse.Source) (PublishResult, error) {
	if p == nil || p.target == nil {
		return PublishResult{}, errors.New("publish source: publisher is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	if source == nil {
		return PublishResult{}, errors.New("publish source: logical source is required")
	}
	switch role {
	case RoleImage, RoleSandbox:
	default:
		return PublishResult{}, fmt.Errorf("publish source: unsupported root role %q", role)
	}
	ref, err := p.target.Put(ctx, role, source)
	if err != nil {
		return PublishResult{}, err
	}
	return PublishResult{Role: role, Ref: ref}, nil
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
	parsedRoot, err := manifest.ParseRef(rootRef)
	if err != nil {
		return PublishResult{}, err
	}
	if parsedRoot.Scheme == manifest.RefSchemeManifest {
		return PublishResult{}, errors.New("publish: manifest root is already portable and cannot be materialized")
	}
	stream, childScope, err := p.open(ctx, rootRef, scope)
	if err != nil {
		return PublishResult{}, err
	}
	if opened, ok := stream.(*OpenedFile); ok && opened.Format() == FileFormatManifestBundle {
		return p.publishBundle(ctx, rootRef, scope, opened)
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
	refs := portableDiskRefs(root.Portable)
	roles := make(map[string]bool, len(refs))
	for _, dependency := range refs {
		if rootImage, seen := roles[dependency.raw]; seen && rootImage != dependency.rootImage {
			return "", fmt.Errorf("publish Sandbox dependency %q is used as both root image and disk layer", dependency.raw)
		}
		roles[dependency.raw] = dependency.rootImage
	}
	if err := p.enter("sandbox:" + identity); err != nil {
		return "", err
	}
	defer p.leave("sandbox:" + identity)
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
	if portable, ok, err := portablePublishRef(cfg.SandboxRef); err != nil {
		return "", fmt.Errorf("publish Snapshot sandbox_ref: %w", err)
	} else if ok {
		cfg.SandboxRef = portable
	} else {
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
	if portable, ok, err := portablePublishRef(raw); err != nil {
		return "", err
	} else if ok {
		return portable, nil
	}
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
	role := RoleOverlay
	if rootImage {
		image, imageErr := sandboxfile.OpenEROFSArtifact(ctx, payload)
		if imageErr != nil {
			return "", imageErr
		}
		// Root images remain flattened EROFS artifacts. Block consumers narrow
		// them to Payload; publication retains config.json for image defaults.
		payload = image.FullStream
		role = RoleImage
	}
	defer payload.Close()
	ref, err := p.target.Put(ctx, role, payload)
	if err == nil {
		p.diskMemo[key] = ref
	}
	return ref, err
}

func (p *Publisher) publishMemoryLayer(ctx context.Context, raw string, scope publishScope) (string, error) {
	if portable, ok, err := portablePublishRef(raw); err != nil {
		return "", err
	} else if ok {
		return portable, nil
	}
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
		stream, err := readretry.Open(ctx, func() (fetch.Stream, error) { return fetcher.OpenManifest(ctx, key) })
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

func portablePublishRef(raw string) (string, bool, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", false, err
	}
	if !ref.Portable() {
		return "", false, nil
	}
	return ref.String(), true, nil
}

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
	return nil
}

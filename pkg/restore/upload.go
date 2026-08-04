package restore

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/util"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// UploadLocal publishes a local snapshot graph to manifest storage. Local file
// refs are upgraded to manifest refs; existing manifest and located file refs
// remain unchanged.
func UploadLocal(ctx context.Context, snapshotPath string, mcfg *manifest.Config, keyFn ingest.CustomerKeyFunc, manifestFetcher fetch.Fetcher, codec tarstream.Codec, required bool, logf func(string, ...any)) (string, error) {
	if mcfg == nil {
		return "", fmt.Errorf("manifest config is required")
	}
	if keyFn == nil {
		return "", fmt.Errorf("customer key resolver is required")
	}
	ing, err := mcfg.NewIngester(keyFn, nil)
	if err != nil {
		return "", fmt.Errorf("ingester: %w", err)
	}
	defer ing.Close()
	p := newSnapshotPublisher(ctx, codec, required, logf)
	p.ing = ing
	p.fetcher = manifestFetcher
	return p.publishRootSnapshot(snapshotPath, manifest.Ref{})
}

// PublishLocalToLocation publishes a local snapshot graph into one trusted
// named file location. It writes only content-addressed files and returns the
// canonical located root ref.
func PublishLocalToLocation(ctx context.Context, snapshotPath, location, directory string, codec tarstream.Codec, required bool, logf func(string, ...any)) (string, error) {
	probe := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: "probe.snapshot", Location: location}
	if err := probe.Validate(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(directory) {
		return "", fmt.Errorf("ref location directory must be absolute: %q", directory)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("create ref location: %w", err)
	}
	p := newSnapshotPublisher(ctx, codec, required, logf)
	p.location = location
	p.directory = filepath.Clean(directory)
	return p.publishRootSnapshot(snapshotPath, manifest.Ref{})
}

// publishRole is derived from the snapshot.cfg field being traversed. A root
// graph is parsed and rewritten, while memory layers and ordinary artifacts are
// published as opaque logical tarstreams.
type publishRole uint8

const (
	publishRootGraph publishRole = iota
	publishMemoryLayer
	publishLeafArtifact
)

type publishCacheKey struct {
	RealPath string
	Role     publishRole
}

type snapshotPublisher struct {
	ctx       context.Context
	ing       ingest.Ingester
	fetcher   fetch.Fetcher
	codec     tarstream.Codec
	required  bool
	location  string
	directory string
	logf      func(string, ...any)
	done      map[publishCacheKey]string
	visiting  map[publishCacheKey]bool
}

func newSnapshotPublisher(ctx context.Context, codec tarstream.Codec, required bool, logf func(string, ...any)) *snapshotPublisher {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &snapshotPublisher{
		ctx: ctx, codec: codec, required: required, logf: logf,
		done: make(map[publishCacheKey]string), visiting: make(map[publishCacheKey]bool),
	}
}

func resolvePublishPath(path string, codec tarstream.Codec, operation string) (string, error) {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", protectArtifactReadError(codec, operation, err)
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil {
		return "", err
	}
	return realPath, nil
}

func (p *snapshotPublisher) publishRootSnapshot(snapshotPath string, expected manifest.Ref) (result string, retErr error) {
	realPath, err := resolvePublishPath(snapshotPath, p.codec, "resolve local snapshot")
	if err != nil {
		return "", err
	}
	key := publishCacheKey{RealPath: realPath, Role: publishRootGraph}
	if p.visiting[key] {
		return "", fmt.Errorf("upload-snapshot: snapshot cycle detected")
	}
	p.visiting[key] = true
	defer func() { delete(p.visiting, key) }()

	bundle, bundleScheme, bundleDigest, err := openTarArtifact(realPath, expected, p.codec, p.required)
	if err != nil {
		return "", fmt.Errorf("open snapshot: %w", err)
	}
	if ref, ok := p.done[key]; ok {
		_ = bundle.Close()
		return ref, nil
	}
	bundleSize := int64(bundle.Size())
	entries, parsed, err := readSnapshotEntries(p.ctx, bundle, bundleSize)
	if err != nil {
		_ = bundle.Close()
		return "", err
	}
	if err := bundle.Close(); err != nil {
		return "", err
	}
	identity := manifest.Ref{
		Scheme: manifest.RefSchemeFile, Path: realPath,
		DigestScheme: bundleScheme, Digest: bundleDigest,
	}
	if err := validateSequentialInput(p.ctx, realPath, "snapshot", identity, bundleSize, p.codec, p.required); err != nil {
		return "", fmt.Errorf("fully validate snapshot: %w", err)
	}
	bundleDir := filepath.Dir(realPath)

	for i, ref := range parsed.FromRefs {
		parsed.FromRefs[i], err = p.publishRef(fmt.Sprintf("memory parent %d", i), ref, bundleDir, publishMemoryLayer)
		if err != nil {
			return "", err
		}
	}
	if err := p.publishDiskNode("root", &parsed.Boot.Root.BaseRef, &parsed.Boot.Root.Base, &parsed.Boot.Root.BaseFromRefs, parsed.Boot.Root.Overlay, bundleDir); err != nil {
		return "", err
	}
	for i := range parsed.Boot.Disks {
		n := &parsed.Boot.Disks[i]
		if err := p.publishDiskNode(fmt.Sprintf("disk %d", i), &n.BaseRef, &n.Base, &n.BaseFromRefs, n.Overlay, bundleDir); err != nil {
			return "", err
		}
	}

	newCfg, err := yaml.Marshal(parsed)
	if err != nil {
		return "", fmt.Errorf("render snapshot.cfg: %w", err)
	}
	zipBytes, err := snapshot.BuildZIP(map[string][]byte{
		"config.json":  entries["config.json"],
		"state.json":   entries["state.json"],
		"snapshot.cfg": newCfg,
	})
	if err != nil {
		return "", fmt.Errorf("build zip: %w", err)
	}
	memSize, err := util.ParseSize(parsed.Resources.Capacity.Memory)
	if err != nil {
		return "", fmt.Errorf("capacity.memory: %w", err)
	}
	if int64(memSize) > bundleSize {
		return "", fmt.Errorf("snapshot is too small for its memory section")
	}
	sequential, closer, _, sequentialScheme, sequentialDigest, err := openSequentialTarArtifact(realPath, "snapshot", identity, p.codec, p.required)
	if err != nil {
		return "", fmt.Errorf("fully open snapshot: %w", err)
	}
	defer closer.Close()
	if int64(sequential.Size()) != bundleSize {
		return "", fmt.Errorf("snapshot logical size changed during publication")
	}
	if err := matchDigest(sequentialScheme, sequentialDigest, bundleScheme, bundleDigest); err != nil {
		return "", fmt.Errorf("snapshot identity changed during publication")
	}
	src := &memZipSource{bundle: sequential, originalSize: sequential.Size(), memSize: memSize, zip: zipBytes}
	if p.ing != nil {
		res, err := p.ing.Ingest(p.ctx, src, ingest.IngestOption{OnProgress: p.progress("memory section")})
		if err != nil {
			return "", fmt.Errorf("ingest bundle: %w", err)
		}
		result = "manifest://" + manifest.HexKey(res.ManifestKey)
		p.logf("upload-snapshot: memory stored=%d dedup=%d zero=%d", res.StoredChunks, res.DedupChunks, res.ZeroChunks)
	} else {
		tmp, err := os.CreateTemp(p.directory, ".publish-snapshot-*.tmp")
		if err != nil {
			return "", err
		}
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)
		scheme, digest, writeErr := tarstream.WriteTo(p.ctx, tmp, "snapshot", src, p.writeOptions()...)
		if writeErr == nil {
			writeErr = tmp.Sync()
		}
		closeErr := tmp.Close()
		if writeErr != nil {
			return "", fmt.Errorf("rebuild snapshot: %w", writeErr)
		}
		if closeErr != nil {
			return "", closeErr
		}
		result, err = p.publishLocationFile(tmpPath, ".snapshot", scheme, digest, src.Size())
		if err != nil {
			return "", err
		}
	}
	p.done[key] = result
	return result, nil
}

func validateSequentialInput(ctx context.Context, path, name string, identity manifest.Ref, logicalSize int64, codec tarstream.Codec, required bool) error {
	source, closer, _, _, _, err := openSequentialTarArtifact(path, name, identity, codec, required)
	if err != nil {
		return err
	}
	if int64(source.Size()) != logicalSize {
		_ = closer.Close()
		return fmt.Errorf("logical size changed during validation")
	}
	consumeErr := consumeSource(ctx, source, 0)
	closeErr := closer.Close()
	if consumeErr != nil {
		return consumeErr
	}
	return closeErr
}

func readSnapshotEntries(ctx context.Context, bundle fetch.Stream, bundleSize int64) (map[string][]byte, *SnapshotCfg, error) {
	zr, err := zip.NewReader(fetch.NewReaderAt(ctx, bundle), bundleSize)
	if err != nil {
		return nil, nil, fmt.Errorf("read snapshot ZIP trailer: %w", err)
	}
	entries := make(map[string][]byte)
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("zip open %s: %w", zf.Name, err)
		}
		body, readErr := io.ReadAll(rc)
		closeErr := rc.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("zip read %s: %w", zf.Name, readErr)
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		entries[zf.Name] = body
	}
	for _, name := range []string{"config.json", "state.json", "snapshot.cfg"} {
		if _, ok := entries[name]; !ok {
			return nil, nil, fmt.Errorf("snapshot bundle missing %s", name)
		}
	}
	parsed, err := ParseSnapshotCfg(entries["snapshot.cfg"])
	if err != nil {
		return nil, nil, err
	}
	return entries, parsed, nil
}

func (p *snapshotPublisher) publishDiskNode(label string, baseRef, base *string, baseFromRefs *[]string, overlay *SnapOverlayCfg, bundleDir string) error {
	var err error
	if *baseRef, err = p.publishRef(label+" base", *baseRef, bundleDir, publishLeafArtifact); err != nil {
		return err
	}
	if *base, err = p.publishRef(label+" top", *base, bundleDir, publishLeafArtifact); err != nil {
		return err
	}
	for i, ref := range *baseFromRefs {
		(*baseFromRefs)[i], err = p.publishRef(fmt.Sprintf("%s parent %d", label, i), ref, bundleDir, publishLeafArtifact)
		if err != nil {
			return err
		}
	}
	if overlay == nil {
		return nil
	}
	if overlay.Base, err = p.publishRef(label+" overlay", overlay.Base, bundleDir, publishLeafArtifact); err != nil {
		return err
	}
	for i, ref := range overlay.BaseFromRefs {
		overlay.BaseFromRefs[i], err = p.publishRef(fmt.Sprintf("%s overlay parent %d", label, i), ref, bundleDir, publishLeafArtifact)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *snapshotPublisher) publishRef(label, raw, relativeDir string, role publishRole) (string, error) {
	if raw == "" {
		return "", nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", protectArtifactReadError(p.codec, "upload-snapshot: parse "+label, err)
	}
	if ref.Scheme == manifest.RefSchemeManifest && p.fetcher != nil {
		key, err := manifest.ParseHexKey(ref.Path)
		if err != nil {
			return "", fmt.Errorf("upload-snapshot: %s: %w", label, err)
		}
		stream, err := p.fetcher.OpenManifest(p.ctx, key)
		if err != nil {
			return "", fmt.Errorf("upload-snapshot: %s %q: %w", label, ref.String(), err)
		}
		if err := stream.Close(); err != nil {
			return "", err
		}
	}
	if ref.Portable() {
		return ref.String(), nil
	}
	path := ref.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(relativeDir, path)
	}
	switch role {
	case publishRootGraph:
		return p.publishRootSnapshot(path, ref)
	case publishMemoryLayer:
		return p.publishMemorySnapshotLayer(label, path, ref)
	case publishLeafArtifact:
		return p.publishLeaf(label, path, ref, publishLeafArtifact)
	default:
		return "", fmt.Errorf("upload-snapshot: %s: unknown publish role %d", label, role)
	}
}

// publishMemorySnapshotLayer deliberately does not read snapshot.cfg. A
// from_refs entry is already a flattened memory-only lower in the current root
// graph, so its historical disk graph is not a dependency of this publication.
func (p *snapshotPublisher) publishMemorySnapshotLayer(label, path string, expected manifest.Ref) (string, error) {
	return p.publishLeaf(label, path, expected, publishMemoryLayer)
}

func (p *snapshotPublisher) publishLeaf(label, path string, expected manifest.Ref, role publishRole) (string, error) {
	if role != publishMemoryLayer && role != publishLeafArtifact {
		return "", fmt.Errorf("upload-snapshot: %s: role %d is not an opaque artifact role", label, role)
	}
	realPath, err := resolvePublishPath(path, p.codec, "resolve local "+label)
	if err != nil {
		return "", err
	}
	stream, closer, payloadName, scheme, digest, err := openSequentialTarArtifact(realPath, "", expected, p.codec, p.required)
	if err != nil {
		return "", fmt.Errorf("upload-snapshot: %s: %w", label, err)
	}
	key := publishCacheKey{RealPath: realPath, Role: role}
	if ref, ok := p.done[key]; ok {
		_ = closer.Close()
		return ref, nil
	}
	defer closer.Close()

	if p.ing != nil {
		res, err := p.ing.Ingest(p.ctx, stream, ingest.IngestOption{OnProgress: p.progress(label)})
		if err != nil {
			return "", fmt.Errorf("upload-snapshot: ingest %s: %w", label, err)
		}
		manifestKey := manifest.HexKey(res.ManifestKey)
		p.logf("upload-snapshot: %s → manifest://%s (stored=%d dedup=%d)", label, manifestKey, res.StoredChunks, res.DedupChunks)
		result := "manifest://" + manifestKey
		p.done[key] = result
		return result, nil
	}
	tmp, err := os.CreateTemp(p.directory, ".publish-leaf-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	outScheme, outDigest, writeErr := tarstream.WriteTo(p.ctx, tmp, payloadName, stream, p.writeOptions()...)
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if writeErr != nil {
		return "", fmt.Errorf("upload-snapshot: encode %s: %w", label, writeErr)
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := matchDigest(outScheme, outDigest, scheme, digest); err != nil {
		return "", fmt.Errorf("upload-snapshot: converted identity changed")
	}
	ext := filepath.Ext(realPath)
	result, err := p.publishLocationFile(tmpPath, ext, outScheme, outDigest, stream.Size())
	if err != nil {
		return "", err
	}
	p.done[key] = result
	return result, nil
}

func (p *snapshotPublisher) publishLocationFile(sourcePath, ext, scheme, digest string, logicalSize uint64) (string, error) {
	basename := digest + ext
	destination := filepath.Join(p.directory, basename)
	outputRequired := p.codec != nil
	if err := os.Chmod(sourcePath, 0o644); err != nil {
		return "", fmt.Errorf("publish location: set temporary permissions: %w", err)
	}
	if err := validatePublishedFinal(p.ctx, sourcePath, logicalSize, p.codec, outputRequired, scheme, digest); err != nil {
		return "", fmt.Errorf("publish location: validate converted output: %w", err)
	}
	err := unix.Renameat2(unix.AT_FDCWD, sourcePath, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
	if err == unix.EEXIST {
		if validateErr := validatePublishedFinal(p.ctx, destination, logicalSize, p.codec, outputRequired, scheme, digest); validateErr != nil {
			return "", fmt.Errorf("publish location: existing final is invalid: %w", validateErr)
		}
		return p.locatedRef(basename, scheme, digest)
	}
	if err != nil {
		return "", fmt.Errorf("publish location: commit without replacement: %w", err)
	}
	dir, err := os.Open(p.directory)
	if err != nil {
		return "", fmt.Errorf("publish location: open parent directory: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return "", fmt.Errorf("publish location: sync parent directory: %w", syncErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("publish location: close parent directory: %w", closeErr)
	}
	p.logf("upload-snapshot: published %s", destination)
	return p.locatedRef(basename, scheme, digest)
}

func validatePublishedFinal(ctx context.Context, path string, logicalSize uint64, codec tarstream.Codec, required bool, scheme, digest string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return protectArtifactReadError(codec, "open existing final", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("existing final is not a regular file")
	}
	var options []tarstream.ReadOption
	if codec != nil {
		options = append(options, tarstream.WithCodec(codec, required))
	}
	options = append(options, tarstream.WithExpectedDigest(scheme, digest))
	source, _, err := tarstream.SourceFrom(f, "", options...)
	if err != nil {
		return err
	}
	if source.Size() != logicalSize {
		return fmt.Errorf("existing final logical size mismatch")
	}
	return consumeSource(ctx, source, 0)
}

func (p *snapshotPublisher) locatedRef(basename, scheme, digest string) (string, error) {
	ref := manifest.Ref{
		Scheme: manifest.RefSchemeFile, Path: basename, Location: p.location,
		DigestScheme: scheme, Digest: digest,
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	return ref.String(), nil
}

func (p *snapshotPublisher) writeOptions() []tarstream.WriteOption {
	if p.codec == nil {
		return nil
	}
	return []tarstream.WriteOption{tarstream.WithCodec(p.codec, p.required)}
}

func (p *snapshotPublisher) progress(label string) func(processed, total uint64) {
	const mib = 1 << 20
	last := time.Now()
	return func(processed, total uint64) {
		if time.Since(last) < 2*time.Second {
			return
		}
		last = time.Now()
		p.logf("upload: %s %d/%d MiB", label, processed/mib, total/mib)
	}
}

// memZipSource composes [0,memSize) of the bundle entry followed by the
// re-rendered ZIP trailer as one sparse.Source.
type memZipSource struct {
	bundle       sparse.Source
	originalSize uint64
	memSize      uint64
	zip          []byte
	validated    bool
	validateErr  error
}

func (s *memZipSource) Size() uint64 { return s.memSize + uint64(len(s.zip)) }

func (s *memZipSource) RunAt(off, limit uint64) (sparse.RunKind, uint64, error) {
	total := s.Size()
	if off >= total {
		return 0, 0, io.EOF
	}
	if off < s.memSize {
		if limit > s.memSize-off {
			limit = s.memSize - off
		}
		return s.bundle.RunAt(off, limit)
	}
	end := off + limit
	if end > total {
		end = total
	}
	return sparse.Data, end, nil
}

func (s *memZipSource) ReadAt(ctx context.Context, buf []byte, off uint64) (int, error) {
	total := s.Size()
	if off >= total {
		return 0, io.EOF
	}
	n := len(buf)
	var eof error
	if off+uint64(n) > total {
		n = int(total - off)
		eof = io.EOF
	}
	p := buf[:n]
	done := 0
	if off < s.memSize {
		part := n
		if rest := s.memSize - off; uint64(part) > rest {
			part = int(rest)
		}
		read, err := s.bundle.ReadAt(ctx, p[:part], off)
		if err != nil && err != io.EOF {
			return read, err
		}
		if read != part {
			return read, fmt.Errorf("short snapshot source read")
		}
		done += part
		off += uint64(part)
	}
	if done < n {
		if err := s.validateOriginal(ctx); err != nil {
			return done, err
		}
		copy(p[done:], s.zip[off-s.memSize:])
	}
	return n, eof
}

func (s *memZipSource) validateOriginal(ctx context.Context) error {
	if !s.validated {
		s.validated = true
		if s.originalSize != s.bundle.Size() || s.memSize > s.originalSize {
			s.validateErr = fmt.Errorf("snapshot source geometry changed")
		} else {
			s.validateErr = consumeSource(ctx, s.bundle, s.memSize)
		}
	}
	return s.validateErr
}

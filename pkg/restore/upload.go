package restore

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
func UploadLocal(ctx context.Context, snapshotPath string, mcfg *manifest.Config, logf func(string, ...any)) (string, error) {
	if mcfg == nil {
		return "", fmt.Errorf("manifest config is required")
	}
	ing, err := mcfg.NewIngester(mcfg.IngestKeyFunc(), nil)
	if err != nil {
		return "", fmt.Errorf("ingester: %w", err)
	}
	defer ing.Close()
	p := newSnapshotPublisher(ctx, logf)
	p.ing = ing
	p.manifestConfig = mcfg
	return p.publishSnapshot(snapshotPath, "")
}

// PublishLocalToLocation publishes a local snapshot graph into one trusted
// named file location. It writes only content-addressed files and returns the
// canonical located root ref.
func PublishLocalToLocation(ctx context.Context, snapshotPath, location, directory string, logf func(string, ...any)) (string, error) {
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
	p := newSnapshotPublisher(ctx, logf)
	p.location = location
	p.directory = filepath.Clean(directory)
	return p.publishSnapshot(snapshotPath, "")
}

type snapshotPublisher struct {
	ctx            context.Context
	ing            ingest.Ingester
	manifestConfig *manifest.Config
	location       string
	directory      string
	logf           func(string, ...any)
	done           map[string]string
	visiting       map[string]bool
}

func newSnapshotPublisher(ctx context.Context, logf func(string, ...any)) *snapshotPublisher {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &snapshotPublisher{
		ctx:      ctx,
		logf:     logf,
		done:     make(map[string]string),
		visiting: make(map[string]bool),
	}
}

func (p *snapshotPublisher) publishSnapshot(snapshotPath, wantDigest string) (result string, retErr error) {
	realPath, err := filepath.EvalSymlinks(snapshotPath)
	if err != nil {
		return "", fmt.Errorf("open snapshot: %w", err)
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil {
		return "", err
	}
	if p.visiting[realPath] {
		return "", fmt.Errorf("upload-snapshot: cycle at %s", realPath)
	}
	p.visiting[realPath] = true
	defer func() { delete(p.visiting, realPath) }()

	bundle, bundleDigest, err := openTarArtifact(realPath)
	if err != nil {
		return "", fmt.Errorf("open snapshot: %w", err)
	}
	defer bundle.Close()
	if wantDigest != "" {
		if err := matchDigest(bundleDigest, wantDigest); err != nil {
			return "", fmt.Errorf("open snapshot: %w", err)
		}
	} else if err := validateContentAddressedName(realPath, bundleDigest); err != nil {
		return "", fmt.Errorf("open snapshot: %w", err)
	}
	if err := verifyArtifactFile(realPath, bundleDigest, int64(bundle.Size())); err != nil {
		return "", fmt.Errorf("open snapshot: %w", err)
	}
	if ref, ok := p.done[realPath]; ok {
		return ref, nil
	}
	bundleSize := int64(bundle.Size())
	entries, parsed, err := readSnapshotEntries(p.ctx, bundle, bundleSize)
	if err != nil {
		return "", err
	}
	bundleDir := filepath.Dir(realPath)

	for i, ref := range parsed.FromRefs {
		parsed.FromRefs[i], err = p.publishRef(fmt.Sprintf("memory parent %d", i), ref, bundleDir, true)
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
		return "", fmt.Errorf("snapshot %s too small (%d) for its memory section (%d)", realPath, bundleSize, memSize)
	}
	src := &memZipSource{bundle: bundle, memSize: memSize, zip: zipBytes}
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
		digest, writeErr := tarstream.WriteTo(p.ctx, tmp, "snapshot", src)
		closeErr := tmp.Close()
		if writeErr != nil {
			return "", fmt.Errorf("rebuild snapshot: %w", writeErr)
		}
		if closeErr != nil {
			return "", closeErr
		}
		result, err = p.publishLocationFile(tmpPath, ".snapshot", digest, false)
		if err != nil {
			return "", err
		}
	}
	p.done[realPath] = result
	return result, nil
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
	if *baseRef, err = p.publishRef(label+" base", *baseRef, bundleDir, false); err != nil {
		return err
	}
	if *base, err = p.publishRef(label+" top", *base, bundleDir, false); err != nil {
		return err
	}
	for i, ref := range *baseFromRefs {
		(*baseFromRefs)[i], err = p.publishRef(fmt.Sprintf("%s parent %d", label, i), ref, bundleDir, false)
		if err != nil {
			return err
		}
	}
	if overlay == nil {
		return nil
	}
	if overlay.Base, err = p.publishRef(label+" overlay", overlay.Base, bundleDir, false); err != nil {
		return err
	}
	for i, ref := range overlay.BaseFromRefs {
		overlay.BaseFromRefs[i], err = p.publishRef(fmt.Sprintf("%s overlay parent %d", label, i), ref, bundleDir, false)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *snapshotPublisher) publishRef(label, raw, relativeDir string, snapshotRef bool) (string, error) {
	if raw == "" {
		return "", nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", fmt.Errorf("upload-snapshot: %s: %w", label, err)
	}
	if ref.Scheme == manifest.RefSchemeManifest && p.manifestConfig != nil {
		key, err := manifest.ParseHexKey(ref.Path)
		if err != nil {
			return "", fmt.Errorf("upload-snapshot: %s: %w", label, err)
		}
		if err := p.manifestConfig.CheckManifest(p.ctx, key); err != nil {
			return "", fmt.Errorf("upload-snapshot: %s %q: %w", label, ref.String(), err)
		}
	}
	if ref.Location != "" && ref.Location == p.location {
		if err := p.validateLocatedRef(label, ref); err != nil {
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
	if snapshotRef {
		return p.publishSnapshot(path, ref.Digest)
	}
	return p.publishLeaf(label, path, ref.Digest)
}

func (p *snapshotPublisher) validateLocatedRef(label string, ref manifest.Ref) error {
	path := filepath.Join(p.directory, ref.Path)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("upload-snapshot: %s %q: %w", label, ref.String(), err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("upload-snapshot: %s %q: location target is not a regular file", label, ref.String())
	}
	stream, digest, err := openTarArtifact(path)
	if err != nil {
		return fmt.Errorf("upload-snapshot: %s %q: %w", label, ref.String(), err)
	}
	logicalSize := int64(stream.Size())
	stream.Close()
	if ref.Digest != "" {
		if err := matchDigest(digest, ref.Digest); err != nil {
			return fmt.Errorf("upload-snapshot: %s %q: %w", label, ref.String(), err)
		}
	}
	if err := validateContentAddressedName(path, digest); err != nil {
		return fmt.Errorf("upload-snapshot: %s %q: %w", label, ref.String(), err)
	}
	if err := verifyArtifactFile(path, digest, logicalSize); err != nil {
		return fmt.Errorf("upload-snapshot: %s %q: %w", label, ref.String(), err)
	}
	return nil
}

func (p *snapshotPublisher) publishLeaf(label, path, wantDigest string) (string, error) {
	stream, digest, err := openTarArtifact(path)
	if err != nil {
		return "", fmt.Errorf("upload-snapshot: %s: %w", label, err)
	}
	defer stream.Close()
	if err := verifyArtifactFile(path, digest, int64(stream.Size())); err != nil {
		return "", fmt.Errorf("upload-snapshot: %s: %w", label, err)
	}
	if wantDigest != "" {
		if err := matchDigest(digest, wantDigest); err != nil {
			return "", err
		}
	} else if err := validateContentAddressedName(path, digest); err != nil {
		return "", fmt.Errorf("upload-snapshot: %s: %w", label, err)
	}

	if p.ing != nil {
		res, err := p.ing.Ingest(p.ctx, stream, ingest.IngestOption{OnProgress: p.progress(label)})
		if err != nil {
			return "", fmt.Errorf("upload-snapshot: ingest %s: %w", label, err)
		}
		key := manifest.HexKey(res.ManifestKey)
		p.logf("upload-snapshot: %s %s → manifest://%s (stored=%d dedup=%d)", label, path, key, res.StoredChunks, res.DedupChunks)
		return "manifest://" + key, nil
	}
	ext := filepath.Ext(path)
	return p.publishLocationFile(path, ext, digest, wantDigest != "")
}

func (p *snapshotPublisher) publishLocationFile(sourcePath, ext, digest string, keepDigest bool) (string, error) {
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	basename := hexDigest + ext
	destination := filepath.Join(p.directory, basename)
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		return "", err
	}
	if destInfo, statErr := os.Lstat(destination); statErr == nil {
		if !destInfo.Mode().IsRegular() {
			return "", fmt.Errorf("ref location destination is not a regular file: %s", destination)
		}
		if destInfo.Size() == sourceInfo.Size() && verifyArtifactFile(destination, digest, 0) == nil {
			return p.locatedRef(basename, hexDigest, keepDigest), nil
		}
	} else if !os.IsNotExist(statErr) {
		return "", statErr
	}

	in, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	defer in.Close()
	// Named locations deliberately do not require temporary-file rename support.
	// Valid content-addressed files are reused; an invalid final name is repaired
	// in place by one sequential write.
	out, err := openLocationDestination(destination)
	if err != nil {
		return "", err
	}
	copyErr := copyAndVerifyArtifact(out, in, sourceInfo.Size(), digest)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := verifyArtifactFile(destination, digest, 0); err != nil {
		return "", fmt.Errorf("verify published %s: %w", destination, err)
	}
	p.logf("upload-snapshot: published %s", destination)
	return p.locatedRef(basename, hexDigest, keepDigest), nil
}

func openLocationDestination(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o644)
	if err != nil {
		return nil, err
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("ref location destination is not a regular file: %s", path)
	}
	if err := unix.Ftruncate(fd, 0); err != nil {
		return nil, err
	}
	closeFD = false
	return os.NewFile(uintptr(fd), path), nil
}

func (p *snapshotPublisher) locatedRef(basename, digest string, keepDigest bool) string {
	ref := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: basename, Location: p.location}
	if keepDigest {
		ref.Digest = digest
	}
	return ref.String()
}

const tarstreamTrailerSize = int64(3 * 512) // marker header + two zero blocks

func copyAndVerifyArtifact(dst io.Writer, src io.Reader, size int64, wantDigest string) error {
	if size < tarstreamTrailerSize {
		return fmt.Errorf("tarstream artifact is too small: %d", size)
	}
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(dst, h), src, size-tarstreamTrailerSize); err != nil {
		return err
	}
	if _, err := io.CopyN(dst, src, tarstreamTrailerSize); err != nil {
		return err
	}
	got := fmt.Sprintf("sha256:%x", h.Sum(nil))
	if got != wantDigest {
		return fmt.Errorf("tarstream digest mismatch: got %s, want %s", got, wantDigest)
	}
	return nil
}

func verifyArtifactFile(path, wantDigest string, wantLogicalSize int64) error {
	stream, digest, err := openTarArtifact(path)
	if err != nil {
		return err
	}
	logicalSize := int64(stream.Size())
	stream.Close()
	if digest != wantDigest {
		return fmt.Errorf("digest marker mismatch: got %s, want %s", digest, wantDigest)
	}
	if wantLogicalSize > 0 && logicalSize != wantLogicalSize {
		return fmt.Errorf("logical size mismatch: got %d, want %d", logicalSize, wantLogicalSize)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return copyAndVerifyArtifact(io.Discard, f, info.Size(), wantDigest)
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
	bundle  sparse.Source
	memSize uint64
	zip     []byte
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
		if _, err := s.bundle.ReadAt(ctx, p[:part], off); err != nil && err != io.EOF {
			return 0, err
		}
		done += part
		off += uint64(part)
	}
	if done < n {
		copy(p[done:], s.zip[off-s.memSize:])
	}
	return n, eof
}

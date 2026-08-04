package snapshot

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"golang.org/x/sys/unix"
)

// HexKey hex-encodes a content key for use in `manifest://<hex>` refs.
func HexKey(k store.ContentKey) string {
	return hex.EncodeToString(k[:])
}

// SnapshotSink absorbs the two large snapshot artifacts — the blk1 overlay and
// the memory+ZIP bundle — straight from their sources to a destination,
// WITHOUT staging them in /run tmpfs. Two impls share this interface:
//
//   - FileSink packs scheme-qualified content-addressed local tarstream
//     artifacts (<digest>.overlay / <digest>.snapshot) under an output dir.
//   - IngestSink streams to a manifest store via ingest.Ingester (--upload).
//
// Each method takes the source as an io.ReadSeeker plus its authoritative hole
// map. Memory holes come from SEEK_HOLE on the live memfd; overlay holes come
// from BlockCOW's dirty bitmap. From the artifact on, the tar envelope is the
// hole authority. The sink reads only resident extents. They return the
// artifact's ref (file://<digest>.ext@<scheme>:<digest> | manifest://<key>)
// and, for file mode, its local path ("" for ingest).
type SnapshotSink interface {
	AbsorbOverlay(ctx context.Context, diff io.ReadSeeker, holes []sparse.Extent) (ref, path string, err error)
	AbsorbBundle(ctx context.Context, mem io.ReadSeeker, holes []sparse.Extent, zip io.Reader) (ref, path string, err error)
}

// ---------------------------------------------------------------------------
// FileSink — sparse local files, content-addressed while tarstream writes.
// ---------------------------------------------------------------------------

type FileSink struct {
	outDir    string
	sandboxID string
	codec     tarstream.Codec
	required  bool
	logf      func(string, ...any)
}

func NewFileSink(outDir, sandboxID string, codec tarstream.Codec, required bool, logf func(string, ...any)) *FileSink {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &FileSink{outDir: outDir, sandboxID: sandboxID, codec: codec, required: required, logf: logf}
}

func (s *FileSink) AbsorbOverlay(ctx context.Context, diff io.ReadSeeker, holes []sparse.Extent) (string, string, error) {
	size, err := seekerSize(diff)
	if err != nil {
		return "", "", err
	}
	scheme, digest, final, err := s.writeArtifact(ctx, "overlay",
		&seekerSource{rs: diff, size: uint64(size), holes: holes})
	if err != nil {
		return "", "", err
	}
	s.logf("snapshot: %s.overlay written", digest[:12])
	return localArtifactRef(digest+".overlay", scheme, digest), final, nil
}

func (s *FileSink) AbsorbBundle(ctx context.Context, mem io.ReadSeeker, holes []sparse.Extent, zip io.Reader) (string, string, error) {
	memSize, err := seekerSize(mem)
	if err != nil {
		return "", "", err
	}
	tail, err := io.ReadAll(zip) // ZIP trailer is small (KB)
	if err != nil {
		return "", "", fmt.Errorf("read zip: %w", err)
	}
	concat := &concatReadSeeker{mem: mem, memSize: memSize, tail: tail}
	scheme, digest, final, err := s.writeArtifact(ctx, "snapshot",
		&seekerSource{rs: concat, size: uint64(memSize) + uint64(len(tail)), holes: holes})
	if err != nil {
		return "", "", err
	}
	// <sid>.snapshot symlink → the immutable content-addressed name, so
	// File-mode from_refs chains reference the immutable digest-named snapshot
	// (docs/sandbox.md §6.1).
	link := filepath.Join(s.outDir, s.sandboxID+".snapshot")
	_ = os.Remove(link)
	if err := os.Symlink(digest+".snapshot", link); err != nil {
		return "", "", fmt.Errorf("symlink %s.snapshot: %w", s.sandboxID, err)
	}
	s.logf("snapshot: %s.snapshot written", digest[:12])
	return localArtifactRef(digest+".snapshot", scheme, digest), final, nil
}

// writeArtifact packs src as a tarstream artifact (payload named kind plus a
// digest marker) at a unique same-directory temporary path, hashing the prefix
// while writing, then commits without replacement to <digest>.<kind>. Only data
// extents flow (holes ride the envelope map); the artifact file itself is dense
// and survives non-sparse-aware copies and filesystems.
func (s *FileSink) writeArtifact(ctx context.Context, kind string, src sparse.Source) (string, string, string, error) {
	if s.required && s.codec == nil {
		return "", "", "", fmt.Errorf("pack %s: required policy has no codec", kind)
	}
	f, err := os.CreateTemp(s.outDir, s.sandboxID+"."+kind+".*.partial")
	if err != nil {
		return "", "", "", err
	}
	tmp := f.Name()
	keepTmp := false
	defer func() {
		if !keepTmp {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return "", "", "", fmt.Errorf("set %s temporary permissions: %w", kind, err)
	}
	var options []tarstream.WriteOption
	if s.codec != nil {
		options = append(options, tarstream.WithCodec(s.codec, s.required))
	}
	scheme, digest, err := tarstream.WriteTo(ctx, f, kind, src, options...)
	if err != nil {
		_ = f.Close()
		return "", "", "", fmt.Errorf("pack %s: %w", kind, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", "", "", err
	}
	if err := f.Close(); err != nil {
		return "", "", "", err
	}
	final := filepath.Join(s.outDir, digest+"."+kind)
	err = unix.Renameat2(unix.AT_FDCWD, tmp, unix.AT_FDCWD, final, unix.RENAME_NOREPLACE)
	if err == unix.EEXIST {
		// A codec-backed write always emits ciphertext, including in auto
		// mode. Reuse must therefore prove that an existing final has the
		// same encoding as the output we attempted to commit.
		if validateErr := validateArtifactFile(ctx, final, kind, src.Size(), s.codec, s.codec != nil, scheme, digest); validateErr != nil {
			return "", "", "", fmt.Errorf("reuse existing %s: %w", kind, validateErr)
		}
		return scheme, digest, final, nil
	}
	if err != nil {
		return "", "", "", fmt.Errorf("commit %s without replacement: %w", kind, err)
	}
	keepTmp = true // rename consumed the path; deferred cleanup has nothing to remove
	dir, err := os.Open(s.outDir)
	if err != nil {
		return "", "", "", fmt.Errorf("open snapshot output directory: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return "", "", "", fmt.Errorf("sync snapshot output directory: %w", syncErr)
	}
	if closeErr != nil {
		return "", "", "", fmt.Errorf("close snapshot output directory: %w", closeErr)
	}
	return scheme, digest, final, nil
}

func localArtifactRef(basename, scheme, digest string) string {
	ref := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: basename, DigestScheme: scheme, Digest: digest}
	return ref.String()
}

func validateArtifactFile(ctx context.Context, path, name string, logicalSize uint64, codec tarstream.Codec, required bool, scheme, digest string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
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
	source, _, err := tarstream.SourceFrom(f, name, options...)
	if err != nil {
		return err
	}
	if source.Size() != logicalSize {
		return fmt.Errorf("logical size mismatch")
	}
	return consumeSource(ctx, source)
}

func consumeSource(ctx context.Context, source sparse.Source) error {
	buffer := make([]byte, 128*1024)
	for offset := uint64(0); offset < source.Size(); {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind, end, err := source.RunAt(offset, source.Size()-offset)
		if err != nil {
			return err
		}
		if end <= offset || end > source.Size() {
			return fmt.Errorf("invalid sparse run")
		}
		if kind != sparse.Hole {
			for position := offset; position < end; {
				chunk := min(uint64(len(buffer)), end-position)
				n, readErr := source.ReadAt(ctx, buffer[:int(chunk)], position)
				if readErr != nil && readErr != io.EOF {
					return readErr
				}
				if n != int(chunk) {
					return fmt.Errorf("short source read")
				}
				position += chunk
			}
		}
		offset = end
	}
	return nil
}

// ---------------------------------------------------------------------------
// IngestSink — manifest store via ingest.Ingester (--upload).
// ---------------------------------------------------------------------------

type IngestSink struct {
	ing  ingest.Ingester
	logf func(string, ...any)
	// Captured for the caller's Response (read via Results after Take).
	overlayRes *ingest.Result
	bundleRes  *ingest.Result
}

func NewIngestSink(ing ingest.Ingester, logf func(string, ...any)) *IngestSink {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &IngestSink{ing: ing, logf: logf}
}

// Results returns the overlay and bundle ingest results (nil until the
// corresponding Absorb call succeeds). Used by the caller to report stats.
func (s *IngestSink) Results() (overlay, bundle *ingest.Result) {
	return s.overlayRes, s.bundleRes
}

func (s *IngestSink) AbsorbOverlay(ctx context.Context, diff io.ReadSeeker, holes []sparse.Extent) (string, string, error) {
	size, err := seekerSize(diff)
	if err != nil {
		return "", "", err
	}
	res, err := s.run(ctx, &seekerSource{rs: diff, size: uint64(size), holes: holes}, holes, "overlay")
	if err != nil {
		return "", "", err
	}
	s.overlayRes = res
	return "manifest://" + HexKey(res.ManifestKey), "", nil
}

func (s *IngestSink) AbsorbBundle(ctx context.Context, mem io.ReadSeeker, holes []sparse.Extent, zip io.Reader) (string, string, error) {
	memSize, err := seekerSize(mem)
	if err != nil {
		return "", "", err
	}
	tail, err := io.ReadAll(zip) // ZIP trailer is small (KB)
	if err != nil {
		return "", "", fmt.Errorf("read zip: %w", err)
	}
	// Present [mem][zip] as one sparse source so ingest records the
	// memory holes and reads only resident extents — no tmpfs copy of
	// the bundle (the ZIP tail is plain data after the memory section).
	concat := &concatReadSeeker{mem: mem, memSize: memSize, tail: tail}
	src := &seekerSource{rs: concat, size: uint64(memSize) + uint64(len(tail)), holes: holes}
	res, err := s.run(ctx, src, holes, "memory section")
	if err != nil {
		return "", "", err
	}
	s.bundleRes = res
	return "manifest://" + HexKey(res.ManifestKey), "", nil
}

// run ingests src with a throttled progress log (effective denominator = size
// minus hole bytes, so % reflects real work).
func (s *IngestSink) run(ctx context.Context, src sparse.Source, holes []sparse.Extent, label string) (*ingest.Result, error) {
	size := src.Size()
	var holeBytes uint64
	for _, h := range holes {
		holeBytes += h.Size
	}
	effective := size
	if holeBytes < size {
		effective = size - holeBytes
	}
	const mib = 1 << 20
	start := time.Now()
	lastT := start
	var lastProcessed uint64
	onProgress := func(processed, _ uint64) {
		now := time.Now()
		if now.Sub(lastT) < 2*time.Second {
			return
		}
		rate := float64(processed-lastProcessed) / now.Sub(lastT).Seconds() / mib
		pct := uint64(0)
		if effective > 0 {
			pct = min(processed*100/effective, 100)
		}
		s.logf("upload: %s %d/%d MiB (%d%%) %.0f MiB/s", label, processed/mib, effective/mib, pct, rate)
		lastT, lastProcessed = now, processed
	}
	res, err := s.ing.Ingest(ctx, src, ingest.IngestOption{OnProgress: onProgress})
	if err != nil {
		return nil, fmt.Errorf("ingest %s: %w", label, err)
	}
	s.logf("upload: %s ingested key=%x stored=%d dedup=%d in %.1fs",
		label, res.ManifestKey, res.StoredChunks, res.DedupChunks, time.Since(start).Seconds())
	return res, nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// seekerSize returns rs's length and rewinds it to the start.
func seekerSize(rs io.ReadSeeker) (int64, error) {
	n, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return n, nil
}

// residentBytes = size minus the sum of hole sizes.
func residentBytes(size int64, holes []sparse.Extent) uint64 {
	var holeBytes uint64
	for _, h := range holes {
		holeBytes += h.Size
	}
	if holeBytes >= uint64(size) {
		return 0
	}
	return uint64(size) - holeBytes
}

// fdReaderAt is a non-owning io.ReaderAt over a raw fd (pread); used to present
// the memfd as an io.ReadSeeker (via io.SectionReader) without an *os.File
// wrapper whose finalizer would close the shared fd.
type fdReaderAt int

func (fd fdReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := unix.Pread(int(fd), p, off)
	if err != nil {
		return n, err
	}
	if n == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return n, nil
}

// memfdReader presents memfd[0,size) as an io.ReadSeeker (pread-backed, no fd
// ownership). CH is paused during snapshot, so the memfd content is stable.
func memfdReader(fd int, size int64) io.ReadSeeker {
	return io.NewSectionReader(fdReaderAt(fd), 0, size)
}

// concatReadSeeker presents [mem (0..memSize)] followed by [tail] as one
// io.ReadSeeker, so ingest.Ingest can Seek to each data segment across the
// memory/ZIP boundary (skipping memory holes → resident-only reads).
type concatReadSeeker struct {
	mem     io.ReadSeeker
	memSize int64
	tail    []byte
	pos     int64
}

func (c *concatReadSeeker) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = c.pos + off
	case io.SeekEnd:
		abs = c.memSize + int64(len(c.tail)) + off
	default:
		return 0, fmt.Errorf("concat: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("concat: negative position %d", abs)
	}
	c.pos = abs
	if abs < c.memSize {
		if _, err := c.mem.Seek(abs, io.SeekStart); err != nil {
			return 0, err
		}
	}
	return abs, nil
}

func (c *concatReadSeeker) Read(p []byte) (int, error) {
	total := c.memSize + int64(len(c.tail))
	if c.pos >= total {
		return 0, io.EOF
	}
	if c.pos < c.memSize {
		if maxN := c.memSize - c.pos; int64(len(p)) > maxN {
			p = p[:maxN]
		}
		n, err := c.mem.Read(p)
		c.pos += int64(n)
		if err == io.EOF {
			err = nil // memory section ended; the tail still follows
		}
		return n, err
	}
	n := copy(p, c.tail[c.pos-c.memSize:])
	c.pos += int64(n)
	return n, nil
}

// seekerSource adapts the sink's wire-in pair (io.ReadSeeker + static
// hole map) to a sparse.Source for ingest: RunAt classifies from the
// hole map, ReadAt is Seek+ReadFull — monotone, which is exactly the
// Source baseline contract and how ingest consumes (data segments in
// ascending order). Synthetic merge readers (Read+Seek only, no
// io.ReaderAt) stay usable this way.
type seekerSource struct {
	rs    io.ReadSeeker
	size  uint64
	holes []sparse.Extent // sorted, disjoint (WalkHoles guarantees)
}

func (s *seekerSource) Size() uint64 { return s.size }

func (s *seekerSource) RunAt(offset, limit uint64) (sparse.RunKind, uint64, error) {
	if offset >= s.size {
		return 0, 0, io.EOF
	}
	limEnd := offset + limit
	if limEnd < offset || limEnd > s.size {
		limEnd = s.size
	}
	isHole, end := holeRun(int64(offset), s.holes, int64(s.size))
	e := uint64(end)
	if e > limEnd {
		e = limEnd
	}
	if isHole {
		return sparse.Hole, e, nil
	}
	return sparse.Data, e, nil
}

func (s *seekerSource) ReadAt(_ context.Context, buf []byte, offset uint64) (int, error) {
	if offset >= s.size {
		return 0, io.EOF
	}
	n := len(buf)
	var eof error
	if offset+uint64(n) > s.size {
		n = int(s.size - offset)
		eof = io.EOF
	}
	if _, err := s.rs.Seek(int64(offset), io.SeekStart); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(s.rs, buf[:n]); err != nil {
		return 0, err
	}
	return n, eof
}

package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
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

// finalValidator reports the file identity it validated alongside the
// outcome, so callers can bind later removal to that exact inode.
type finalValidator func(context.Context, string, bool) (os.FileInfo, error)

// publishFileExclusively publishes sourcePath to destination with the same
// no-lock, rename-free shared-filesystem contract as named ref-location
// publication: O_CREATE|O_EXCL ownership, sequential cancellable copy, full
// validation, file and parent-directory sync, and last-active-writer eventual
// publication when concurrent writers repair a confirmed-invalid regular
// final. Cancellation, transient I/O, permission, and unknown validation
// errors never remove an existing path; neither do symlinks or other
// non-regular finals. Repair removes the final only while the path still
// names the inode whose validation reported the mismatch, so a concurrent
// publisher that completes the same inode is never deleted underneath its
// own successful validation.
func publishFileExclusively(ctx context.Context, sourcePath, destination string, validate finalValidator, what string) (reused bool, err error) {
	if _, err := validate(ctx, sourcePath, false); err != nil {
		return false, fmt.Errorf("publish %s: validate source: %w", what, err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("publish %s: %w", what, err)
		}
		created, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if !os.IsExist(err) {
				return false, fmt.Errorf("publish %s: create final exclusively: %w", what, err)
			}
			invalidInfo, validateErr := validate(ctx, destination, true)
			if os.IsNotExist(validateErr) {
				continue // raced away between the exclusive open and validation
			}
			if validateErr == nil {
				if err := syncDirectory(filepath.Dir(destination)); err != nil {
					return false, fmt.Errorf("publish %s: sync parent directory: %w", what, err)
				}
				return true, nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, fmt.Errorf("publish %s: %w", what, ctxErr)
			}
			if errors.Is(validateErr, context.Canceled) || errors.Is(validateErr, context.DeadlineExceeded) {
				return false, fmt.Errorf("reuse existing %s: %w", what, validateErr)
			}
			if os.IsNotExist(validateErr) {
				continue
			}
			if !isConfirmedFinalMismatch(validateErr) {
				return false, fmt.Errorf("reuse existing %s: %w", what, validateErr)
			}
			if err := removeFinalIfStill(destination, invalidInfo, true); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				if errors.Is(err, errFinalReplaced) {
					continue // another publisher owns the path now; revalidate
				}
				return false, fmt.Errorf("reuse existing %s: invalid final (%v) cannot be replaced: %w", what, validateErr, err)
			}
			continue
		}
		ownedInfo, err := created.Stat()
		if err != nil {
			_ = created.Close()
			if removeErr := os.Remove(destination); removeErr != nil && !os.IsNotExist(removeErr) {
				return false, fmt.Errorf("publish %s: stat created final: %v; remove incomplete final: %w", what, err, removeErr)
			}
			return false, fmt.Errorf("publish %s: stat created final: %w", what, err)
		}
		owned := true
		defer func() {
			if owned {
				_ = removeFinalIfStill(destination, ownedInfo, false)
			}
		}()
		if err := created.Chmod(0o644); err != nil {
			_ = created.Close()
			return false, fmt.Errorf("publish %s: set final permissions: %w", what, err)
		}
		if err := copySyncAndClose(ctx, created, sourcePath); err != nil {
			return false, fmt.Errorf("publish %s: write final: %w", what, err)
		}
		if _, err := validate(ctx, destination, false); err != nil {
			return false, fmt.Errorf("publish %s: validate final: %w", what, err)
		}
		if err := syncDirectory(filepath.Dir(destination)); err != nil {
			return false, fmt.Errorf("publish %s: sync parent directory: %w", what, err)
		}
		owned = false
		return false, nil
	}
}

var errFinalMismatch = errors.New("published final mismatch")

func isConfirmedFinalMismatch(err error) bool {
	for _, mismatch := range []error{
		errFinalMismatch,
		io.EOF,
		io.ErrUnexpectedEOF,
		tarstream.ErrCodecRequired,
		tarstream.ErrPlaintextForbidden,
		tarstream.ErrUnsupportedVersion,
		tarstream.ErrMalformedEnvelope,
		tarstream.ErrAuthentication,
		tarstream.ErrDigestMismatch,
		tarstream.ErrInvalidCanonicalTarstream,
		tarstream.ErrUnsupportedEncoding,
		tarstream.ErrNotFound,
	} {
		if errors.Is(err, mismatch) {
			return true
		}
	}
	return false
}

// errFinalReplaced reports that the final path no longer names the exact file
// the caller observed; that file's outcome is not this publisher's to decide.
var errFinalReplaced = errors.New("final path names a different publisher's file")

// removeFinalIfStill removes the final only while the path still names the
// observed file. With unchangedContents it also requires unchanged size and
// modification time, so a concurrent publisher that completes the observed
// inode is never deleted underneath its owner; owners of a failed partial
// write pass false because they legitimately changed their own file after
// observing it.
func removeFinalIfStill(path string, observed os.FileInfo, unchangedContents bool) error {
	if observed == nil {
		return os.Remove(path)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(observed, current) {
		return errFinalReplaced
	}
	if unchangedContents &&
		(current.Size() != observed.Size() || !current.ModTime().Equal(observed.ModTime())) {
		return errFinalReplaced
	}
	if !current.Mode().IsRegular() {
		return fmt.Errorf("existing final is not a regular file")
	}
	return os.Remove(path)
}

// exclusivePublishCopy is a test seam for failures after the publisher owns
// the exclusively-created final. Production copies are sequential and
// cancellable.
var exclusivePublishCopy = copySequentially

func copySequentially(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buf := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := source.Read(buf)
		if n > 0 {
			written, writeErr := destination.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func copySyncAndClose(ctx context.Context, destination *os.File, sourcePath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		_ = destination.Close()
		return err
	}
	info, statErr := source.Stat()
	var copied int64
	if statErr == nil {
		copied, err = exclusivePublishCopy(ctx, destination, source)
	}
	sourceCloseErr := source.Close()
	if statErr != nil {
		err = statErr
	} else if err == nil && copied != info.Size() {
		err = fmt.Errorf("short copy: wrote %d of %d bytes", copied, info.Size())
	} else if err == nil && sourceCloseErr != nil {
		err = sourceCloseErr
	}
	if err == nil {
		err = destination.Sync()
	}
	closeErr := destination.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// ArtifactSink is the narrow lifecycle writer shared by export and snapshot.
// Logical roles are explicit methods; there is intentionally no artifact-kind
// registry or side metadata. Absorb writes immutable content only. Commit*
// publishes the operation root (local alias or Bundle root) last. Take and
// Export take ownership of the sink and close it before deciding whether the
// paused sandbox is resumed or destroyed.
type ArtifactSink interface {
	AbsorbOverlay(ctx context.Context, diff io.ReadSeeker, holes []sparse.Extent) (ref, path string, err error)
	AbsorbOverlaySource(ctx context.Context, source sparse.Source) (ref, path string, err error)
	AbsorbSandbox(ctx context.Context, source sparse.Source) (ref, path string, err error)
	AbsorbSnapshot(ctx context.Context, source sparse.Source) (ref, path string, err error)
	CommitSandbox(ctx context.Context, ref, path string) error
	CommitSnapshot(ctx context.Context, ref, path string) error
	Close() error
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

// AbsorbOverlaySource is the sparse.Source counterpart used by the unified
// offline publisher. It shares the exact same atomic, crypto and reuse path as
// live capture without forcing a random-access Stream through an io.Seeker.
func (s *FileSink) AbsorbOverlaySource(ctx context.Context, source sparse.Source) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("overlay source is nil")
	}
	scheme, digest, path, err := s.writeArtifact(ctx, "overlay", source)
	if err != nil {
		return "", "", err
	}
	return localArtifactRef(filepath.Base(path), scheme, digest), path, nil
}

func (s *FileSink) AbsorbSandbox(ctx context.Context, source sparse.Source) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("sandbox source is nil")
	}
	scheme, digest, final, err := s.writeArtifact(ctx, "sandbox", source)
	if err != nil {
		return "", "", err
	}
	s.logf("artifact: %s.sandbox written", digest[:12])
	return localArtifactRef(digest+".sandbox", scheme, digest), final, nil
}

func (s *FileSink) AbsorbSnapshot(ctx context.Context, source sparse.Source) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("snapshot source is nil")
	}
	scheme, digest, final, err := s.writeArtifact(ctx, "snapshot", source)
	if err != nil {
		return "", "", err
	}
	s.logf("artifact: %s.snapshot written", digest[:12])
	return localArtifactRef(digest+".snapshot", scheme, digest), final, nil
}

func (s *FileSink) CommitSandbox(ctx context.Context, _, path string) error {
	return s.commitAlias(ctx, "sandbox", path)
}

func (s *FileSink) CommitSnapshot(ctx context.Context, _, path string) error {
	return s.commitAlias(ctx, "snapshot", path)
}

func (*FileSink) Close() error { return nil }

func (s *FileSink) commitAlias(ctx context.Context, role, path string) error {
	return commitArtifactAlias(ctx, s.outDir, s.sandboxID, role, path)
}

// writeArtifact packs src as a tarstream artifact (payload named kind plus a
// digest marker) at a unique same-directory temporary path, hashing the prefix
// while writing, then commits without replacement to <digest>.<kind>. Only data
// extents flow (holes ride the envelope map); the artifact file itself is dense
// and survives non-sparse-aware copies and filesystems.
func (s *FileSink) writeArtifact(ctx context.Context, kind string, src sparse.Source) (string, string, string, error) {
	if err := validateArtifactAliasID(s.sandboxID); err != nil {
		return "", "", "", fmt.Errorf("pack %s: %w", kind, err)
	}
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
	// A codec-backed write always emits ciphertext, including in auto mode, so
	// reuse must prove that an existing final has the same encoding as the
	// output we attempted to commit; publishFileExclusively applies that rule.
	validate := func(ctx context.Context, path string, syncFile bool) (os.FileInfo, error) {
		return validateArtifactFile(ctx, path, kind, src.Size(), s.codec, s.codec != nil, scheme, digest, syncFile)
	}
	if _, err := publishFileExclusively(ctx, tmp, final, validate, kind); err != nil {
		return "", "", "", fmt.Errorf("commit %s without replacement: %w", kind, err)
	}
	// Copy-mode publication leaves the temporary file in place; the deferred
	// cleanup removes it once the directory sync below has landed.
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

// validateArtifactFile reports the file identity it validated so callers can
// bind later removal to that exact inode.
func validateArtifactFile(ctx context.Context, path, name string, logicalSize uint64, codec tarstream.Codec, required bool, scheme, digest string, syncFile bool) (os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return info, fmt.Errorf("%w: existing final is not a regular file", errFinalMismatch)
	}
	var options []tarstream.ReadOption
	if codec != nil {
		options = append(options, tarstream.WithCodec(codec, required))
	}
	options = append(options, tarstream.WithExpectedDigest(scheme, digest))
	source, _, err := tarstream.SourceFrom(f, name, options...)
	if err != nil {
		return info, err
	}
	if source.Size() != logicalSize {
		return info, fmt.Errorf("%w: existing final logical size", errFinalMismatch)
	}
	if err := consumeSource(ctx, source); err != nil {
		return info, err
	}
	if syncFile {
		if err := f.Sync(); err != nil {
			return info, fmt.Errorf("sync existing final: %w", err)
		}
	}
	return info, nil
}

func consumeSource(ctx context.Context, source sparse.Source) error {
	buffer := make([]byte, 128*1024)
	for offset := uint64(0); offset < source.Size(); {
		if err := ctx.Err(); err != nil {
			return err
		}
		run, err := source.RunAt(offset, source.Size()-offset)
		if err != nil {
			return err
		}
		if run == nil || run.Offset() != offset || run.End() <= offset || run.End() > source.Size() {
			return fmt.Errorf("invalid sparse run at offset %d", offset)
		}
		kind, end := run.Kind(), run.End()
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
	ing       ingest.Ingester
	closer    io.Closer
	closeOnce sync.Once
	closeErr  error
	logf      func(string, ...any)
	// Captured for the caller's Response (read via Results after Take).
	overlayRes *ingest.Result
	bundleRes  *ingest.Result
	sandboxRes *ingest.Result
}

// ---------------------------------------------------------------------------
// BundleSink — one standard ZIP64 file containing every current Manifest.
// ---------------------------------------------------------------------------

type BundleSink struct {
	outDir    string
	sandboxID string
	cfg       *manifest.Config
	keyFn     ingest.CustomerKeyFunc
	logf      func(string, ...any)

	file      *os.File
	tmpPath   string
	writer    *manifestbundle.Writer
	ing       ingest.Ingester
	admission store.WriteAdmission
	manifests map[store.ContentKey]struct{}

	overlayResults []*ingest.Result
	sandboxResult  *ingest.Result
	bundleResult   *ingest.Result
	finalized      bool
	closed         bool
}

// NewBundleSink retains the no-refs convenience path. Snapshot lifecycle code
// that plans external sources must use NewPlannedBundleSink so admission and
// refs are both immutable before the Writer emits its metadata prefix.
func NewBundleSink(ctx context.Context, outDir, sandboxID string, cfg *manifest.Config, keyFn ingest.CustomerKeyFunc, logf func(string, ...any)) (*BundleSink, error) {
	if cfg == nil {
		return nil, fmt.Errorf("snapshot Bundle requires manifest configuration")
	}
	if keyFn == nil {
		return nil, fmt.Errorf("snapshot Bundle requires customer key resolver")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	admission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot Bundle admission: %w", err)
	}
	return NewPlannedBundleSink(outDir, sandboxID, cfg, keyFn, admission, nil, logf)
}

// NewPlannedBundleSink constructs one Bundle after the caller has resolved its
// sole admission and complete ordered flat refs plan. It never admits again.
func NewPlannedBundleSink(outDir, sandboxID string, cfg *manifest.Config, keyFn ingest.CustomerKeyFunc, admission store.WriteAdmission, refs []string, logf func(string, ...any)) (*BundleSink, error) {
	if cfg == nil {
		return nil, fmt.Errorf("snapshot Bundle requires manifest configuration")
	}
	if keyFn == nil {
		return nil, fmt.Errorf("snapshot Bundle requires customer key resolver")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := validateArtifactAliasID(sandboxID); err != nil {
		return nil, fmt.Errorf("snapshot Bundle: %w", err)
	}
	f, err := os.CreateTemp(outDir, sandboxID+".bundle.*.partial")
	if err != nil {
		return nil, fmt.Errorf("snapshot Bundle temporary file: %w", err)
	}
	fail := func(err error) (*BundleSink, error) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	if err := f.Chmod(0o644); err != nil {
		return fail(fmt.Errorf("snapshot Bundle temporary permissions: %w", err))
	}
	writer, err := manifestbundle.NewWriter(f, admission, manifestbundle.WriterOptions{Refs: refs})
	if err != nil {
		return fail(err)
	}
	ing, err := cfg.NewIngesterWithWriter(keyFn, nil, writer)
	if err != nil {
		return fail(err)
	}
	return &BundleSink{
		outDir: outDir, sandboxID: sandboxID, cfg: cfg, keyFn: keyFn, logf: logf,
		file: f, tmpPath: f.Name(), writer: writer, ing: ing, admission: admission,
		manifests: make(map[store.ContentKey]struct{}),
	}, nil
}

func (s *BundleSink) Admission() store.WriteAdmission { return s.admission }

func (s *BundleSink) Writer() *manifestbundle.Writer { return s.writer }

func (s *BundleSink) Results() (overlays []*ingest.Result, root *ingest.Result) {
	return append([]*ingest.Result(nil), s.overlayResults...), s.bundleResult
}

func (s *BundleSink) SandboxResult() *ingest.Result { return s.sandboxResult }

func (s *BundleSink) AbsorbOverlay(ctx context.Context, diff io.ReadSeeker, holes []sparse.Extent) (string, string, error) {
	size, err := seekerSize(diff)
	if err != nil {
		return "", "", err
	}
	result, err := s.ingestSource(ctx, &seekerSource{rs: diff, size: uint64(size), holes: holes}, holes, "overlay")
	if err != nil {
		return "", "", err
	}
	s.overlayResults = append(s.overlayResults, result)
	return "manifest://" + HexKey(result.ManifestKey), "", nil
}

func (s *BundleSink) AbsorbOverlaySource(ctx context.Context, source sparse.Source) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("overlay source is nil")
	}
	result, err := s.ingestSource(ctx, source, nil, "overlay dependency")
	if err != nil {
		return "", "", err
	}
	s.overlayResults = append(s.overlayResults, result)
	return "manifest://" + HexKey(result.ManifestKey), "", nil
}

func (s *BundleSink) AbsorbSandbox(ctx context.Context, source sparse.Source) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("sandbox source is nil")
	}
	result, err := s.ingestSource(ctx, source, nil, "Sandbox E")
	if err != nil {
		return "", "", err
	}
	s.sandboxResult = result
	return "manifest://" + HexKey(result.ManifestKey), "", nil
}

func (s *BundleSink) AbsorbSnapshot(ctx context.Context, source sparse.Source) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("snapshot source is nil")
	}
	result, err := s.ingestSource(ctx, source, nil, "Snapshot S")
	if err != nil {
		return "", "", err
	}
	s.bundleResult = result
	return "manifest://" + HexKey(result.ManifestKey), "", nil
}

func (s *BundleSink) CommitSandbox(ctx context.Context, ref, _ string) error {
	return s.commitRoot(ctx, ref, "sandbox")
}

func (s *BundleSink) CommitSnapshot(ctx context.Context, ref, _ string) error {
	return s.commitRoot(ctx, ref, "snapshot")
}

func (s *BundleSink) commitRoot(ctx context.Context, ref, role string) error {
	parsed, err := manifest.ParseRef(ref)
	if err != nil || parsed.Scheme != manifest.RefSchemeManifest {
		return fmt.Errorf("commit Bundle %s root: expected manifest ref", role)
	}
	key, err := manifest.ParseKeyRef(parsed.Path)
	if err != nil {
		return fmt.Errorf("commit Bundle %s root: %w", role, err)
	}
	return s.finalize(ctx, key, role)
}

// IngestStream collects one already-decoded local parent or immutable
// artifact into this Bundle's admission. It is intended for pre-pause use.
func (s *BundleSink) IngestStream(ctx context.Context, stream fetch.Stream, label string) (store.ContentKey, error) {
	if stream == nil {
		return store.ContentKey{}, fmt.Errorf("snapshot Bundle import %s: nil stream", label)
	}
	result, err := s.ingestSource(ctx, stream, nil, label)
	if err != nil {
		return store.ContentKey{}, err
	}
	return result.ManifestKey, nil
}

// CopyManifestFromBundle copies an exact complete parent layer when both
// Bundles use the same admission.
func (s *BundleSink) CopyManifestFromBundle(ctx context.Context, key store.ContentKey, source *manifestbundle.Reader) error {
	if err := s.writer.CopyManifestFromBundle(ctx, key, source); err != nil {
		return err
	}
	s.manifests[key] = struct{}{}
	return nil
}

func (s *BundleSink) ingestSource(ctx context.Context, source sparse.Source, holes []sparse.Extent, label string) (*ingest.Result, error) {
	if s.finalized || s.closed {
		return nil, fmt.Errorf("snapshot Bundle is closed")
	}
	var holeBytes uint64
	for _, hole := range holes {
		holeBytes += hole.Size
	}
	effective := source.Size()
	if holeBytes < effective {
		effective -= holeBytes
	}
	const mib = 1 << 20
	started := time.Now()
	last := started
	var previous uint64
	result, err := s.ing.Ingest(ctx, source, ingest.IngestOption{OnProgress: func(processed, _ uint64) {
		now := time.Now()
		if now.Sub(last) < 2*time.Second {
			return
		}
		percent := uint64(0)
		if effective != 0 {
			percent = min(processed*100/effective, 100)
		}
		rate := float64(processed-previous) / now.Sub(last).Seconds() / mib
		s.logf("bundle: %s %d/%d MiB (%d%%) %.0f MiB/s", label, processed/mib, effective/mib, percent, rate)
		last, previous = now, processed
	}})
	if err != nil {
		return nil, fmt.Errorf("snapshot Bundle ingest %s: %w", label, err)
	}
	s.manifests[result.ManifestKey] = struct{}{}
	s.logf("bundle: %s manifest=%s stored=%d dedup=%d in %.1fs", label,
		HexKey(result.ManifestKey), result.StoredChunks, result.DedupChunks, time.Since(started).Seconds())
	return result, nil
}

func (s *BundleSink) finalize(ctx context.Context, root store.ContentKey, role string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.writer.Finalize(root); err != nil {
		return err
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("sync snapshot Bundle: %w", err)
	}
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("close snapshot Bundle: %w", err)
	}
	s.closed = true
	final := filepath.Join(s.outDir, HexKey(root)+".bundle")
	validate := func(ctx context.Context, path string, syncFile bool) (os.FileInfo, error) {
		return s.validateExisting(ctx, path, root, syncFile)
	}
	if _, err := publishFileExclusively(ctx, s.tmpPath, final, validate, "snapshot Bundle"); err != nil {
		return fmt.Errorf("commit snapshot Bundle without replacement: %w", err)
	}
	if err := os.Remove(s.tmpPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove published snapshot Bundle temporary file: %w", err)
	}
	s.tmpPath = ""
	if err := syncDirectory(s.outDir); err != nil {
		return fmt.Errorf("sync snapshot Bundle directory: %w", err)
	}
	if err := commitArtifactAlias(ctx, s.outDir, s.sandboxID, role, final); err != nil {
		return fmt.Errorf("commit %s Bundle alias: %w", role, err)
	}
	s.finalized = true
	s.logf("artifact: %s.bundle written as %s root", HexKey(root)[:12], role)
	return nil
}

// validateExisting reports the file identity it validated so callers can
// bind later removal to that exact inode.
func (s *BundleSink) validateExisting(ctx context.Context, path string, root store.ContentKey, syncFile bool) (os.FileInfo, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return info, fmt.Errorf("%w: existing final is not a regular file", errFinalMismatch)
	}
	reader, err := manifestbundle.NewReader(file, info.Size())
	if err != nil {
		return info, classifyBundleFinalValidationError(err)
	}
	defer reader.Close()
	if reader.Admission() != s.admission {
		return info, fmt.Errorf("%w: recorded admission differs", errFinalMismatch)
	}
	customerKey, err := s.keyFn()
	if err != nil {
		return info, err
	}
	_, decryptor, err := manifestcrypto.New(s.cfg.Crypto)
	if err != nil {
		clear(customerKey[:])
		return info, err
	}
	defer clear(customerKey[:])
	expected := make([]store.ContentKey, 0, len(s.manifests))
	for key := range s.manifests {
		expected = append(expected, key)
	}
	if err := classifyBundleFinalValidationError(reader.FullVerify(ctx, root, customerKey, decryptor,
		manifestbundle.VerifyOptions{ExpectedManifests: expected})); err != nil {
		return info, err
	}
	if syncFile {
		if err := file.Sync(); err != nil {
			return info, fmt.Errorf("sync existing final: %w", err)
		}
	}
	return info, nil
}

func classifyBundleFinalValidationError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		os.IsNotExist(err) || os.IsPermission(err) || errors.Is(err, unix.EIO) || errors.Is(err, unix.ESTALE) {
		return err
	}
	var pathErr *os.PathError
	var linkErr *os.LinkError
	var syscallErr *os.SyscallError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &syscallErr) {
		return err
	}
	return errors.Join(errFinalMismatch, err)
}

func (s *BundleSink) Close() error {
	if s == nil {
		return nil
	}
	var result error
	if !s.closed && s.file != nil {
		result = s.file.Close()
		s.closed = true
	}
	if s.tmpPath != "" {
		if err := os.Remove(s.tmpPath); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
		s.tmpPath = ""
	}
	return result
}

func syncDirectory(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), path)
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func validateArtifactAliasID(sandboxID string) error {
	if sandboxID == "" || sandboxID == "." || sandboxID == ".." || filepath.Base(sandboxID) != sandboxID || strings.ContainsAny(sandboxID, `/\`) {
		return fmt.Errorf("sandbox id %q is not a safe alias component", sandboxID)
	}
	return nil
}

func commitArtifactAlias(ctx context.Context, outDir, sandboxID, role, artifactPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateArtifactAliasID(sandboxID); err != nil {
		return err
	}
	if role != "sandbox" && role != "snapshot" {
		return fmt.Errorf("unsupported artifact alias role %q", role)
	}
	if artifactPath == "" {
		return fmt.Errorf("commit %s alias: empty artifact path", role)
	}
	outAbs, err := filepath.Abs(outDir)
	if err != nil {
		return fmt.Errorf("commit %s alias output directory: %w", role, err)
	}
	artifactAbs, err := filepath.Abs(artifactPath)
	if err != nil {
		return fmt.Errorf("commit %s alias artifact path: %w", role, err)
	}
	if filepath.Dir(artifactAbs) != filepath.Clean(outAbs) {
		return fmt.Errorf("commit %s alias artifact is outside the output directory", role)
	}
	fd, err := unix.Open(artifactAbs, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("commit %s alias artifact: %w", role, err)
	}
	artifact := os.NewFile(uintptr(fd), artifactAbs)
	info, statErr := artifact.Stat()
	closeErr := artifact.Close()
	if statErr != nil || closeErr != nil {
		return fmt.Errorf("commit %s alias artifact: %w", role, errors.Join(statErr, closeErr))
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("commit %s alias artifact is not a regular file", role)
	}

	alias := filepath.Join(outAbs, sandboxID+"."+role)
	oldTarget := ""
	hadAlias := false
	if oldInfo, lstatErr := os.Lstat(alias); lstatErr == nil {
		if oldInfo.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("commit %s alias refuses to replace a non-symlink", role)
		}
		oldTarget, err = os.Readlink(alias)
		if err != nil {
			return fmt.Errorf("read existing %s alias: %w", role, err)
		}
		hadAlias = true
	} else if !os.IsNotExist(lstatErr) {
		return fmt.Errorf("inspect existing %s alias: %w", role, lstatErr)
	}

	target := filepath.Base(artifactAbs)
	temporary, err := createAliasSymlink(outAbs, sandboxID, role, target)
	if err != nil {
		return err
	}
	temporaryOpen := true
	defer func() {
		if temporaryOpen {
			_ = os.Remove(temporary)
		}
	}()
	if err := os.Rename(temporary, alias); err != nil {
		return fmt.Errorf("commit %s alias: %w", role, err)
	}
	temporaryOpen = false
	if err := syncDirectory(outAbs); err == nil {
		return nil
	} else {
		rollbackErr := rollbackArtifactAlias(outAbs, alias, sandboxID, role, target, hadAlias, oldTarget)
		return fmt.Errorf("sync %s alias directory: %w", role, errors.Join(err, rollbackErr))
	}
}

func createAliasSymlink(outDir, sandboxID, role, target string) (string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var suffix [16]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("create %s alias randomness: %w", role, err)
		}
		path := filepath.Join(outDir, "."+sandboxID+"."+role+"."+hex.EncodeToString(suffix[:])+".tmp")
		if err := os.Symlink(target, path); err == nil {
			return path, nil
		} else if !os.IsExist(err) {
			return "", fmt.Errorf("create %s alias temporary symlink: %w", role, err)
		}
	}
	return "", fmt.Errorf("create %s alias temporary symlink: name collision limit exceeded", role)
}

func rollbackArtifactAlias(outDir, alias, sandboxID, role, newTarget string, hadAlias bool, oldTarget string) error {
	if hadAlias {
		temporary, err := createAliasSymlink(outDir, sandboxID, role, oldTarget)
		if err != nil {
			return fmt.Errorf("restore prior %s alias: %w", role, err)
		}
		if err := os.Rename(temporary, alias); err != nil {
			_ = os.Remove(temporary)
			return fmt.Errorf("restore prior %s alias: %w", role, err)
		}
	} else {
		current, err := os.Readlink(alias)
		if err != nil {
			return fmt.Errorf("remove failed %s alias: %w", role, err)
		}
		if current != newTarget {
			return fmt.Errorf("remove failed %s alias: target changed concurrently", role)
		}
		if err := os.Remove(alias); err != nil {
			return fmt.Errorf("remove failed %s alias: %w", role, err)
		}
	}
	if err := syncDirectory(outDir); err != nil {
		return fmt.Errorf("sync restored %s alias directory: %w", role, err)
	}
	return nil
}

func NewIngestSink(ing ingest.Ingester, logf func(string, ...any)) *IngestSink {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	closer, _ := ing.(io.Closer)
	return &IngestSink{ing: ing, closer: closer, logf: logf}
}

// Results returns the overlay and bundle ingest results (nil until the
// corresponding Absorb call succeeds). Used by the caller to report stats.
func (s *IngestSink) Results() (overlay, bundle *ingest.Result) {
	return s.overlayRes, s.bundleRes
}

func (s *IngestSink) SandboxResult() *ingest.Result { return s.sandboxRes }

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

func (s *IngestSink) AbsorbOverlaySource(ctx context.Context, source sparse.Source) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("overlay source is nil")
	}
	res, err := s.run(ctx, source, nil, "overlay dependency")
	if err != nil {
		return "", "", err
	}
	s.overlayRes = res
	return "manifest://" + HexKey(res.ManifestKey), "", nil
}

func (s *IngestSink) AbsorbSandbox(ctx context.Context, source sparse.Source) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("sandbox source is nil")
	}
	res, err := s.run(ctx, source, nil, "Sandbox E")
	if err != nil {
		return "", "", err
	}
	s.sandboxRes = res
	return "manifest://" + HexKey(res.ManifestKey), "", nil
}

func (s *IngestSink) AbsorbSnapshot(ctx context.Context, source sparse.Source) (string, string, error) {
	if source == nil {
		return "", "", fmt.Errorf("snapshot source is nil")
	}
	res, err := s.run(ctx, source, nil, "Snapshot S")
	if err != nil {
		return "", "", err
	}
	s.bundleRes = res
	return "manifest://" + HexKey(res.ManifestKey), "", nil
}

func (s *IngestSink) CommitSandbox(context.Context, string, string) error  { return nil }
func (s *IngestSink) CommitSnapshot(context.Context, string, string) error { return nil }

func (s *IngestSink) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.closer != nil {
			s.closeErr = s.closer.Close()
		}
	})
	return s.closeErr
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

func (s *seekerSource) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.size {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, fmt.Errorf("snapshot: seeker RunAt limit is zero")
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
	kind := sparse.Data
	if isHole {
		kind = sparse.Hole
	}
	return seekerRun{source: s, offset: offset, end: e, kind: kind}, nil
}

type seekerRun struct {
	source *seekerSource
	offset uint64
	end    uint64
	kind   sparse.RunKind
}

func (r seekerRun) Offset() uint64       { return r.offset }
func (r seekerRun) End() uint64          { return r.end }
func (r seekerRun) Kind() sparse.RunKind { return r.kind }

func (r seekerRun) ReadAt(ctx context.Context, buf []byte, innerOffset uint64) (int, error) {
	length := r.end - r.offset
	if innerOffset > length || uint64(len(buf)) > length-innerOffset {
		return 0, fmt.Errorf("snapshot: seeker Run read outside [0,%d)", length)
	}
	if len(buf) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.kind == sparse.Hole || r.kind == sparse.Zero {
		clear(buf)
		return len(buf), nil
	}
	n, err := r.source.ReadAt(ctx, buf, r.offset+innerOffset)
	if n == len(buf) && (err == nil || errors.Is(err, io.EOF)) {
		return n, nil
	}
	if err != nil {
		return n, err
	}
	return n, io.ErrUnexpectedEOF
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

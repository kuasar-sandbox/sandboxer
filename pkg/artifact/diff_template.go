package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/emptyext4"
	"golang.org/x/sys/unix"
)

// VerifiedManifestFetcher is the explicit capability required of an embedding
// caller's borrowed template Store. Open must validate the selected Manifest's
// physical ContentKey; every Chunk read must validate its physical ContentKey
// before decryption, irrespective of ordinary-read verification policy. The
// returned stream must stay bound to that selected source, without fallback to
// another Store/Bundle after a missing or corrupt object. Template completion
// consumes its full sparse content, including pages overwritten by the frontend.
type VerifiedManifestFetcher interface {
	OpenVerifiedManifest(context.Context, store.ContentKey) (fetch.Stream, error)
}

// OpenDiffTemplate opens one immutable logical disk initializer. The URI alone
// selects its format; generic OpenFile's detection and snapshot unpacking are
// deliberately not used. Call only after an existing writable disk has lost
// source selection. The caller owns the result, including its pending verifier.
// A nil ProcessStorage supports unencrypted local sources and mkfs.
func (s *ProcessStorage) OpenDiffTemplate(ctx context.Context, raw string, dir string, locations config.RefLocations) (diffstore.TemplateSource, error) {
	return s.openDiffTemplate(ctx, raw, dir, locations, openTemplateFile)
}

type templateFileOpener func(string) (*os.File, os.FileInfo, error)

func (s *ProcessStorage) openDiffTemplate(ctx context.Context, raw string, dir string, locations config.RefLocations, openFile templateFileOpener) (diffstore.TemplateSource, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ref, err := config.ParseDiffTemplate(raw)
	if err != nil {
		return nil, readerr.Mark(err, false)
	}
	if s == nil {
		s = &ProcessStorage{}
	}
	var path string
	if ref.Kind != config.DiffTemplateMkfs && ref.Kind != config.DiffTemplateManifest {
		requested, err := locations.ResolveFile(ref.Ref, dir)
		if err != nil {
			return nil, readerr.Mark(err, false)
		}
		// Resolve an alias once. Metadata-open retries keep the selected physical
		// path; a successfully opened source and its verifier pin the same fd.
		err = readretry.Do(ctx, func() error {
			var err error
			path, err = resolveFileTarget(requested, ref.Ref, locations)
			if err != nil {
				var pathError *os.PathError
				if !errors.As(err, &pathError) {
					// EvalSymlinks reports structural failures, such as cycles,
					// without a PathError. Actual filesystem failures retain one.
					err = readerr.Mark(err, false)
				}
			}
			return templateFileError(err)
		})
		if err != nil {
			return nil, err
		}
	}
	return readretry.Open(ctx, func() (diffstore.TemplateSource, error) {
		return s.openDiffTemplateRef(ctx, ref, path, openFile)
	})
}

func (s *ProcessStorage) openDiffTemplateRef(ctx context.Context, ref config.DiffTemplateRef, path string, openFile templateFileOpener) (diffstore.TemplateSource, error) {
	switch ref.Kind {
	case config.DiffTemplateMkfs:
		source, err := emptyext4.New(ref.Size)
		if err != nil {
			return nil, readerr.Mark(err, false)
		}
		return source, nil
	case config.DiffTemplateManifest:
		return s.openManifestTemplate(ctx, ref)
	case config.DiffTemplateRaw:
		return openRawTemplate(path, openFile)
	case config.DiffTemplateTarstream:
		codec, required, err := s.readCodec(ref.Ref.Location != "")
		if err != nil {
			return nil, readerr.Mark(err, false)
		}
		options, err := tarReadOptions(ref.Ref, codec, required)
		if err != nil {
			return nil, protectProcessLocalReadError(codec, "open diff template", err)
		}
		source, err := openTarTemplate(path, ref, codec, options, openFile)
		return source, protectProcessLocalReadError(codec, "open diff template", err)
	case config.DiffTemplateBundle:
		return s.openBundleTemplate(ctx, path, ref, openFile)
	default:
		return nil, readerr.Mark(fmt.Errorf("unsupported diff template kind %q", ref.Kind), false)
	}
}

func (s *ProcessStorage) openBundleTemplate(ctx context.Context, path string, ref config.DiffTemplateRef, openFile templateFileOpener) (diffstore.TemplateSource, error) {
	if s.cfg == nil || s.keyFn == nil {
		return nil, readerr.Mark(fmt.Errorf("diff template Bundle requires manifest configuration and customer key"), false)
	}
	file, info, err := openFile(path)
	if err != nil {
		return nil, err
	}
	reader, err := bundle.NewReader(file, info.Size())
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	fail := func(err error) (diffstore.TemplateSource, error) {
		return nil, errors.Join(err, reader.Close(), file.Close())
	}
	root, err := manifest.ParseHexKey(ref.Ref.Digest)
	if err != nil {
		return fail(readerr.Mark(err, false))
	}
	if !reader.HasManifest(root) {
		return fail(readerr.Mark(fmt.Errorf("diff template Bundle root %s is absent", manifest.HexKey(root)), false))
	}
	key, err := s.keyFn()
	if err != nil {
		return fail(readerr.Mark(fmt.Errorf("diff template Bundle customer key: %w", err), false))
	}
	defer clear(key[:])
	_, decryptor, err := manifestcrypto.New(s.cfg.Crypto)
	if err != nil {
		return fail(readerr.Mark(err, false))
	}
	// The explicit root and its entire Chunk closure belong to this pinned
	// Bundle only. Neither bundle/refs nor a Store can supply missing content.
	local := fetch.NewFetcherWithOptions(key, reader.Getter(), decryptor, fetch.Options{VerifyContent: true})
	selected := bundle.NewManifestFetcher(reader, local, nil)
	stream, err := selected.OpenRootManifest(ctx, root)
	if err != nil {
		return fail(err)
	}
	opened := &OpenedFile{Stream: stream, format: FileFormatManifestBundle, resolvedPath: path,
		scopedFetcher: selected, manifestSet: selected, bundleReader: reader, rootKey: root}
	return &verifiedTemplateSource{TemplateSource: &bundleTemplateSource{OpenedFile: opened, file: file}}, nil
}

type bundleTemplateSource struct {
	*OpenedFile
	file *os.File
	once sync.Once
	err  error
}

func (s *bundleTemplateSource) Close() error {
	s.once.Do(func() { s.err = errors.Join(s.OpenedFile.Close(), s.file.Close()) })
	return s.err
}

func (s *ProcessStorage) openManifestTemplate(ctx context.Context, ref config.DiffTemplateRef) (diffstore.TemplateSource, error) {
	selected := s.templateFetcher
	// Preserve package-level embeddings that construct ProcessStorage directly.
	if selected == nil && s.borrowedFetcher == nil && s.fetcher != nil {
		selected = s.fetcher
	}
	verified, ok := selected.(VerifiedManifestFetcher)
	if !ok || verified == nil {
		return nil, readerr.Mark(fmt.Errorf("manifest diff template requires a VerifiedManifestFetcher capability"), false)
	}
	key, err := manifest.ParseHexKey(ref.Ref.Path)
	if err != nil {
		return nil, readerr.Mark(err, false)
	}
	source, err := verified.OpenVerifiedManifest(ctx, key)
	if err != nil {
		if source != nil {
			err = errors.Join(err, source.Close())
		}
		return nil, err
	}
	if source == nil {
		return nil, readerr.Mark(fmt.Errorf("manifest diff template returned a nil stream"), false)
	}
	return &verifiedTemplateSource{TemplateSource: source}, nil
}

func templateFileError(err error) error {
	for _, cause := range []error{os.ErrNotExist, os.ErrPermission, unix.ENOTDIR, unix.ELOOP, unix.ENAMETOOLONG, unix.EINVAL} {
		if errors.Is(err, cause) {
			return readerr.Mark(err, false)
		}
	}
	return err
}

func openTemplateFile(path string) (*os.File, os.FileInfo, error) {
	// A replaced alias or an explicitly supplied FIFO must fail at fstat,
	// without blocking before we can reject a non-regular carrier.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if info, statErr := os.Lstat(path); statErr == nil && !info.Mode().IsRegular() {
			err = readerr.Mark(fmt.Errorf("diff template must be a nonempty regular file: %w", err), false)
		}
		return nil, nil, templateFileError(err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Size() <= 0) {
		err = readerr.Mark(fmt.Errorf("diff template must be a nonempty regular file"), false)
	}
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	return file, info, nil
}

func openRawTemplate(path string, openFile templateFileOpener) (diffstore.TemplateSource, error) {
	file, info, err := openFile(path)
	if err != nil {
		return nil, err
	}
	// These are active-D/legacy-envelope bytes, not a raw logical disk.
	// Reject their reserved magic without decoding or inferring another
	// template format. Bare Bundle/tarstream files still remain raw bytes.
	var prefix [8]byte
	if n, err := file.ReadAt(prefix[:], 0); err != nil && err != io.EOF {
		return nil, errors.Join(err, file.Close())
	} else if n == len(prefix) && bytes.Equal(prefix[:], []byte{0x89, 'K', 'D', 'X', 'T', 'S', '1', '\n'}) {
		return nil, errors.Join(readerr.Mark(fmt.Errorf("raw diff_template no longer accepts DIFF working files or v1 envelopes; export the logical disk first"), false), file.Close())
	}
	holes, err := sparse.ProbeHoles(file)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	source, err := sparse.NewSource(file, uint64(info.Size()), holes)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &rawTemplateSource{Source: source, file: file}, nil
}

type rawTemplateSource struct {
	sparse.Source
	file *os.File
	once sync.Once
	err  error
}

func (s *rawTemplateSource) Close() error {
	s.once.Do(func() { s.err = s.file.Close() })
	return s.err
}

type tarTemplateSource struct {
	sparse.Source
	file         *os.File
	physicalSize int64
	payload      string
	options      []tarstream.ReadOption
	codec        tarstream.Codec
	closeOnce    sync.Once
	closeErr     error
	verifyOnce   sync.Once
	verifyErr    error
}

func openTarTemplate(path string, ref config.DiffTemplateRef, codec tarstream.Codec, options []tarstream.ReadOption, openFile templateFileOpener) (diffstore.TemplateSource, error) {
	file, info, err := openFile(path)
	if err != nil {
		return nil, err
	}
	source, payload, err := tarstream.SourceAt(file, info.Size(), "", options...)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	result := &tarTemplateSource{Source: source, file: file, physicalSize: info.Size(), payload: payload, options: options, codec: codec}
	if err := validateTarIdentity(ref.Ref, path, source); err != nil {
		return nil, errors.Join(err, result.Close())
	}
	return result, nil
}

func (s *tarTemplateSource) Close() error {
	s.closeOnce.Do(func() {
		if closer, ok := s.Source.(io.Closer); ok {
			s.closeErr = closer.Close()
		}
		s.closeErr = errors.Join(s.closeErr, s.file.Close())
	})
	return s.closeErr
}

func (s *tarTemplateSource) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.verifyOnce.Do(func() { s.verifyErr = s.verify(ctx, s.file) })
	return protectProcessLocalReadError(s.codec, "verify diff template", s.verifyErr)
}

func (s *tarTemplateSource) verify(ctx context.Context, carrier io.ReaderAt) (retErr error) {
	// The immutable carrier is pinned by the already-open descriptor. An
	// independent SectionReader never changes SourceAt's file position and
	// never follows a path or symlink again while frontend reads are active.
	reader := io.NewSectionReader(templateRetryReaderAt{ctx: ctx, ReaderAt: carrier}, 0, s.physicalSize)
	source, payload, err := tarstream.SourceFrom(reader, s.payload, s.options...)
	if err != nil {
		return err
	}
	if closer, ok := source.(io.Closer); ok {
		defer func() { retErr = errors.Join(retErr, closer.Close()) }()
	}
	if payload != s.payload || source.Size() != s.Size() {
		return readerr.Mark(fmt.Errorf("diff template payload or capacity changed during verification"), false)
	}
	return consumeLocationSource(ctx, source)
}

// Retry physical ranges before a sequential parser observes any bytes. A
// partial failed read must not advance SourceFrom and then retry backwards.
type templateRetryReaderAt struct {
	ctx context.Context
	io.ReaderAt
}

func (r templateRetryReaderAt) ReadAt(p []byte, off int64) (int, error) {
	return readretry.ReadAt(r.ctx, len(p), func() (int, error) { return r.ReaderAt.ReadAt(p, off) })
}

type verifiedTemplateSource struct {
	diffstore.TemplateSource
	once sync.Once
	err  error
}

func (s *verifiedTemplateSource) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.once.Do(func() { s.err = consumeLocationSource(ctx, s.TemplateSource) })
	return s.err
}

package artifact

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// FileFormat identifies the physical format selected by the file magic.
type FileFormat uint8

const (
	FileFormatTarstream FileFormat = iota
	FileFormatManifestBundle
)

var manifestBundleMagic = [4]byte{'P', 'K', 0x03, 0x04}

// OpenedFile is one immutable file:// artifact. For a Manifest Bundle, Stream
// is the selected root Manifest plaintext and ScopedFetcher routes each later
// Manifest as an all-local Bundle layer or an all-remote layer. Close keeps the
// Bundle mmap alive until the selected Stream is released.
type OpenedFile struct {
	fetch.Stream

	format        FileFormat
	scopedFetcher fetch.Fetcher
	manifestSet   *bundle.ManifestFetcher
	resolver      *bundleSourceResolver
	bundleReader  *bundle.Reader
	rootKey       store.ContentKey
	digestScheme  string
	digest        string

	closeOnce sync.Once
	closeErr  error
}

func (f *OpenedFile) Format() FileFormat { return f.format }

func (f *OpenedFile) ScopedFetcher() fetch.Fetcher { return f.scopedFetcher }

// ManifestFetcher exposes Manifest-level source selection for callers that
// must plan a new Bundle or exact Store upload. It is nil for tarstreams.
func (f *OpenedFile) ManifestFetcher() *bundle.ManifestFetcher { return f.manifestSet }

func (f *OpenedFile) BundleReader() *bundle.Reader { return f.bundleReader }

func (f *OpenedFile) RootManifestKey() (store.ContentKey, bool) {
	return f.rootKey, f.format == FileFormatManifestBundle
}

// Digest implements tarstream.Digester for tarstream callers. Bundle callers
// select identity with RootManifestKey instead.
func (f *OpenedFile) Digest() (string, string) { return f.digestScheme, f.digest }

func (f *OpenedFile) TarStreamDigest(name string) ([32]byte, bool) {
	provider, ok := f.Stream.(tarstream.IdentityProvider)
	if !ok {
		return [32]byte{}, false
	}
	return provider.TarStreamDigest(name)
}

func (f *OpenedFile) PayloadCommitment() (uint64, [32]byte, bool) {
	provider, ok := f.Stream.(tarstream.IdentityProvider)
	if !ok {
		return f.Size(), [32]byte{}, false
	}
	return provider.PayloadCommitment()
}

func (f *OpenedFile) Close() error {
	f.closeOnce.Do(func() {
		if f.Stream != nil {
			f.closeErr = f.Stream.Close()
		}
		if f.resolver != nil {
			f.closeErr = errors.Join(f.closeErr, f.resolver.Close())
		}
		if f.bundleReader != nil {
			f.closeErr = errors.Join(f.closeErr, f.bundleReader.Close())
		}
	})
	return f.closeErr
}

// OpenFile detects encrypted tarstream, Manifest Bundle, or plaintext
// tarstream from content. A ZIP local-header magic commits to strict Bundle
// parsing; malformed Bundles never fall back to tarstream.
func OpenFile(
	ctx context.Context,
	path string,
	ref manifest.Ref,
	manifestCfg *config.ManifestConfig,
	keyFn ingest.CustomerKeyFunc,
	remote fetch.Fetcher,
	localCodec tarstream.Codec,
	localRequired bool,
) (*OpenedFile, error) {
	return OpenFileWithLocations(ctx, path, ref, manifestCfg, keyFn, remote, nil, localCodec, localRequired)
}

// OpenFileWithLocations is OpenFile with the trusted location map required by
// ordered Bundle refs. Tarstream behavior is unchanged.
func OpenFileWithLocations(
	ctx context.Context,
	path string,
	ref manifest.Ref,
	manifestCfg *config.ManifestConfig,
	keyFn ingest.CustomerKeyFunc,
	remote fetch.Fetcher,
	locations config.RefLocations,
	localCodec tarstream.Codec,
	localRequired bool,
) (*OpenedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref.Scheme == "" {
		ref = manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path}
	}
	if ref.Scheme != manifest.RefSchemeFile {
		return nil, fmt.Errorf("artifact: file opener requires file:// ref")
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	path, err := resolveLocatedFileTarget(path, ref, locations)
	if err != nil {
		return nil, err
	}

	format, err := DetectFileFormat(path)
	if err != nil {
		return nil, err
	}
	if format == FileFormatManifestBundle {
		return openManifestBundle(ctx, path, ref, manifestCfg, keyFn, remote, locations)
	}
	return openTarstream(path, ref, localCodec, localRequired)
}

// validateLocatedFileTarget confines a named ref-location alias to the same
// trusted directory as its resolved basename. Semantic aliases may point at a
// sibling content-addressed file, but cannot escape the named location through
// a symlink. Unlocated paths are explicit host input and retain existing
// filesystem semantics.
func resolveLocatedFileTarget(path string, ref manifest.Ref, locations config.RefLocations) (string, error) {
	if ref.Location == "" {
		return path, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("artifact: resolve located path: %w", err)
	}
	expected, err := locations.ResolveFile(ref, "")
	if err != nil {
		return "", err
	}
	expected, err = filepath.Abs(expected)
	if err != nil {
		return "", fmt.Errorf("artifact: resolve ref-location path: %w", err)
	}
	if filepath.Clean(absolute) != filepath.Clean(expected) {
		return "", fmt.Errorf("artifact: located ref path does not match ref-location %q", ref.Location)
	}
	realDirectory, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("artifact: resolve ref-location directory: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("artifact: resolve located artifact: %w", err)
	}
	if filepath.Clean(filepath.Dir(realTarget)) != filepath.Clean(realDirectory) {
		return "", fmt.Errorf("artifact: located alias target escapes ref-location %q", ref.Location)
	}
	return realTarget, nil
}

// OpenFile uses the process-fixed customer key and lazy remote Fetcher.
func (s *ProcessStorage) OpenFile(ctx context.Context, path string, ref manifest.Ref) (*OpenedFile, error) {
	if s == nil {
		return nil, fmt.Errorf("artifact: process storage is required")
	}
	opened, err := OpenFile(ctx, path, ref, s.cfg, s.keyFn, s.Fetcher(), s.localCodec, s.localRequired)
	return opened, protectProcessLocalReadError(s.localCodec, "open local artifact", err)
}

// OpenFileWithLocations supplies ordered Bundle refs with trusted named
// location resolution while retaining ProcessStorage ownership.
func (s *ProcessStorage) OpenFileWithLocations(ctx context.Context, path string, ref manifest.Ref, locations config.RefLocations) (*OpenedFile, error) {
	if s == nil {
		return nil, fmt.Errorf("artifact: process storage is required")
	}
	opened, err := OpenFileWithLocations(ctx, path, ref, s.cfg, s.keyFn, s.Fetcher(), locations, s.localCodec, s.localRequired)
	return opened, protectProcessLocalReadError(s.localCodec, "open local artifact", err)
}

type processLocalReadError struct {
	op  string
	err error
}

func (e *processLocalReadError) Error() string { return e.op + " failed" }
func (e *processLocalReadError) Unwrap() error { return e.err }

// protectProcessLocalReadError preserves errors.Is while preventing paths,
// content identities, and crypto diagnostics from reaching CLI output when a
// local codec is active.
func protectProcessLocalReadError(codec tarstream.Codec, op string, err error) error {
	if codec == nil || err == nil {
		return err
	}
	return &processLocalReadError{op: op, err: err}
}

// DetectFileFormat performs the non-consuming magic check used by snapshot,
// restore, info, merge, and upload paths. Non-ZIP input is delegated to the
// tarstream parser, which distinguishes encrypted and plaintext envelopes.
func DetectFileFormat(path string) (FileFormat, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("artifact: open %s: %w", path, err)
	}
	defer f.Close()
	var magic [4]byte
	n, readErr := io.ReadFull(f, magic[:])
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return 0, fmt.Errorf("artifact: read %s magic: %w", path, readErr)
	}
	if n == len(magic) && magic == manifestBundleMagic {
		return FileFormatManifestBundle, nil
	}
	return FileFormatTarstream, nil
}

func openManifestBundle(ctx context.Context, path string, ref manifest.Ref, cfg *config.ManifestConfig, keyFn ingest.CustomerKeyFunc, remote fetch.Fetcher, locations config.RefLocations) (*OpenedFile, error) {
	if ref.DigestScheme != "" && ref.DigestScheme != "manifest" {
		return nil, fmt.Errorf("manifest Bundle rejects @%s identity", ref.DigestScheme)
	}
	if cfg == nil || keyFn == nil {
		return nil, fmt.Errorf("manifest Bundle requires manifest configuration and customer key")
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("manifest Bundle resolve final path: %w", err)
	}
	resolvedPath, err = filepath.Abs(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("manifest Bundle resolve final path: %w", err)
	}
	reader, err := bundle.Open(resolvedPath)
	if err != nil {
		return nil, err
	}
	var resolver *bundleSourceResolver
	fail := func(err error) (*OpenedFile, error) {
		if resolver != nil {
			_ = resolver.Close()
		}
		_ = reader.Close()
		return nil, err
	}
	root, err := BundleRootKey(resolvedPath, ref)
	if err != nil {
		return fail(err)
	}
	if !reader.HasManifest(root) {
		return fail(fmt.Errorf("manifest Bundle root %s is absent", manifest.HexKey(root)))
	}
	customerKey, err := keyFn()
	if err != nil {
		return fail(fmt.Errorf("manifest Bundle customer key: %w", err))
	}
	_, decryptor, err := manifestcrypto.New(cfg.Crypto)
	if err != nil {
		clear(customerKey[:])
		return fail(err)
	}
	if len(reader.Refs()) != 0 {
		resolver = newBundleSourceResolver(filepath.Dir(resolvedPath), locations, customerKey, decryptor, verificationOptions(cfg))
	}
	local := fetch.NewFetcherWithOptions(customerKey, reader.Getter(), decryptor, verificationOptions(cfg))
	clear(customerKey[:])
	scoped := bundle.NewManifestFetcherWithResolver(reader, local, resolver, remote)
	stream, err := scoped.OpenRootManifest(ctx, root)
	if err != nil {
		return fail(fmt.Errorf("open manifest Bundle root %s: %w", manifest.HexKey(root), err))
	}
	return &OpenedFile{
		Stream: stream, format: FileFormatManifestBundle, scopedFetcher: scoped, manifestSet: scoped,
		resolver: resolver, bundleReader: reader, rootKey: root,
	}, nil
}

// BundleRootKey resolves an explicit @manifest selector or infers the root
// from the symlink-resolved <64hex>.bundle filename.
func BundleRootKey(path string, ref manifest.Ref) (store.ContentKey, error) {
	if ref.DigestScheme == "manifest" {
		key, err := manifest.ParseHexKey(ref.Digest)
		if err != nil {
			return key, fmt.Errorf("manifest Bundle selector: %w", err)
		}
		return key, nil
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return store.ContentKey{}, fmt.Errorf("manifest Bundle resolve final filename: %w", err)
	}
	base := filepath.Base(realPath)
	if filepath.Ext(base) != ".bundle" {
		return store.ContentKey{}, fmt.Errorf("manifest Bundle without @manifest selector requires <64hex>.bundle final filename")
	}
	key, err := manifest.ParseHexKey(strings.TrimSuffix(base, ".bundle"))
	if err != nil {
		return key, fmt.Errorf("manifest Bundle root from final filename: %w", err)
	}
	return key, nil
}

func openTarstream(path string, ref manifest.Ref, codec tarstream.Codec, required bool) (*OpenedFile, error) {
	if ref.DigestScheme == "manifest" {
		return nil, fmt.Errorf("tarstream rejects @manifest selector")
	}
	options, err := tarReadOptions(ref, codec, required)
	if err != nil {
		return nil, err
	}
	stream, err := fetch.OpenTarStream(path, options...)
	if err != nil {
		return nil, err
	}
	if err := validateTarIdentity(ref, path, stream); err != nil {
		_ = stream.Close()
		return nil, err
	}
	digester, ok := stream.(tarstream.Digester)
	if !ok {
		_ = stream.Close()
		return nil, fmt.Errorf("local tarstream has no declared digest")
	}
	scheme, digest := digester.Digest()
	return &OpenedFile{Stream: stream, format: FileFormatTarstream, digestScheme: scheme, digest: digest}, nil
}

func tarReadOptions(ref manifest.Ref, codec tarstream.Codec, required bool) ([]tarstream.ReadOption, error) {
	if required && codec == nil {
		return nil, fmt.Errorf("local tarstream: required policy has no codec")
	}
	var options []tarstream.ReadOption
	if codec != nil {
		options = append(options, tarstream.WithCodec(codec, required))
	}
	if ref.Digest != "" {
		options = append(options, tarstream.WithExpectedDigest(ref.DigestScheme, ref.Digest))
	}
	return options, nil
}

func validateTarIdentity(ref manifest.Ref, path string, stream fetch.Stream) error {
	digester, ok := stream.(tarstream.Digester)
	if !ok {
		return fmt.Errorf("local tarstream: artifact has no declared digest")
	}
	scheme, digest := digester.Digest()
	if scheme != tarstream.DigestScheme && scheme != tarstream.DigestSchemeHMAC {
		return fmt.Errorf("local tarstream: artifact digest scheme is incompatible with policy")
	}
	if ref.Digest != "" {
		return nil
	}
	if ref.Location == "" {
		return nil
	}
	realPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		realPath = resolved
	}
	base := filepath.Base(realPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if digestEqual(stem, digest) {
		return nil
	}
	return fmt.Errorf("local tarstream: content name does not match artifact identity")
}

func digestEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

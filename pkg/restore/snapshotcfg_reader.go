package restore

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
)

// MaxSnapshotCfgSize is the largest uncompressed snapshot.cfg accepted by the
// single-root reader.
const MaxSnapshotCfgSize = 1 << 20

// SnapshotCfgReadOptions supplies trusted path resolution inputs. RefLocations
// is a reader path map (name -> absolute path), not a CLI file:// URI map.
// RelativeDir is used only for unlocated relative file refs and raw relative
// paths.
type SnapshotCfgReadOptions struct {
	RefLocations config.RefLocations
	RelativeDir  string
}

// SnapshotCfgDocument is the canonical parsed root config together with its
// original YAML body. Raw is retained for sandbox-ctl info's human-readable
// output; task callers should consume Config.
type SnapshotCfgDocument struct {
	Config *SnapshotCfg
	Raw    []byte
}

type snapshotCfgStorage interface {
	Fetcher() fetch.Fetcher
	LocalCodec() tarstream.Codec
	LocalRequired() bool
	Close() error
}

type snapshotCfgFileOpener interface {
	OpenFile(context.Context, string, manifest.Ref) (*artifact.OpenedFile, error)
}

type snapshotCfgLocationFileOpener interface {
	OpenFileWithLocations(context.Context, string, manifest.Ref, config.RefLocations) (*artifact.OpenedFile, error)
}

// SnapshotCfgReader reads exactly one root snapshot bundle. It does not walk
// FromRefs, apply conductor policy, or cache results across tasks.
type SnapshotCfgReader struct {
	storage snapshotCfgStorage
}

// NewSnapshotCfgReader creates a process-local reader using the existing
// MANIFEST_KEY convention. The returned reader owns its lazy manifest client
// and must be closed before a caller replaces its process via exec.
func NewSnapshotCfgReader(manifestCfg *config.ManifestConfig) (*SnapshotCfgReader, error) {
	storage, err := artifact.NewProcessStorage(manifestCfg)
	if err != nil {
		return nil, err
	}
	return newSnapshotCfgReader(storage), nil
}

func newSnapshotCfgReader(storage snapshotCfgStorage) *SnapshotCfgReader {
	return &SnapshotCfgReader{storage: storage}
}

// Close releases any cache or store client opened while reading the root.
func (r *SnapshotCfgReader) Close() error {
	if r == nil || r.storage == nil {
		return nil
	}
	return r.storage.Close()
}

// Read opens rootRef, extracts exactly one snapshot.cfg ZIP entry under the
// configured size limit, and returns its canonical parsed representation.
func (r *SnapshotCfgReader) Read(ctx context.Context, rootRef string, opts SnapshotCfgReadOptions) (*SnapshotCfgDocument, error) {
	body, err := r.ReadRaw(ctx, rootRef, opts)
	if err != nil {
		return nil, err
	}
	cfg, err := ParseSnapshotCfg(body)
	if err != nil {
		return nil, err
	}
	return &SnapshotCfgDocument{Config: cfg, Raw: body}, nil
}

// ReadRaw performs the same bounded, exact-entry read as Read without parsing
// the YAML. It exists for sandbox-ctl info's default diagnostic output; task
// callers should use Read so malformed root configs fail preparation.
func (r *SnapshotCfgReader) ReadRaw(ctx context.Context, rootRef string, opts SnapshotCfgReadOptions) ([]byte, error) {
	if r == nil || r.storage == nil {
		return nil, errors.New("snapshot.cfg reader is not initialized")
	}
	if rootRef == "" {
		return nil, errors.New("snapshot root ref is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stream, size, err := r.openRoot(ctx, rootRef, opts)
	if err != nil {
		return nil, err
	}
	body, readErr := readSnapshotCfgEntry(fetch.NewReaderAt(ctx, stream), size)
	closeErr := stream.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, errors.Join(readErr, fmt.Errorf("close snapshot stream: %w", closeErr))
		}
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close snapshot stream: %w", closeErr)
	}
	return body, nil
}

func (r *SnapshotCfgReader) openRoot(ctx context.Context, rootRef string, opts SnapshotCfgReadOptions) (fetch.Stream, int64, error) {
	codec := r.storage.LocalCodec()
	required := r.storage.LocalRequired()
	if strings.HasPrefix(rootRef, "manifest://") || strings.HasPrefix(rootRef, "file://") {
		ref, err := manifest.ParseRef(rootRef)
		if err != nil {
			return nil, 0, err
		}
		if ref.Scheme == manifest.RefSchemeFile {
			path, err := opts.RefLocations.ResolveFile(ref, opts.RelativeDir)
			if err != nil {
				return nil, 0, err
			}
			if opened, handled, err := openSnapshotCfgFile(ctx, r.storage, path, ref, opts.RefLocations); handled {
				if err != nil {
					return nil, 0, protectArtifactReadError(codec, "open local snapshot", err)
				}
				if opened.Size() > math.MaxInt64 {
					closeErr := opened.Close()
					return nil, 0, errors.Join(fmt.Errorf("snapshot bundle is too large"), closeErr)
				}
				return opened, int64(opened.Size()), nil
			}
		}
		return sandbox.OpenDiskStreamAt(ctx, rootRef, r.storage.Fetcher(), opts.RefLocations, opts.RelativeDir, codec, required)
	}

	path := rootRef
	if !filepath.IsAbs(path) && opts.RelativeDir != "" {
		path = filepath.Join(opts.RelativeDir, path)
	}
	if opened, handled, err := openSnapshotCfgFile(ctx, r.storage, path, manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path}, opts.RefLocations); handled {
		if err != nil {
			return nil, 0, protectArtifactReadError(codec, "open local snapshot", err)
		}
		if opened.Size() > math.MaxInt64 {
			_ = opened.Close()
			return nil, 0, fmt.Errorf("snapshot bundle is too large")
		}
		return opened, int64(opened.Size()), nil
	}
	options, err := tarReadOptions(manifest.Ref{}, codec, required)
	if err != nil {
		return nil, 0, err
	}
	stream, err := fetch.OpenTarStream(path, options...)
	if err != nil {
		return nil, 0, protectArtifactReadError(codec, "open local snapshot", err)
	}
	if stream.Size() > math.MaxInt64 {
		_ = stream.Close()
		return nil, 0, fmt.Errorf("snapshot bundle is too large")
	}
	return stream, int64(stream.Size()), nil
}

func openSnapshotCfgFile(ctx context.Context, storage snapshotCfgStorage, path string, ref manifest.Ref, locations config.RefLocations) (*artifact.OpenedFile, bool, error) {
	if opener, ok := storage.(snapshotCfgLocationFileOpener); ok {
		opened, err := opener.OpenFileWithLocations(ctx, path, ref, locations)
		return opened, true, err
	}
	if opener, ok := storage.(snapshotCfgFileOpener); ok {
		opened, err := opener.OpenFile(ctx, path, ref)
		return opened, true, err
	}
	return nil, false, nil
}

func readSnapshotCfgEntry(reader io.ReaderAt, size int64) ([]byte, error) {
	if size < 0 {
		return nil, fmt.Errorf("read snapshot zip: negative bundle size")
	}
	zr, err := zip.NewReader(reader, size)
	if err != nil {
		return nil, fmt.Errorf("read snapshot zip: %w", err)
	}
	var entry *zip.File
	for _, candidate := range zr.File {
		if candidate.Name != "snapshot.cfg" {
			continue
		}
		if entry != nil {
			return nil, fmt.Errorf("snapshot bundle has duplicate snapshot.cfg entries")
		}
		entry = candidate
	}
	if entry == nil {
		return nil, fmt.Errorf("input has no snapshot.cfg entry (not a snapshot image?)")
	}
	if entry.UncompressedSize64 > MaxSnapshotCfgSize {
		return nil, fmt.Errorf("snapshot.cfg exceeds %d-byte limit", MaxSnapshotCfgSize)
	}
	rc, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open snapshot.cfg: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(rc, MaxSnapshotCfgSize+1))
	closeErr := rc.Close()
	if len(body) > MaxSnapshotCfgSize {
		return nil, fmt.Errorf("snapshot.cfg exceeds %d-byte limit", MaxSnapshotCfgSize)
	}
	if readErr != nil {
		readErr = fmt.Errorf("read snapshot.cfg: %w", readErr)
		if closeErr != nil {
			return nil, errors.Join(readErr, fmt.Errorf("close snapshot.cfg: %w", closeErr))
		}
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close snapshot.cfg: %w", closeErr)
	}
	return body, nil
}

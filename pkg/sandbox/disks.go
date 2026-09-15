package sandbox

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

// FileStreamOpener opens one already-resolved file:// artifact. Callers that
// have manifest configuration use it to share the tarstream/Bundle content
// detector; nil selects the ordinary local tarstream path.
type FileStreamOpener func(ctx context.Context, path string, ref manifest.Ref) (fetch.Stream, error)

// OpenBlockReader resolves a file:// or manifest:// disk URI into a
// vhost.BlockReader plus the total disk size, via a fetch.Stream. file://
// opens a local tarstream by default or uses the supplied unified opener;
// manifest:// resolves one manifest through the fetcher. Both wrap in a
// StreamReader whose Close releases the stream (the file's fd; a manifest
// stream's cache/store client is owned by the Fetcher and closed separately).
//
// For manifest:// the fetcher must be non-nil — it carries the cache-ctl /
// store-ctl client and the per-process decryptor. Cold-start lifecycle and
// restore share one process-owned fetcher across every disk URI; its network
// clients are created only when a manifest is actually opened.
//
// ctx scopes asynchronous chunk fetches kicked off by later ReadAt calls;
// cancelling it makes pending vhost-user-blk reads fail promptly at shutdown.
func OpenBlockReader(ctx context.Context, uri string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, required bool) (vhost.BlockReader, int64, error) {
	return OpenBlockReaderWithOpener(ctx, uri, fetcher, locations, codec, required, nil)
}

// OpenBlockReaderWithOpener is OpenBlockReader with unified file:// format
// detection supplied by the process artifact owner.
func OpenBlockReaderWithOpener(ctx context.Context, uri string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, required bool, opener FileStreamOpener) (vhost.BlockReader, int64, error) {
	stream, size, err := OpenDiskStreamAtWithOpener(ctx, uri, fetcher, locations, "", codec, required, opener)
	if err != nil {
		return nil, 0, err
	}
	return vhost.NewStreamReader(ctx, stream, size), size, nil
}

// OpenRootImageBlockReaderWithOpener opens the read-only root EROFS image and
// always removes its configuration ZIP from the block-visible view. A parent
// .sandbox has already been narrowed by OpenDiskStreamAtWithOpener; a normal
// container image is then narrowed by the strict flattened-image reader.
// ImageConfigBytes remains available to LoadImageConfigFrom in both cases.
func OpenRootImageBlockReaderWithOpener(ctx context.Context, uri string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, required bool, opener FileStreamOpener) (vhost.BlockReader, int64, error) {
	stream, size, err := OpenDiskStreamAtWithOpener(ctx, uri, fetcher, locations, "", codec, required, opener)
	if err != nil {
		return nil, 0, err
	}
	if provider, ok := stream.(interface{ ImageConfigBytes() []byte }); ok && provider.ImageConfigBytes() != nil {
		return vhost.NewStreamReader(ctx, stream, size), size, nil
	}
	image, err := sandboxfile.OpenEROFSArtifact(ctx, stream)
	if err != nil {
		return nil, 0, fmt.Errorf("root container image: %w", err)
	}
	if image.Payload.Size() > math.MaxInt64 {
		closeErr := image.Close()
		return nil, 0, errors.Join(errors.New("root container image is too large"), closeErr)
	}
	size = int64(image.Payload.Size())
	return vhost.NewStreamReader(ctx, image.Payload, size), size, nil
}

// OpenDiskStream resolves a file:// or manifest:// disk URI into a fetch.Stream
// and its size. Exported so callers outside this package (notably
// pkg/restore) can share the same code path. The caller owns the
// returned stream and must Close it (directly or via a StreamReader).
func OpenDiskStream(ctx context.Context, uri string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, required bool) (fetch.Stream, int64, error) {
	return OpenDiskStreamAt(ctx, uri, fetcher, locations, "", codec, required)
}

// OpenDiskStreamAt is OpenDiskStream with an explicit base directory for an
// unlocated relative file ref. Located and absolute file refs ignore
// relativeDir. Callers that prepare task-local refs should pass the directory
// established by their bootstrap rather than relying on the process working
// directory.
func OpenDiskStreamAt(ctx context.Context, uri string, fetcher fetch.Fetcher, locations config.RefLocations, relativeDir string, codec tarstream.Codec, required bool) (fetch.Stream, int64, error) {
	return OpenDiskStreamAtWithOpener(ctx, uri, fetcher, locations, relativeDir, codec, required, nil)
}

// OpenDiskStreamAtWithOpener is OpenDiskStreamAt with a caller-owned unified
// file opener. Manifest selection and Chunk source isolation stay inside that
// opener; this function only resolves the trusted file location.
func OpenDiskStreamAtWithOpener(ctx context.Context, uri string, fetcher fetch.Fetcher, locations config.RefLocations, relativeDir string, codec tarstream.Codec, required bool, opener FileStreamOpener) (fetch.Stream, int64, error) {
	ref, err := manifest.ParseRef(uri)
	if err != nil {
		return nil, 0, protectLocalArtifactError(codec, "parse local artifact ref", err)
	}
	switch ref.Scheme {
	case manifest.RefSchemeFile:
		// Local tarstream hole maps and Bundle Manifest sparse maps both come
		// from their logical envelopes, never from the outer filesystem.
		path, err := locations.ResolveFile(ref, relativeDir)
		if err != nil {
			return nil, 0, protectLocalArtifactError(codec, "resolve local artifact ref", err)
		}
		if opener != nil {
			stream, err := opener(ctx, path, ref)
			if err != nil {
				return nil, 0, protectLocalArtifactError(codec, "open local artifact", err)
			}
			return narrowDiskPayload(ctx, stream, filepath.Ext(ref.Path) == ".sandbox", codec)
		}
		stream, _, err := openLocalDiskStream(path, ref, codec, required)
		if err != nil {
			return nil, 0, err
		}
		return narrowDiskPayload(ctx, stream, filepath.Ext(ref.Path) == ".sandbox", codec)
	case manifest.RefSchemeManifest:
		stream, _, err := OpenManifestStream(ctx, ref.Path, fetcher)
		if err != nil {
			return nil, 0, err
		}
		return narrowDiskPayload(ctx, stream, false, nil)
	default:
		return nil, 0, fmt.Errorf("unknown disk URI scheme: %s", ref.Scheme)
	}
}

func narrowDiskPayload(ctx context.Context, stream fetch.Stream, requireSandbox bool, codec tarstream.Codec) (fetch.Stream, int64, error) {
	payload, _, err := sandboxfile.PayloadIfSandbox(ctx, stream, requireSandbox)
	if err != nil {
		return nil, 0, protectLocalArtifactError(codec, "open Sandbox payload", err)
	}
	if payload.Size() > math.MaxInt64 {
		closeErr := payload.Close()
		return nil, 0, errors.Join(fmt.Errorf("artifact is too large"), closeErr)
	}
	return payload, int64(payload.Size()), nil
}

func openLocalDiskStream(path string, ref manifest.Ref, codec tarstream.Codec, required bool) (fetch.Stream, int64, error) {
	options, err := fileReadOptions(ref, codec, required)
	if err != nil {
		return nil, 0, err
	}
	s, err := fetch.OpenTarStream(path, options...)
	if err != nil {
		return nil, 0, protectLocalArtifactError(codec, "open local artifact", err)
	}
	if err := validateFileRefIdentity(ref, path, s); err != nil {
		_ = s.Close()
		return nil, 0, err
	}
	return s, int64(s.Size()), nil
}

// OpenLayeredBlockReader opens refs in top-to-bottom order and composes them as
// one read-only block source. A single ref is returned without an extra layer.
func OpenLayeredBlockReader(ctx context.Context, refs []string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, required bool) (vhost.BlockReader, int64, error) {
	return OpenLayeredBlockReaderWithOpener(ctx, refs, fetcher, locations, codec, required, nil)
}

// OpenLayeredBlockReaderWithOpener is OpenLayeredBlockReader with unified
// file:// format detection for every layer.
func OpenLayeredBlockReaderWithOpener(ctx context.Context, refs []string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, required bool, opener FileStreamOpener) (vhost.BlockReader, int64, error) {
	if len(refs) == 0 {
		return nil, 0, errors.New("disk layer list is empty")
	}
	streams := make([]fetch.Stream, 0, len(refs))
	var logicalSize int64 = -1
	for i, ref := range refs {
		stream, size, err := OpenDiskStreamAtWithOpener(ctx, ref, fetcher, locations, "", codec, required, opener)
		if err != nil {
			for _, opened := range streams {
				_ = opened.Close()
			}
			return nil, 0, fmt.Errorf("layer[%d]: %w", i, err)
		}
		if logicalSize < 0 {
			logicalSize = size
		} else if size != logicalSize {
			closeErr := stream.Close()
			for _, opened := range streams {
				closeErr = errors.Join(closeErr, opened.Close())
			}
			return nil, 0, errors.Join(
				fmt.Errorf("layer[%d] logical size %d differs from layer[0] size %d", i, size, logicalSize),
				closeErr,
			)
		}
		streams = append(streams, stream)
	}
	stream := streams[0]
	if len(streams) > 1 {
		stream = fetch.NewLayered(streams...)
	}
	return vhost.NewStreamReader(ctx, stream, logicalSize), logicalSize, nil
}

type localArtifactError struct {
	op  string
	err error
}

func (e *localArtifactError) Error() string { return e.op + " failed" }
func (e *localArtifactError) Unwrap() error { return e.err }

func protectLocalArtifactError(codec tarstream.Codec, op string, err error) error {
	if codec == nil || err == nil {
		return err
	}
	return &localArtifactError{op: op, err: err}
}

// OpenManifestStream resolves one manifest:// key into a fetch.Stream and its
// image size. Exported so callers (notably pkg/restore for snapshot
// memory bundles) can share the code path.
//
// fetcher's underlying store/cache client is shared with every read it
// produces; callers close it when the sandbox lifecycle ends.
func OpenManifestStream(ctx context.Context, keyRef string, fetcher fetch.Fetcher) (fetch.Stream, int64, error) {
	if fetcher == nil {
		return nil, 0, errors.New("manifest:// requires a fetch.Fetcher")
	}
	key, err := manifest.ParseKeyRef(keyRef)
	if err != nil {
		return nil, 0, err
	}
	stream, err := readretry.Open(ctx, func() (fetch.Stream, error) { return fetcher.OpenManifest(ctx, key) })
	if err != nil {
		return nil, 0, err
	}
	return stream, int64(stream.Size()), nil
}

func fileReadOptions(ref manifest.Ref, codec tarstream.Codec, required bool) ([]tarstream.ReadOption, error) {
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

func validateFileRefIdentity(ref manifest.Ref, path string, stream fetch.Stream) error {
	digester, ok := stream.(tarstream.Digester)
	if !ok {
		return fmt.Errorf("local tarstream: artifact has no declared digest")
	}
	scheme, digest := digester.Digest()
	if scheme != tarstream.DigestScheme && scheme != tarstream.DigestSchemeHMAC {
		return fmt.Errorf("local tarstream: artifact digest scheme is incompatible with policy")
	}
	if ref.Digest != "" {
		// fileReadOptions supplied the policy-normalized expected identity to
		// the tarstream parser, which compares it in constant time.
		return nil
	}
	if ref.Location == "" {
		// An unqualified node-local path is an explicit provisioning input,
		// not a content-addressed lookup. Its caller may canonicalize the
		// identity returned by the stream after this open.
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

// needsManifestFetcher returns true if any disk URI in cfg uses the
// manifest:// scheme — boot.root and every boot.disks[] node, in both
// single-disk (base) and overlay (overlay.base) modes. The result decides
// whether sandbox-ctl must dial store-ctl / cache-ctl on this run.
func needsManifestFetcher(cfg *config.SandboxConfig) bool {
	nodes := []config.RootConfig{cfg.Boot.Root}
	for _, d := range cfg.Boot.Disks {
		nodes = append(nodes, d.RootConfig)
	}
	for _, n := range nodes {
		candidates := append([]string{n.Base}, n.BaseFromRefs...)
		if n.Overlay != nil {
			candidates = append(candidates, n.Overlay.Base)
			candidates = append(candidates, n.Overlay.BaseFromRefs...)
		}
		for _, uri := range candidates {
			if uri == "" {
				continue
			}
			if scheme, _, ok := config.SchemeAndPath(uri); ok && scheme == "manifest" {
				return true
			}
		}
	}
	return false
}

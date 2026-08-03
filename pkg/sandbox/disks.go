package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/internal/tartransition"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

// OpenBlockReader resolves a file:// or manifest:// disk URI into a
// vhost.BlockReader plus the total disk size, via a fetch.Stream. file://
// opens a local tarstream artifact; manifest:// resolves one manifest through
// the fetcher. Both wrap in a
// StreamReader whose Close releases the stream (the file's fd; a manifest
// stream's cache/store client is owned by the Fetcher and closed separately).
//
// For manifest:// the fetcher must be non-nil — it carries the cache-ctl /
// store-ctl client and the per-process decryptor. Cold-start lifecycle and
// restore both construct the fetcher up front (only when manifest:// resources
// are referenced) and share it across every disk URI.
//
// ctx scopes asynchronous chunk fetches kicked off by later ReadAt calls;
// cancelling it makes pending vhost-user-blk reads fail promptly at shutdown.
func OpenBlockReader(ctx context.Context, uri string, fetcher fetch.Fetcher, locations config.RefLocations) (vhost.BlockReader, int64, error) {
	stream, size, err := OpenDiskStream(ctx, uri, fetcher, locations)
	if err != nil {
		return nil, 0, err
	}
	return vhost.NewStreamReader(ctx, stream, size), size, nil
}

// OpenDiskStream resolves a file:// or manifest:// disk URI into a fetch.Stream
// and its size. Exported so callers outside this package (notably
// pkg/restore) can share the same code path. The caller owns the
// returned stream and must Close it (directly or via a StreamReader).
func OpenDiskStream(ctx context.Context, uri string, fetcher fetch.Fetcher, locations config.RefLocations) (fetch.Stream, int64, error) {
	ref, err := manifest.ParseRef(uri)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid disk URI %q: %w", uri, err)
	}
	switch ref.Scheme {
	case manifest.RefSchemeFile:
		// Local disk artifacts are tarstream envelopes (image/overlay);
		// the hole map comes from the envelope, never the filesystem.
		path, err := locations.ResolveFile(ref, "")
		if err != nil {
			return nil, 0, err
		}
		s, err := fetch.OpenTarStream(path)
		if err != nil {
			return nil, 0, err
		}
		if err := validateFileRefIdentity(ref, s); err != nil {
			s.Close()
			return nil, 0, err
		}
		return s, int64(s.Size()), nil
	case manifest.RefSchemeManifest:
		return OpenManifestStream(ctx, ref.Path, fetcher)
	default:
		return nil, 0, fmt.Errorf("unknown disk URI scheme: %s", ref.Scheme)
	}
}

// OpenLayeredBlockReader opens refs in top-to-bottom order and composes them as
// one read-only block source. A single ref is returned without an extra layer.
func OpenLayeredBlockReader(ctx context.Context, refs []string, fetcher fetch.Fetcher, locations config.RefLocations) (vhost.BlockReader, int64, error) {
	if len(refs) == 0 {
		return nil, 0, errors.New("disk layer list is empty")
	}
	streams := make([]fetch.Stream, 0, len(refs))
	for i, ref := range refs {
		stream, _, err := OpenDiskStream(ctx, ref, fetcher, locations)
		if err != nil {
			for _, opened := range streams {
				_ = opened.Close()
			}
			return nil, 0, fmt.Errorf("layer[%d] %q: %w", i, ref, err)
		}
		streams = append(streams, stream)
	}
	stream := streams[0]
	if len(streams) > 1 {
		stream = fetch.NewLayered(streams...)
	}
	size := int64(stream.Size())
	return vhost.NewStreamReader(ctx, stream, size), size, nil
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
	stream, err := fetcher.OpenManifest(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	return stream, int64(stream.Size()), nil
}

func validateFileRefIdentity(ref manifest.Ref, stream fetch.Stream) error {
	tagged, ok, err := tartransition.Digest(stream)
	if err != nil {
		return fmt.Errorf("file ref %q has invalid digest marker: %w", ref.String(), err)
	}
	if !ok {
		return fmt.Errorf("file ref %q has no digest marker", ref.String())
	}
	digest, err := tartransition.SHA256Digest(tagged)
	if err != nil {
		return fmt.Errorf("file ref %q uses an unsupported digest scheme: %w", ref.String(), err)
	}
	expected, err := tartransition.SHA256RefDigest(ref)
	if err != nil {
		return fmt.Errorf("file ref %q uses an unsupported digest scheme: %w", ref.String(), err)
	}
	if expected != "" && expected != digest {
		return fmt.Errorf("file ref %q digest mismatch: got %s", ref.String(), digest)
	}
	if ref.Location != "" {
		contentName := strings.TrimSuffix(ref.Path, filepath.Ext(ref.Path))
		if contentName != digest {
			return fmt.Errorf("located file ref %q content name does not match digest %s", ref.String(), digest)
		}
	}
	return nil
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

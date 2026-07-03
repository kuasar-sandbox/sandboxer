package sandbox

import (
	"context"
	"errors"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

// OpenBlockReader resolves a file:// or manifest:// disk URI into a
// vhost.BlockReader plus the total disk size, via a fetch.Stream. file://
// opens a local tarstream artifact; manifest:// (one key, or ':'-joined keys
// that overlay as layers) resolves through the fetcher. Both wrap in a
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
func OpenBlockReader(ctx context.Context, uri string, fetcher fetch.Fetcher) (vhost.BlockReader, int64, error) {
	stream, size, err := OpenDiskStream(ctx, uri, fetcher)
	if err != nil {
		return nil, 0, err
	}
	return vhost.NewStreamReader(ctx, stream, size), size, nil
}

// OpenDiskStream resolves a file:// or manifest:// disk URI into a fetch.Stream
// and its size. Exported so callers outside this package (notably
// pkg/restore) can share the same code path. The caller owns the
// returned stream and must Close it (directly or via a StreamReader).
func OpenDiskStream(ctx context.Context, uri string, fetcher fetch.Fetcher) (fetch.Stream, int64, error) {
	scheme, value, ok := config.SchemeAndPath(uri)
	if !ok {
		return nil, 0, fmt.Errorf("invalid disk URI: %s", uri)
	}
	switch scheme {
	case "file":
		// Local disk artifacts are tarstream envelopes (image/overlay);
		// the hole map comes from the envelope, never the filesystem.
		s, err := fetch.OpenTarStream(value)
		if err != nil {
			return nil, 0, err
		}
		return s, int64(s.Size()), nil
	case "manifest":
		return OpenManifestStream(ctx, value, fetcher)
	default:
		return nil, 0, fmt.Errorf("unknown disk URI scheme: %s", scheme)
	}
}

// OpenManifestStream resolves a manifest:// key reference (one key, or
// several ':'-joined keys that overlay as layers) into a fetch.Stream and its
// image size. Exported so callers (notably pkg/restore for snapshot
// memory bundles) can share the code path.
//
// fetcher's underlying store/cache client is shared with every read it
// produces; callers close it when the sandbox lifecycle ends.
func OpenManifestStream(ctx context.Context, keyRef string, fetcher fetch.Fetcher) (fetch.Stream, int64, error) {
	if fetcher == nil {
		return nil, 0, errors.New("manifest:// requires a fetch.Fetcher")
	}
	keys, err := manifest.ParseKeyRefs(keyRef)
	if err != nil {
		return nil, 0, err
	}
	stream, err := fetcher.Fetch(ctx, keys...)
	if err != nil {
		return nil, 0, err
	}
	return stream, int64(stream.Size()), nil
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
		candidates := []string{n.Base}
		if n.Overlay != nil {
			candidates = append(candidates, n.Overlay.Base)
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

// Compile-time guard: store.ContentKey is used indirectly by
// ParseHexKey above. Keep the import alive without an unused symbol.
var _ = store.PartitionChunk

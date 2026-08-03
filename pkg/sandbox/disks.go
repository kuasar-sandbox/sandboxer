package sandbox

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
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
// restore share one process-owned fetcher across every disk URI; its network
// clients are created only when a manifest is actually opened.
//
// ctx scopes asynchronous chunk fetches kicked off by later ReadAt calls;
// cancelling it makes pending vhost-user-blk reads fail promptly at shutdown.
func OpenBlockReader(ctx context.Context, uri string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, required bool) (vhost.BlockReader, int64, error) {
	stream, size, err := OpenDiskStream(ctx, uri, fetcher, locations, codec, required)
	if err != nil {
		return nil, 0, err
	}
	return vhost.NewStreamReader(ctx, stream, size), size, nil
}

// OpenDiskStream resolves a file:// or manifest:// disk URI into a fetch.Stream
// and its size. Exported so callers outside this package (notably
// pkg/restore) can share the same code path. The caller owns the
// returned stream and must Close it (directly or via a StreamReader).
func OpenDiskStream(ctx context.Context, uri string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, required bool) (fetch.Stream, int64, error) {
	ref, err := manifest.ParseRef(uri)
	if err != nil {
		return nil, 0, protectLocalArtifactError(codec, "parse local artifact ref", err)
	}
	switch ref.Scheme {
	case manifest.RefSchemeFile:
		// Local disk artifacts are tarstream envelopes (image/overlay);
		// the hole map comes from the envelope, never the filesystem.
		path, err := locations.ResolveFile(ref, "")
		if err != nil {
			return nil, 0, protectLocalArtifactError(codec, "resolve local artifact ref", err)
		}
		options, err := fileReadOptions(ref, codec, required)
		if err != nil {
			return nil, 0, err
		}
		s, err := fetch.OpenTarStream(path, options...)
		if err != nil {
			return nil, 0, protectLocalArtifactError(codec, "open local artifact", err)
		}
		if err := validateFileRefIdentity(ref, path, s, codec, required); err != nil {
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
func OpenLayeredBlockReader(ctx context.Context, refs []string, fetcher fetch.Fetcher, locations config.RefLocations, codec tarstream.Codec, required bool) (vhost.BlockReader, int64, error) {
	if len(refs) == 0 {
		return nil, 0, errors.New("disk layer list is empty")
	}
	streams := make([]fetch.Stream, 0, len(refs))
	for i, ref := range refs {
		stream, _, err := OpenDiskStream(ctx, ref, fetcher, locations, codec, required)
		if err != nil {
			for _, opened := range streams {
				_ = opened.Close()
			}
			return nil, 0, fmt.Errorf("layer[%d]: %w", i, err)
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
	stream, err := fetcher.OpenManifest(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	return stream, int64(stream.Size()), nil
}

func fileReadOptions(ref manifest.Ref, codec tarstream.Codec, required bool) ([]tarstream.ReadOption, error) {
	if codec == nil {
		if required {
			return nil, fmt.Errorf("local tarstream: required policy has no codec")
		}
		if ref.DigestScheme == tarstream.DigestSchemeHMAC {
			return nil, fmt.Errorf("local tarstream: hmac ref requires crypto.local=auto or required")
		}
		if ref.DigestScheme == tarstream.DigestSchemeSHA256 {
			return []tarstream.ReadOption{tarstream.WithExpectedDigest(ref.DigestScheme, ref.Digest)}, nil
		}
		return nil, nil
	}

	options := []tarstream.ReadOption{tarstream.WithCodec(codec, required)}
	switch ref.DigestScheme {
	case "":
		return options, nil
	case tarstream.DigestSchemeHMAC:
		return append(options, tarstream.WithExpectedDigest(tarstream.DigestSchemeHMAC, ref.Digest)), nil
	case tarstream.DigestSchemeSHA256:
		if required {
			return nil, fmt.Errorf("local tarstream: legacy sha256 ref is forbidden by required policy")
		}
		keyed, err := keyedDigestHex(codec, ref.Digest)
		if err != nil {
			return nil, fmt.Errorf("local tarstream: invalid legacy sha256 ref")
		}
		return append(options, tarstream.WithExpectedDigest(tarstream.DigestSchemeHMAC, keyed)), nil
	default:
		return nil, fmt.Errorf("local tarstream: unsupported digest scheme")
	}
}

func validateFileRefIdentity(ref manifest.Ref, path string, stream fetch.Stream, codec tarstream.Codec, required bool) error {
	digester, ok := stream.(tarstream.Digester)
	if !ok {
		return fmt.Errorf("local tarstream: artifact has no declared digest")
	}
	scheme, digest := digester.Digest()
	wantScheme := tarstream.DigestSchemeSHA256
	if codec != nil {
		wantScheme = tarstream.DigestSchemeHMAC
	}
	if scheme != wantScheme {
		return fmt.Errorf("local tarstream: artifact digest scheme is incompatible with policy")
	}
	if ref.Digest != "" {
		// fileReadOptions supplied the policy-normalized expected identity to
		// the tarstream parser, which compares it in constant time.
		return nil
	}

	realPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		realPath = resolved
	}
	base := filepath.Base(realPath)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if codec == nil || required {
		if !digestEqual(stem, digest) {
			return fmt.Errorf("local tarstream: content name does not match artifact identity")
		}
		return nil
	}
	if digestEqual(stem, digest) {
		return nil
	}
	legacy, err := keyedDigestHex(codec, stem)
	if err != nil || !digestEqual(legacy, digest) {
		return fmt.Errorf("local tarstream: content name does not match artifact identity")
	}
	return nil
}

func keyedDigestHex(codec tarstream.Codec, plainHex string) (string, error) {
	var plain [32]byte
	if len(plainHex) != hex.EncodedLen(len(plain)) || strings.ToLower(plainHex) != plainHex {
		return "", fmt.Errorf("invalid digest")
	}
	if _, err := hex.Decode(plain[:], []byte(plainHex)); err != nil {
		return "", fmt.Errorf("invalid digest")
	}
	keyed := codec.KeyedDigest(plain)
	return hex.EncodeToString(keyed[:]), nil
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

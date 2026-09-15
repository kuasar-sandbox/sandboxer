package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

// OpenFlattenedImage opens and validates one flattened image carried by a
// digest-qualified local tarstream, manifest:// ref, or located Manifest
// Bundle. The returned image owns the selected stream and must be closed.
func OpenFlattenedImage(ctx context.Context, raw string, storage *artifact.ProcessStorage, locations config.RefLocations) (*sandboxfile.FlattenedImage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if storage == nil {
		return nil, errors.New("open flattened image: process storage is required")
	}
	ref, path, err := resolveFlattenedImageRef(raw, locations)
	if err != nil {
		return nil, err
	}

	var stream fetch.Stream
	switch ref.Scheme {
	case manifest.RefSchemeManifest:
		if storage.Fetcher() == nil {
			return nil, errors.New("open flattened image: manifest configuration is required")
		}
		key, err := manifest.ParseKeyRef(ref.Path)
		if err != nil {
			return nil, fmt.Errorf("open flattened image manifest ref: %w", err)
		}
		stream, err = readretry.Open(ctx, func() (fetch.Stream, error) { return storage.Fetcher().OpenManifest(ctx, key) })
		if err != nil {
			return nil, fmt.Errorf("open flattened image manifest: %w", err)
		}
	case manifest.RefSchemeFile:
		stream, err = storage.OpenFileWithLocations(ctx, path, ref, locations)
		if err != nil {
			return nil, fmt.Errorf("open flattened image file: %w", err)
		}
	default:
		return nil, fmt.Errorf("open flattened image: unsupported ref scheme %q", ref.Scheme)
	}
	return sandboxfile.OpenFlattenedEROFS(ctx, stream)
}

func resolveFlattenedImageRef(raw string, locations config.RefLocations) (manifest.Ref, string, error) {
	if strings.TrimSpace(raw) == "" {
		return manifest.Ref{}, "", errors.New("open flattened image: source is required")
	}
	if !strings.HasPrefix(raw, "file://") && !strings.HasPrefix(raw, "manifest://") {
		path, err := filepath.Abs(raw)
		if err != nil {
			return manifest.Ref{}, "", err
		}
		return manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path}, path, nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return manifest.Ref{}, "", err
	}
	if ref.Scheme != manifest.RefSchemeFile {
		return ref, "", nil
	}
	path, err := locations.ResolveFile(ref, "")
	if err != nil {
		return manifest.Ref{}, "", err
	}
	if !filepath.IsAbs(path) {
		path, err = filepath.Abs(path)
		if err != nil {
			return manifest.Ref{}, "", err
		}
	}
	return ref, path, nil
}

// PrepareSandboxEConfig folds the image's persistent OCI defaults into cfg,
// validates every cold portable field, canonicalizes local artifact refs, and
// returns the direct-EROFS self layout used by a top-level Sandbox E. cfg is
// intentionally updated in place, matching MaterializeImageDefaults.
func PrepareSandboxEConfig(
	ctx context.Context,
	cfg *config.SandboxConfig,
	imageConfig []byte,
	locations config.RefLocations,
	codec tarstream.Codec,
	required bool,
	opener FileStreamOpener,
) (*config.PortableSandboxConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("Sandbox E assembly config is nil")
	}
	imageDefaults, err := LoadImageConfigBytes(imageConfig)
	if err != nil {
		return nil, fmt.Errorf("Sandbox E config.json: %w", err)
	}
	if err := MaterializeImageDefaults(cfg, imageDefaults); err != nil {
		return nil, fmt.Errorf("Sandbox E image defaults: %w", err)
	}
	if err := cfg.ValidateColdProjection(); err != nil {
		return nil, err
	}
	if err := canonicalizeConfiguredTarRefsWithOpener(ctx, cfg, locations, codec, required, opener); err != nil {
		return nil, err
	}
	identities, err := ResolvePortableProjection(cfg)
	if err != nil {
		return nil, err
	}
	return config.NewPortableEROFS(cfg, identities)
}

// AssembleSandboxE appends the canonical Sandbox runtime configuration to a
// validated flattened image without flattening its Hole/Zero/Data map. The
// returned source borrows image: image remains caller-owned and must stay open
// until the source has been completely consumed. No intermediate .sandbox file
// is created.
func AssembleSandboxE(ctx context.Context, image *sandboxfile.FlattenedImage, portable *config.PortableSandboxConfig) (sparse.Source, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if image == nil || image.Payload == nil {
		return nil, errors.New("Sandbox E assembly requires an open flattened image")
	}
	if portable == nil {
		return nil, errors.New("Sandbox E assembly requires a portable config")
	}
	runtimeConfig, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		return nil, err
	}
	source, err := sandboxfile.BuildSourceContext(ctx, image.Payload, image.ImageConfig, runtimeConfig)
	if err != nil {
		return nil, fmt.Errorf("assemble Sandbox E: %w", err)
	}
	return source, nil
}

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

// OpenDiskV1Source prepares the identical read-only source for cold launch
// and for a running slot. The caller owns the returned base reader; template
// is a resolved file URI accepted by PrepareDiff.
func OpenDiskV1Source(ctx context.Context, spec config.DiskV1, storage *artifact.ProcessStorage, locations config.RefLocations, opts ...diffstore.DiskOption) (base diffstore.BlockReader, size int64, template string, err error) {
	return openDiskV1Source(ctx, spec, storage, locations, nil, opts...)
}

// When resolved is supplied, record immutable identities from the very file
// handles backing the live reader, including a selected non-root Bundle member.
func openDiskV1Source(ctx context.Context, spec config.DiskV1, storage *artifact.ProcessStorage, locations config.RefLocations, resolved *config.DiskV1, opts ...diffstore.DiskOption) (base diffstore.BlockReader, size int64, template string, err error) {
	if err = spec.Validate(); err != nil {
		return
	}
	if storage == nil {
		return nil, 0, "", errors.New("disk: storage is required")
	}
	if resolved != nil {
		*resolved = spec
		resolved.BaseFromRefs = slices.Clone(spec.BaseFromRefs)
	}
	var streams []fetch.Stream
	defer func() {
		if err != nil {
			for _, s := range streams {
				err = errors.Join(err, s.Close())
			}
		}
	}()
	if spec.Base != "" {
		for i, raw := range append([]string{spec.Base}, spec.BaseFromRefs...) {
			r, _ := manifest.ParseRef(raw)
			s, ref, err := openPureLayer(ctx, r, storage, locations, resolved != nil)
			if err != nil {
				return nil, 0, "", fmt.Errorf("disk layer %d: %w", i, err)
			}
			if resolved != nil {
				if i == 0 {
					resolved.Base = ref.String()
				} else {
					resolved.BaseFromRefs[i-1] = ref.String()
				}
			}
			streams = append(streams, s)
			if i == 0 {
				size = int64(s.Size())
			}
			if s.Size() > uint64(size) || size <= 0 {
				return nil, 0, "", errors.New("disk layer exceeds top logical capacity")
			}
		}
	}
	if spec.DiffTemplate != "" {
		ref, _ := manifest.ParseRef(spec.DiffTemplate)
		path, e := artifact.ResolveDiskTemplate(ref, locations)
		if e != nil {
			return nil, 0, "", e
		}
		capacity, e := diffstore.DiffSourceCapacity(path, true, opts...)
		if e != nil {
			return nil, 0, "", e
		}
		if capacity < size {
			return nil, 0, "", errors.New("disk: template smaller than base")
		}
		size = capacity
		template = "file://" + path
	}
	if err = diffstore.ValidateWritableSize(size); err != nil {
		return nil, 0, "", err
	}
	if spec.Size != nil && size != *spec.Size {
		return nil, 0, "", fmt.Errorf("disk: source capacity %d differs from requested size %d", size, *spec.Size)
	}
	if len(streams) > 0 {
		base = diffstore.NewStreamReader(ctx, fetch.NewLayered(streams...), int64(streams[0].Size()))
	}
	return
}

// A retry may use a renamed file carrier without an identity selector. Read
// only its immutable layer identity. Provisioning templates are one-time inputs.
func diskRetryIdentity(ctx context.Context, spec config.DiskV1, storage *artifact.ProcessStorage, locations config.RefLocations) (string, error) {
	pin := func(raw string) (string, error) {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return "", err
		}
		if ref.Scheme != manifest.RefSchemeFile || ref.DigestScheme != "" {
			return raw, nil
		}
		stream, actual, err := openPureLayer(ctx, ref, storage, locations, true)
		if err != nil {
			return "", err
		}
		return actual.String(), stream.Close()
	}
	if spec.Base != "" {
		var err error
		if spec.Base, err = pin(spec.Base); err != nil {
			return "", err
		}
		spec.BaseFromRefs = slices.Clone(spec.BaseFromRefs)
		for i, raw := range spec.BaseFromRefs {
			if spec.BaseFromRefs[i], err = pin(raw); err != nil {
				return "", err
			}
		}
	}
	return spec.Identity(), nil
}

func openPureLayer(ctx context.Context, ref manifest.Ref, storage *artifact.ProcessStorage, locations config.RefLocations, pin bool) (fetch.Stream, manifest.Ref, error) {
	if ref.Scheme == manifest.RefSchemeManifest {
		stream, _, err := OpenManifestStream(ctx, ref.Path, storage.Fetcher())
		return stream, ref, err
	}
	path, err := locations.ResolveFile(ref, "")
	if err != nil {
		return nil, ref, err
	}
	file, err := storage.OpenFileWithLocations(ctx, path, ref, locations)
	if err != nil {
		return nil, ref, err
	}
	if pin && ref.DigestScheme == "" {
		if key, bundle := file.RootManifestKey(); bundle {
			ref.DigestScheme, ref.Digest = "manifest", manifest.HexKey(key)
		} else {
			ref.DigestScheme, ref.Digest = file.Digest()
		}
		if ref.DigestScheme == "" || ref.Digest == "" {
			return nil, ref, errors.Join(errors.New("disk layer has no content identity"), file.Close())
		}
	}
	return file, ref, nil
}

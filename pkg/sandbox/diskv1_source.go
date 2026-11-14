package sandbox

import (
	"context"
	"errors"
	"fmt"

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
	if err = spec.Validate(); err != nil {
		return
	}
	if storage == nil {
		return nil, 0, "", errors.New("disk: storage is required")
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
			var s fetch.Stream
			if r.Scheme == manifest.RefSchemeManifest {
				s, _, err = OpenManifestStream(ctx, r.Path, storage.Fetcher())
			} else {
				var path string
				path, err = locations.ResolveFile(r, "")
				if err == nil {
					s, err = storage.OpenFileWithLocations(ctx, path, r, locations)
				}
			}
			if err != nil {
				return nil, 0, "", fmt.Errorf("disk layer %d: %w", i, err)
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

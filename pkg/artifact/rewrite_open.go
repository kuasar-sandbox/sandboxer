package artifact

import (
	"context"
	"errors"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/transfer"
	"path/filepath"
)

// open preserves Bundle source selection and gives local tarstreams the same
// pinned-file, full-identity validation used by manifest-ctl transfers.
func (p *publicationRewrite) open(ctx context.Context, raw string, scope publishScope) (fetch.Stream, publishScope, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return nil, scope, err
	}
	if ref.Scheme != manifest.RefSchemeFile {
		return p.p.open(ctx, raw, scope)
	}
	path, err := p.p.locations.ResolveFile(ref, scope.relativeDir)
	if err != nil {
		return nil, scope, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, scope, err
	}
	path, err = resolveLocatedFileTarget(path, ref, p.p.locations)
	if err != nil {
		return nil, scope, err
	}
	format, err := DetectFileFormat(path)
	if err != nil {
		return nil, scope, err
	}
	if format == FileFormatManifestBundle {
		return p.p.open(ctx, raw, scope)
	}
	cfg := manifest.Config{}
	if p.p.storage.cfg != nil {
		cfg = *p.p.storage.cfg
	}
	verify := true
	cfg.Manifest.VerifyContent = &verify
	key := [32]byte{}
	if keyFn := p.p.storage.CustomerKeyFunc(); keyFn != nil {
		key, err = keyFn()
		if err != nil {
			return nil, scope, err
		}
	}
	defer clear(key[:])
	reader, err := transfer.NewReader(&cfg, key, p.p.locations)
	if err != nil {
		return nil, scope, err
	}
	local := ref
	if local.Location == "" {
		local.Path = path
	}
	opened, err := reader.Open(ctx, local.String())
	if err != nil {
		return nil, scope, errors.Join(err, reader.Close())
	}
	if err = opened.Verify(ctx); err != nil {
		return nil, scope, errors.Join(err, opened.Close(), reader.Close())
	}
	p.closes = append(p.closes, reader.Close)
	scope.relativeDir = filepath.Dir(path)
	return opened, scope, nil
}

package artifact

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/transfer"
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
	path, err = resolveFileTarget(path, ref, p.p.locations)
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
	codec, required, err := p.p.storage.readCodec(ref.Location != "" && !p.p.transientLocations[ref.Location])
	if err != nil {
		return nil, scope, err
	}
	options, err := tarReadOptions(ref, codec, required)
	if err != nil {
		return nil, scope, err
	}
	opened, err := transfer.OpenTarstream(path, options...)
	if err != nil {
		return nil, scope, protectProcessLocalReadError(codec, "open rewrite tarstream", err)
	}
	if err = validateTarIdentity(ref, path, opened.Source); err == nil {
		err = opened.Verify(ctx)
	}
	if err != nil {
		return nil, scope, errors.Join(protectProcessLocalReadError(codec, "verify rewrite tarstream", err), opened.Close())
	}
	scope.relativeDir = filepath.Dir(path)
	return opened, scope, nil
}

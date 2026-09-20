package artifact

import (
	"context"
	"errors"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// SnapshotSandboxRef reads only S root metadata and selects its E binding. It
// does not open E, scan dependencies, or read memory/disk payloads. Callers with
// an already parsed Snapshot should keep its known final E instead.
func (s *ProcessStorage) SnapshotSandboxRef(ctx context.Context, input string, locations config.RefLocations) (ref string, retErr error) {
	p := &Publisher{storage: s, locations: locations}
	raw, scope, err := p.normalizeRoot(input)
	if err != nil {
		return "", err
	}
	stream, child, err := p.open(ctx, raw, scope)
	if err != nil {
		return "", err
	}
	root, err := snapshotfile.Open(ctx, stream)
	if err != nil {
		return "", err
	}
	defer func() {
		retErr = errors.Join(retErr, root.Close())
		if retErr != nil {
			ref = ""
		}
	}()
	cfg, err := readRewriteSnapshot(root)
	if err != nil {
		return "", err
	}
	if parsed, err := manifest.ParseRef(cfg.SandboxRef); err == nil && parsed.Scheme == manifest.RefSchemeManifest {
		if local, ok := child.fetcher.(*manifestbundle.ManifestFetcher); ok {
			key, _ := manifest.ParseKeyRef(parsed.Path)
			selected, err := local.SelectManifest(ctx, key)
			if err != nil {
				return "", err
			}
			child.rememberSelection(cfg.SandboxRef, selected)
		}
	}
	return scopedRef(cfg.SandboxRef, child), nil
}

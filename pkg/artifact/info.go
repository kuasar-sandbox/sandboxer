package artifact

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

type Info struct {
	Role     LogicalRole
	Raw      []byte
	Sandbox  *config.PortableSandboxConfig
	Snapshot *snapshot.Config
}

// Inspect opens local tarstream, Manifest or Manifest Bundle input and returns
// the strict logical root config. It never guesses from the outer ZIP magic.
func (s *ProcessStorage) Inspect(ctx context.Context, input string, locations config.RefLocations) (*Info, error) {
	if s == nil {
		return nil, errors.New("artifact info: process storage is required")
	}
	ref, relativeDir, err := normalizeInfoInput(input)
	if err != nil {
		return nil, err
	}
	open := func() (fetch.Stream, error) {
		switch ref.Scheme {
		case manifest.RefSchemeManifest:
			if s.Fetcher() == nil {
				return nil, errors.New("artifact info: manifest input requires manifest configuration")
			}
			key, err := manifest.ParseKeyRef(ref.Path)
			if err != nil {
				return nil, err
			}
			return readretry.Open(ctx, func() (fetch.Stream, error) { return s.Fetcher().OpenManifest(ctx, key) })
		case manifest.RefSchemeFile:
			path, err := locations.ResolveFile(ref, relativeDir)
			if err != nil {
				return nil, err
			}
			if !filepath.IsAbs(path) {
				path, err = filepath.Abs(path)
				if err != nil {
					return nil, err
				}
			}
			return s.OpenFileWithLocations(ctx, path, ref, locations)
		default:
			return nil, fmt.Errorf("artifact info: unsupported scheme %q", ref.Scheme)
		}
	}
	stream, err := open()
	if err != nil {
		return nil, err
	}
	sandboxRoot, sandboxErr := sandboxfile.Open(ctx, stream)
	if sandboxErr == nil {
		defer sandboxRoot.Close()
		return &Info{
			Role: RoleSandbox, Raw: append([]byte(nil), sandboxRoot.RuntimeConfig...), Sandbox: sandboxRoot.Portable,
		}, nil
	}
	stream, err = open()
	if err != nil {
		return nil, errors.Join(sandboxErr, err)
	}
	snapshotRoot, snapshotErr := snapshotfile.Open(ctx, stream)
	if snapshotErr != nil {
		return nil, fmt.Errorf("artifact info: logical root is neither a strict .sandbox nor .snapshot: %w", errors.Join(sandboxErr, snapshotErr))
	}
	defer snapshotRoot.Close()
	cfg, err := snapshot.ParseConfig(snapshotRoot.SnapshotConfig)
	if err != nil {
		return nil, fmt.Errorf("unsupported snapshot format/version: %w", err)
	}
	canonical, err := snapshot.MarshalConfig(cfg)
	if err != nil || string(canonical) != string(snapshotRoot.SnapshotConfig) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("snapshot.cfg is not canonically encoded")
	}
	return &Info{
		Role: RoleSnapshot, Raw: append([]byte(nil), snapshotRoot.SnapshotConfig...), Snapshot: cfg,
	}, nil
}

func normalizeInfoInput(input string) (manifest.Ref, string, error) {
	if strings.TrimSpace(input) == "" {
		return manifest.Ref{}, "", errors.New("artifact info: input is required")
	}
	if strings.HasPrefix(input, "file://") || strings.HasPrefix(input, "manifest://") {
		ref, err := manifest.ParseRef(input)
		return ref, "", err
	}
	path, err := filepath.Abs(input)
	if err != nil {
		return manifest.Ref{}, "", err
	}
	return manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path}, filepath.Dir(path), nil
}

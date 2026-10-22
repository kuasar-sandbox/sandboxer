package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

// DiffCheckpointBaseID binds a DIFF to its complete immutable portable graph.
// The graph must already use the Checkpoint layout (no self placeholder).
func DiffCheckpointBaseID(root config.PortableRootConfig) string {
	// Location and basename select a carrier, not immutable disk identity.
	// Bundle-qualified file refs and manifest refs share the Manifest key.
	identity := func(raw string) string {
		if raw == "" {
			return ""
		}
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return raw
		}
		if ref.Scheme == manifest.RefSchemeManifest {
			return "manifest:" + ref.Path
		}
		return ref.DigestScheme + ":" + ref.Digest
	}
	clone := root
	clone.Base = identity(root.Base)
	clone.BaseFromRefs = make([]string, len(root.BaseFromRefs))
	for i, raw := range root.BaseFromRefs {
		clone.BaseFromRefs[i] = identity(raw)
	}
	if root.Overlay != nil {
		overlay := *root.Overlay
		overlay.Base = identity(overlay.Base)
		overlay.BaseFromRefs = make([]string, len(root.Overlay.BaseFromRefs))
		for i, raw := range root.Overlay.BaseFromRefs {
			overlay.BaseFromRefs[i] = identity(raw)
		}
		clone.Overlay = &overlay
	}
	raw, _ := json.Marshal(clone)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// materializeCheckpointRoot consumes each pinned version once into private
// ordinary artifacts. Later publication/rewrite may read those artifacts more
// than once without strengthening the fixed-version sequential export API.
func (p *Publisher) materializeCheckpointRoot(ctx context.Context, scope publishScope, root *sandboxfile.Root) (_ *sandboxfile.Root, _ func() error, retErr error) {
	if root.Checkpoint == nil {
		return root, func() error { return nil }, nil
	}
	if scope.relativeDir == "" {
		return nil, nil, errors.New("checkpoint source has no local carrier directory")
	}
	pins := p.checkpointPins
	var err error
	if pins == nil {
		pins, err = p.storage.PinDiffCheckpoints(ctx, scope.relativeDir, root.Checkpoint)
		if err != nil {
			return nil, nil, err
		}
		defer func() { retErr = errors.Join(retErr, pins.Close()) }()
	} else {
		actual, err := os.Stat(scope.relativeDir)
		if err != nil {
			return nil, nil, err
		}
		if !os.SameFile(actual, pins.identity) {
			return nil, nil, errors.New("checkpoint pinned source directory mismatch")
		}
		if pins.Root.Info().DiffID != root.Checkpoint.Root.DiffID || len(pins.Refs) == 0 || pins.Refs[0].ID != root.Checkpoint.CheckpointID {
			return nil, nil, errors.New("checkpoint pinned source identity mismatch")
		}
		for _, disk := range root.Checkpoint.Disks {
			if pins.Disks[disk.Name] == nil || pins.Disks[disk.Name].Info().DiffID != disk.DiffID {
				return nil, nil, errors.New("checkpoint pinned disk topology mismatch")
			}
		}
	}
	if pins.Root.Info().BaseID != DiffCheckpointBaseID(root.Portable.Boot.Root) {
		return nil, nil, errors.New("checkpoint root base binding mismatch")
	}
	for _, disk := range root.Portable.Boot.Disks {
		if pins.Disks[disk.Name].Info().BaseID != DiffCheckpointBaseID(disk.PortableRootConfig) {
			return nil, nil, fmt.Errorf("checkpoint disk %s base binding mismatch", disk.Name)
		}
	}
	directory, err := os.MkdirTemp("", "sandbox-checkpoint-materialize-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() error { return os.RemoveAll(directory) }
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanup())
		}
	}()
	// Only generated payloads need a private location. Original immutable refs
	// retain their selectors and resolve in the actual source carrier scope.
	tempLocation := "checkpoint-temp-" + diffstore.NewDiffID()
	locations := make(config.RefLocations, len(p.locations)+1)
	for k, v := range p.locations {
		locations[k] = v
	}
	locations[tempLocation] = directory
	p.locations = locations
	if p.transientLocations == nil {
		p.transientLocations = make(map[string]bool)
	}
	p.transientLocations[tempLocation] = true
	sink := snapshot.NewFileSink(directory, "checkpoint-materialize", p.storage.LocalCodec(), p.storage.LocalRequired(), p.logf)
	defer func() { retErr = errors.Join(retErr, sink.Close()) }()
	refs := make([]string, len(root.Portable.Boot.Disks))
	for i, disk := range root.Portable.Boot.Disks {
		raw, _, err := sink.AbsorbOverlaySource(ctx, pins.Disks[disk.Name])
		if err != nil {
			return nil, nil, err
		}
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return nil, nil, err
		}
		ref.Location = tempLocation
		refs[i] = ref.String()
	}
	cfg, err := root.Portable.MaterializedCheckpoint(refs)
	if err != nil {
		return nil, nil, err
	}
	runtime, err := config.MarshalPortableSandboxConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	source, err := sandboxfile.BuildSourceContext(ctx, pins.Root, nil, runtime)
	if err != nil {
		return nil, nil, err
	}
	raw, path, err := sink.AbsorbSandbox(ctx, source)
	if err != nil {
		return nil, nil, err
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return nil, nil, err
	}
	stream, err := p.storage.OpenFileWithLocations(ctx, path, ref, p.locations)
	if err != nil {
		return nil, nil, err
	}
	materialized, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		return nil, nil, err
	}
	if p.report != nil {
		p.report.checkpoints = append(p.report.checkpoints, checkpointPublication{directory: pins.Directory, identity: pins.identity, refs: append([]DiffCheckpointRef(nil), pins.Refs...)})
	}
	return materialized, cleanup, nil
}

func (p *Publisher) keepPortable(ctx context.Context, raw string, scope publishScope) (string, bool, error) {
	portable, ok, err := portablePublishRef(raw)
	if err != nil || !ok {
		return portable, ok, err
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", false, err
	}
	if p.transientLocations[ref.Location] {
		return "", false, nil
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		if local, ok := scope.fetcher.(*manifestbundle.ManifestFetcher); ok {
			key, err := manifest.ParseKeyRef(ref.Path)
			if err != nil {
				return "", false, err
			}
			selected, err := local.SelectManifest(ctx, key)
			if err != nil {
				return "", false, err
			}
			scope.rememberSelection(raw, selected)
			if selected.Reader != nil {
				return "", false, nil
			}
		}
	}
	return portable, true, nil
}

// MaterializeCheckpoint creates ordinary local artifacts through the same
// publication path used by the CLI. The returned root resolves in directory;
// its output no longer depends on local DIFF versions.
func (s *ProcessStorage) MaterializeCheckpoint(ctx context.Context, input string, locations config.RefLocations, directory string) (PublishResult, error) {
	return s.MaterializePinnedCheckpoint(ctx, input, locations, directory, nil)
}

// MaterializePinnedCheckpoint consumes already pinned versions without another identity lookup.
func (s *ProcessStorage) MaterializePinnedCheckpoint(ctx context.Context, input string, locations config.RefLocations, directory string, pins *PinnedDiffCheckpoints) (result PublishResult, retErr error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return result, err
	}
	p := newPublisher(s, locations, newLocationPublishTarget("", directory, s.LocalCodec(), s.LocalRequired(), func(string, ...any) {}), nil)
	p.checkpointPins = pins
	result, retErr = p.Publish(ctx, input)
	retErr = errors.Join(retErr, p.Close())
	if retErr != nil {
		return PublishResult{}, retErr
	}
	return result, nil
}

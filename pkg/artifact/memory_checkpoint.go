package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// SnapshotOpenOptions binds a local descriptor to its actual carrier directory.
// It grants no remote descriptor an implicit current-checkpoint namespace.
type SnapshotOpenOptions struct {
	LocalDirectory string
	RefLocations   config.RefLocations
}

// SnapshotDirtyMemory returns candidate addresses without coercing legacy byte
// runs into page bits or losing a local checkpoint's Top/History provenance.
func SnapshotDirtyMemory(root *snapshotfile.Root) (sparse.Source, error) {
	if root == nil {
		return nil, errors.New("snapshot dirty memory: nil root")
	}
	if root.LocalCheckpoint != nil {
		if root.LocalCheckpoint.Dirty == nil {
			return nil, errors.New("snapshot dirty memory: unresolved local checkpoint")
		}
		return root.LocalCheckpoint.Dirty, nil
	}
	return snapshot.DirtyMemory(root.Memory, root.MemoryDirty)
}

// SnapshotParentViewEqual compares the complete ordered suffix by immutable
// content identity. Carrier paths/locations are not parent identity.
func SnapshotParentViewEqual(declared, actual []string) bool {
	if len(declared) != len(actual) {
		return false
	}
	identity := func(raw string) string {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return ""
		}
		if ref.Scheme == manifest.RefSchemeManifest {
			return "manifest:" + ref.Path
		}
		if ref.Digest == "" {
			return ""
		}
		return ref.DigestScheme + ":" + ref.Digest
	}
	for i, raw := range declared {
		if id := identity(raw); id == "" || id != identity(actual[i]) {
			return false
		}
	}
	return true
}

type borrowedMemorySource struct{ sparse.Source }

func (borrowedMemorySource) Close() error { return nil }

func layerMemorySources(sources ...sparse.Source) sparse.Source {
	layers := make([]fetch.Stream, len(sources))
	for i, source := range sources {
		layers[i] = borrowedMemorySource{source}
	}
	return fetch.NewLayered(layers...)
}

func (p *Publisher) openSnapshot(ctx context.Context, stream fetch.Stream, scope publishScope) (*snapshotfile.Root, error) {
	storage := p.storage
	if scope.fetcher != nil {
		storage = storage.BorrowWithFetcher(scope.fetcher)
	}
	return storage.OpenSnapshot(ctx, stream, SnapshotOpenOptions{LocalDirectory: scope.relativeDir, RefLocations: p.locations})
}

// selectedLocalMemoryCheckpoint opens a named file only after a node-local
// capture record proves this exact carrier is a local memory object. Ordinary
// portable parents remain opaque and are never opened, even when accessible.
func (p *Publisher) selectedLocalMemoryCheckpoint(ctx context.Context, raw string, scope publishScope) (fetch.Stream, publishScope, error) {
	if err := ctx.Err(); err != nil {
		return nil, scope, err
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil || ref.Scheme != manifest.RefSchemeFile || ref.Location == "" {
		return nil, scope, nil
	}
	path, err := p.locations.ResolveFile(ref, scope.relativeDir)
	if err != nil {
		return nil, scope, nil
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || !knownLocalMemoryCarrier(physical, ref) {
		return nil, scope, nil
	}
	stream, child, err := p.open(ctx, raw, scope)
	if err != nil {
		return nil, scope, err
	}
	descriptor, err := snapshotfile.ReadLocalDescriptor(ctx, stream)
	if err == nil && descriptor != nil {
		return stream, child, nil
	}
	closeErr := stream.Close()
	if err == nil {
		err = errors.New("memory checkpoint: recorded carrier has no local descriptor")
	}
	return nil, scope, errors.Join(err, closeErr)
}

func knownLocalMemoryCarrier(path string, ref manifest.Ref) bool {
	directory := filepath.Dir(path)
	current, err := memorycheckpoint.ReadCurrent(directory)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	matches := func(record *memorycheckpoint.CaptureRecord) bool {
		if !record.Committed || record.Aborted {
			return false
		}
		source, err := manifest.ParseRef(record.SnapshotRef)
		if err != nil || source.Scheme != manifest.RefSchemeFile || source.Location != "" ||
			source.DigestScheme != ref.DigestScheme || source.Digest != ref.Digest {
			return false
		}
		selected := source.Path
		if !filepath.IsAbs(selected) {
			selected = filepath.Join(directory, selected)
		}
		selectedInfo, err := os.Stat(selected)
		return err == nil && os.SameFile(info, selectedInfo)
	}
	if matches(current) {
		return true
	}
	// A named historical capture may differ from the current SID obligation.
	// These tiny node-local records are evidence; the parent payload is unopened.
	entries, err := os.ReadDir(filepath.Join(directory, "memory"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, err := memorycheckpoint.ReadCaptureRecord(filepath.Join(directory, "memory", entry.Name()))
		if err != nil {
			continue
		}
		canonical, err := memorycheckpoint.RecordPath(directory, record.CheckpointID)
		if err == nil && filepath.Base(canonical) == entry.Name() && matches(record) {
			return true
		}
	}
	return false
}

// sameCheckpointDirectory is deliberately inode-based: mutable aliases must
// not retarget an opened descriptor into another checkpoint's holder namespace.
func sameCheckpointDirectory(a, b string) bool {
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	return err == nil && os.SameFile(left, right)
}

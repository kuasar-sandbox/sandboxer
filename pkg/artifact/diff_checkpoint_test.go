package artifact

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

type diffCheckpointFixture struct {
	dir, path, ref string
	cfg            *config.PortableSandboxConfig
	cp             *sandboxfile.Checkpoint
	stores         []*diffstore.Store
	owner          *diffstore.DirectoryOwner
}

func newDiffCheckpointFixture(t *testing.T, parent ...string) *diffCheckpointFixture {
	return newDiffCheckpointFixtureWithKey(t, nil, parent...)
}

func newDiffCheckpointFixtureWithKey(t *testing.T, key *[32]byte, parent ...string) *diffCheckpointFixture {
	t.Helper()
	ctx := context.Background()
	f := &diffCheckpointFixture{dir: t.TempDir()}
	owner, err := diffstore.LockDirectory(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.owner = owner
	t.Cleanup(func() { _ = owner.Close() })
	base := ""
	if len(parent) != 0 {
		base = parent[0]
	}
	_, p := publishPortable(t, base)
	p.Boot.Disks = []config.PortableDiskConfig{{Name: "data-a"}, {Name: "data-b"}}
	for _, disk := range p.Boot.Disks {
		p.Mounts = append(p.Mounts, config.MountConfig{Type: "disk", Source: disk.Name, Target: "/" + disk.Name})
	}
	f.cfg, err = p.Checkpointed("")
	if err != nil {
		t.Fatal(err)
	}
	f.cp = &sandboxfile.Checkpoint{Version: 1, CheckpointID: "cp-001", Root: sandboxfile.CheckpointDisk{DiffID: "root-1"}, Disks: []sandboxfile.CheckpointNamedDisk{{Name: "data-a", DiffID: "data-1"}, {Name: "data-b", DiffID: "data-2"}}}
	roots := []config.PortableRootConfig{f.cfg.Boot.Root, f.cfg.Boot.Disks[0].PortableRootConfig, f.cfg.Boot.Disks[1].PortableRootConfig}
	for i, id := range []string{"root-1", "data-1", "data-2"} {
		store, err := diffstore.Create(filepath.Join(f.dir, "diffs", id+".diff"), diffstore.Options{DiffID: id, BaseID: DiffCheckpointBaseID(roots[i]), LogicalSize: 8192, Key: key})
		if err != nil {
			t.Fatal(err)
		}
		f.stores = append(f.stores, store)
		t.Cleanup(func() { _ = store.Close() })
		if _, err := store.WriteAt(bytes.Repeat([]byte{byte(i + 1)}, 4096), 0); err != nil {
			t.Fatal(err)
		}
		if _, err := store.WriteAt(make([]byte, 4096), 4096); err != nil {
			t.Fatal(err)
		}
		if err := store.Checkpoint(ctx, f.cp.CheckpointID); err != nil {
			t.Fatal(err)
		}
	}
	source, err := sandboxfile.BuildCheckpointSource(ctx, f.cfg, f.cp)
	if err != nil {
		t.Fatal(err)
	}
	var codec tarstream.Codec
	if key != nil {
		codec, err = manifestcrypto.NewTarStreamCodec(*key)
		if err != nil {
			t.Fatal(err)
		}
	}
	sink := snapshot.NewFileSink(f.dir, "fixture", codec, key != nil, nil)
	f.ref, f.path, err = sink.AbsorbSandbox(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSandbox(ctx, f.ref, f.path); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPublishCheckpointPreservesVersionsAndCleansExactSources(t *testing.T) {
	ctx := context.Background()
	f := newDiffCheckpointFixture(t)
	storage, _ := NewProcessStorage(plaintextRefConfig())
	defer storage.Close()
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range f.stores {
		if _, err := store.WriteAt(bytes.Repeat([]byte{9}, 4096), 0); err != nil {
			t.Fatal(err)
		}
		if err := store.Checkpoint(ctx, "later"); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	publisher, err := NewLocationPublisher(storage, "published", out, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := publisher.Publish(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.DeletePublishedDiffCheckpoints(ctx, result); err == nil {
		t.Fatal("cleanup before target Close succeeded")
	}
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.path)
	if !bytes.Equal(before, after) {
		t.Fatal("source artifact changed")
	}
	ref, _ := manifest.ParseRef(result.Ref)
	stream, err := storage.OpenFileWithLocations(ctx, filepath.Join(out, ref.Path), ref, config.RefLocations{"published": out})
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if root.Checkpoint != nil {
		t.Fatal("published checkpoint metadata survived")
	}
	buf := make([]byte, 8192)
	if _, err := root.Payload.ReadAt(ctx, buf, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf[:4096], bytes.Repeat([]byte{1}, 4096)) || !bytes.Equal(buf[4096:], make([]byte, 4096)) {
		t.Fatal("published root changed after active overwrite")
	}
	for i, disk := range root.Portable.Boot.Disks {
		r, _ := manifest.ParseRef(disk.Base)
		s, err := storage.OpenFileWithLocations(ctx, filepath.Join(out, r.Path), r, config.RefLocations{"published": out})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadAt(ctx, buf, 0); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		if !bytes.Equal(buf[:4096], bytes.Repeat([]byte{byte(i + 2)}, 4096)) {
			t.Fatal("published data disk mismatch")
		}
	}
	for _, store := range f.stores {
		ids, err := store.ListCheckpoints(ctx)
		if err != nil || len(ids) != 2 {
			t.Fatalf("default publication deleted versions: %v %v", ids, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeletePublishedDiffCheckpoints(ctx, result); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeletePublishedDiffCheckpoints(ctx, result); err != nil {
		t.Fatalf("retry: %v", err)
	}
	refs, err := storage.ListDiffCheckpoints(ctx, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("later versions lost: %v", refs)
	}
	for _, r := range refs {
		if r.ID != "later" {
			t.Fatal("wrong version deleted")
		}
	}
}

func TestPinnedCheckpointReopenAndMaterializeAfterDelete(t *testing.T) {
	ctx := context.Background()
	f := newDiffCheckpointFixture(t)
	storage, _ := NewProcessStorage(nil)
	defer storage.Close()
	pins, err := storage.PinDiffCheckpoints(ctx, f.dir, f.cp)
	if err != nil {
		t.Fatal(err)
	}
	defer pins.Close()
	ids, err := pins.ReopenIDs(f.dir, f.cfg)
	if err != nil || len(ids) != 3 {
		t.Fatalf("unchanged reopen: %v %v", ids, err)
	}
	if _, err := f.stores[0].WriteAt(bytes.Repeat([]byte{8}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	ids, err = pins.ReopenIDs(f.dir, f.cfg)
	if err != nil || ids != nil {
		t.Fatalf("changed active reopened: %v %v", ids, err)
	}
	for _, store := range f.stores {
		if err := store.DeleteCheckpoint(ctx, f.cp.CheckpointID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.PinDiffCheckpoints(ctx, f.dir, f.cp); !errors.Is(err, diffstore.ErrNotExist) {
		t.Fatalf("deleted version still visible: %v", err)
	}
	out := t.TempDir()
	result, err := storage.MaterializePinnedCheckpoint(ctx, f.path, nil, out, pins)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := manifest.ParseRef(result.Ref)
	stream, err := storage.OpenFileWithLocations(ctx, filepath.Join(out, ref.Path), ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	buf := make([]byte, 4096)
	if _, err := root.Payload.ReadAt(ctx, buf, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, bytes.Repeat([]byte{1}, 4096)) {
		t.Fatal("pinned deleted source was changed")
	}
}

func TestCheckpointRewriteReduceMaterializes(t *testing.T) {
	ctx := context.Background()
	f := newDiffCheckpointFixture(t)
	storage, _ := NewProcessStorage(plaintextRefConfig())
	defer storage.Close()
	out := t.TempDir()
	p, err := NewLocationPublisher(storage, "published", out, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.PublishWithOptions(ctx, f.path, RewriteOptions{ReduceAny: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	ref, _ := manifest.ParseRef(result.Ref)
	stream, err := storage.OpenFileWithLocations(ctx, filepath.Join(out, ref.Path), ref, config.RefLocations{"published": out})
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if root.Checkpoint != nil || root.Portable.IsCheckpoint() {
		t.Fatal("rewrite retained local checkpoint")
	}
}

func TestCheckpointReplacementUsesOriginalImmutableSelector(t *testing.T) {
	ctx := context.Background()
	lowerDir := t.TempDir()
	sink := snapshot.NewFileSink(lowerDir, "base", nil, false, nil)
	lower, path, err := sink.AbsorbOverlaySource(ctx, publishSource(t, bytes.Repeat([]byte{0x33}, 8192)))
	if err != nil {
		t.Fatal(err)
	}
	f := newDiffCheckpointFixture(t, lower)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, filepath.Base(path)), body, 0600); err != nil {
		t.Fatal(err)
	}
	replacement, _ := manifest.ParseRef(lower)
	replacement.Location = "shared"
	storage, _ := NewProcessStorage(plaintextRefConfig())
	defer storage.Close()
	out := t.TempDir()
	p, err := NewLocationPublisher(storage, "published", out, config.RefLocations{"shared": lowerDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.PublishWithOptions(ctx, f.path, RewriteOptions{Replacements: []RefReplacement{{Old: lower, New: replacement.String()}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	ref, _ := manifest.ParseRef(result.Ref)
	stream, err := storage.OpenFileWithLocations(ctx, filepath.Join(out, ref.Path), ref, config.RefLocations{"published": out})
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if root.Checkpoint != nil || len(root.Portable.Boot.Root.BaseFromRefs) != 1 || root.Portable.Boot.Root.BaseFromRefs[0] != replacement.String() {
		t.Fatalf("wrong replaced materialized graph: %+v", root.Portable.Boot.Root)
	}
	for _, raw := range result.RemovedRefs {
		if strings.Contains(raw, "checkpoint-temp-") {
			t.Fatalf("temporary source leaked into result: %s", raw)
		}
	}
}

func TestCleanupRetiredDiffStillValidatesFilenameIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "diffs"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "diffs", "wrong.diff")
	store, err := diffstore.Create(path, diffstore.Options{DiffID: "actual", LogicalSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Checkpoint(ctx, "cp"); err != nil {
		t.Fatal(err)
	}
	view, err := store.Export(ctx, "cp")
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if err := store.Retire(ctx); !errors.Is(err, diffstore.ErrBusy) {
		t.Fatalf("retire while pinned: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	err = cleanupDiffVersionSet(ctx, dir, nil)
	if err == nil || !strings.Contains(err.Error(), "identity differs") {
		t.Fatalf("wrong retired identity accepted: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("mismatched DIFF removed: %v", err)
	}
}

func TestCheckpointPinsEnforceExplicitLocalRequired(t *testing.T) {
	f := newDiffCheckpointFixture(t)
	key := [32]byte{0x54}
	codec, err := manifestcrypto.NewTarStreamCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewProcessStorageWithLocalAccess(nil, func() ([32]byte, error) { return key, nil }, codec, true)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.PinDiffCheckpoints(context.Background(), f.dir, f.cp); !errors.Is(err, diffstore.ErrPlaintextForbidden) {
		t.Fatalf("plaintext checkpoint accepted by required policy: %v", err)
	}
}

// The cancellation check is a deterministic execution boundary after directory
// ownership but before the exact version mutation; it does not cancel the call.
type checkpointDirectorySwapContext struct {
	context.Context
	once sync.Once
	swap func()
}

func (c *checkpointDirectorySwapContext) Err() error {
	c.once.Do(c.swap)
	return c.Context.Err()
}

func TestPublishedCheckpointCleanupPinsDirectoryAcrossRename(t *testing.T) {
	parent := t.TempDir()
	sourcePath, replacement, renamed := filepath.Join(parent, "source"), filepath.Join(parent, "replacement"), filepath.Join(parent, "renamed")
	for _, directory := range []string{sourcePath, replacement} {
		if err := os.MkdirAll(filepath.Join(directory, "diffs"), 0700); err != nil {
			t.Fatal(err)
		}
		store, err := diffstore.Create(filepath.Join(directory, "diffs", "root.diff"), diffstore.Options{DiffID: "root", LogicalSize: 4096})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Checkpoint(context.Background(), "cp"); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	result := PublishResult{Ref: "published", completion: &publishCompletion{closed: true}, checkpointSources: []checkpointPublication{{directory: sourcePath, identity: identity, refs: []DiffCheckpointRef{{DiffID: "root", ID: "cp"}}}}}
	ctx := &checkpointDirectorySwapContext{Context: context.Background(), swap: func() {
		if err := os.Rename(sourcePath, renamed); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, sourcePath); err != nil {
			t.Fatal(err)
		}
	}}
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if err := storage.DeletePublishedDiffCheckpoints(ctx, result); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{sourcePath, renamed} {
		store, err := diffstore.OpenMetadata(filepath.Join(directory, "diffs", "root.diff"))
		if err != nil {
			t.Fatal(err)
		}
		ids, err := store.ListCheckpoints(context.Background())
		if err := errors.Join(err, store.Close()); err != nil {
			t.Fatal(err)
		}
		if directory == sourcePath && (len(ids) != 1 || ids[0] != "cp") {
			t.Fatalf("replacement was mutated: %v", ids)
		}
		if directory == renamed && len(ids) != 0 {
			t.Fatalf("selected source was not cleaned: %v", ids)
		}
	}
}

func TestPublishedCheckpointRouteRetriesPinnedDirectory(t *testing.T) {
	parent := t.TempDir()
	sourcePath, replacement, renamed := filepath.Join(parent, "source"), filepath.Join(parent, "replacement"), filepath.Join(parent, "renamed")
	owner, err := diffstore.LockDirectory(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := os.MkdirAll(replacement, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(parent, "owner.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	route, err := ctl.PublishCheckpointOwner(sourcePath, socket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.PublishCheckpointOwner(replacement, filepath.Join(parent, "wrong.sock")); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	result := PublishResult{Ref: "published", completion: &publishCompletion{closed: true}, checkpointSources: []checkpointPublication{{directory: sourcePath, identity: identity, refs: []DiffCheckpointRef{{DiffID: "root", ID: "cp"}}}}}
	served := make(chan error, 1)
	go func() {
		for attempt := 0; attempt < 2; attempt++ {
			conn, err := listener.Accept()
			if err != nil {
				served <- err
				return
			}
			var req ctl.Request
			if err = ctl.ReadMessage(conn, &req); err != nil {
				conn.Close()
				served <- err
				return
			}
			if req.CheckpointOwner != route.ID || len(req.CheckpointRefs) != 1 || req.CheckpointRefs[0].DiffID != "root" || req.CheckpointRefs[0].ID != "cp" {
				conn.Close()
				served <- errors.New("routing changed exact owner or source set")
				return
			}
			resp := ctl.Response{Type: ctl.TypeCheckpointDeleteDone}
			if attempt == 0 {
				if err = os.Rename(sourcePath, renamed); err == nil {
					err = os.Rename(replacement, sourcePath)
				}
				if err != nil {
					conn.Close()
					served <- err
					return
				}
				resp = ctl.Response{Type: ctl.TypeError, Msg: "checkpoint owner changed"}
			}
			err = ctl.WriteMessage(conn, resp)
			conn.Close()
			if err != nil {
				served <- err
				return
			}
		}
		served <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if err := storage.DeletePublishedDiffCheckpoints(ctx, result); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"golang.org/x/sys/unix"
)

const localMemorySize = 6 * 4096

type memoryCheckpointFixture struct {
	directory, path, ref, eRef, external string
	descriptor                           memorycheckpoint.Descriptor
	storage                              *ProcessStorage
}

func checkpointPageBytes(values string) []byte {
	out := make([]byte, len(values)*4096)
	for i, value := range []byte(values) {
		copy(out[i*4096:], bytes.Repeat([]byte{value}, 4096))
	}
	return out
}

func writeLocalMemory(t *testing.T, directory, id, eRef string, parents, suffix []string, values string, materialized, dirty, released byte, workingSet bool) (string, string, memorycheckpoint.Descriptor) {
	t.Helper()
	// The real producer establishes GC protection before publishing a record.
	pin, err := memorycheckpoint.PinDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	ctx := context.Background()
	sink := snapshot.NewFileSink(directory, "sid", nil, false, nil)
	defer sink.Close()
	view := &memory.CaptureView{Size: localMemorySize, Materialized: []byte{materialized}, Dirty: []byte{dirty}, Released: []byte{released}}
	encoded, err := memorycheckpoint.EncodeMap(view)
	if err != nil {
		t.Fatal(err)
	}
	mapRef, _, err := sink.AbsorbImageSource(ctx, sparse.Dense(bytes.NewReader(encoded), uint64(len(encoded))))
	if err != nil {
		t.Fatal(err)
	}
	top, _, err := snapshot.CaptureMemory(bytes.NewReader(checkpointPageBytes(values)), view, workingSet)
	if err != nil {
		t.Fatal(err)
	}
	payloadRef, _, err := sink.AbsorbImageSource(ctx, top)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := memorycheckpoint.Descriptor{Version: memorycheckpoint.Version, MemoryID: "memory-" + id,
		CheckpointID: id, Size: localMemorySize, MapRef: mapRef, Parents: parents, WorkingSet: workingSet}
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: eRef, FromRefs: suffix})
	if err != nil {
		t.Fatal(err)
	}
	source, err := snapshotfile.BuildLocalSource([]byte("{}"), []byte("{}"), cfg, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	ref, path, err := sink.AbsorbSnapshot(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	start, err := memorycheckpoint.ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	record := memorycheckpoint.CaptureRecord{Version: memorycheckpoint.Version,
		Producer: memorycheckpoint.RunIdentity{SandboxID: "sid", InstanceID: "instance", RunID: "run-" + id}, CheckpointID: id,
		MemoryID: descriptor.MemoryID, Epoch: 1, SnapshotRef: ref, SandboxRef: eRef, PayloadRef: payloadRef,
		Committed: true, RuntimeRetired: true, FileReady: true, RAMReleased: true,
		Holder: fixtureMemoryHolder(t, directory, id, descriptor.MemoryID, start)}
	writer, err := memorycheckpoint.CreateRecord(directory, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return ref, path, descriptor
}

func newMemoryCheckpointFixture(t *testing.T) memoryCheckpointFixture {
	t.Helper()
	ctx := context.Background()
	directory := t.TempDir()
	storage, err := NewProcessStorage(plaintextRefConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	sink := snapshot.NewFileSink(directory, "sid", nil, false, nil)
	_, ecfg := publishPortable(t, "")
	eRef := rewriteSaveE(t, sink, ecfg, rewriteSparse(t, make([]byte, localMemorySize)))
	external := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: eRef}, rewriteSparse(t, checkpointPageBytes("xxxxxx")))
	// The legacy parent contains clean copies at pages 1 and 4. Only page 0
	// and released page 2 belong to dirty historical H.
	legacyView := &memory.CaptureView{Size: localMemorySize, Materialized: []byte{0x13}, Dirty: []byte{1}, Released: []byte{4}}
	legacy, bits, err := snapshot.CaptureMemory(bytes.NewReader(checkpointPageBytes("bxxxxy")), legacyView, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: eRef, FromRefs: []string{external}})
	if err != nil {
		t.Fatal(err)
	}
	legacySource, err := snapshotfile.BuildSourceWithDirty(legacy, []byte("{}"), []byte("{}"), cfg, bits)
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := sink.AbsorbSnapshot(ctx, legacySource)
	if err != nil {
		t.Fatal(err)
	}
	ref, path, descriptor := writeLocalMemory(t, directory, "one", eRef, []string{parent}, []string{external}, "bcxxxx", 0x2b, 2, 0x10, true)
	return memoryCheckpointFixture{directory: directory, path: path, ref: ref, eRef: eRef, external: external, descriptor: descriptor, storage: storage}
}

func openLocalFixture(t *testing.T, fixture memoryCheckpointFixture) *snapshotfile.Root {
	t.Helper()
	ref, err := manifest.ParseRef(fixture.ref)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := fixture.storage.OpenFile(context.Background(), fixture.path, ref)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fixture.storage.OpenSnapshot(context.Background(), stream, SnapshotOpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func assertMemoryPages(t *testing.T, source sparse.Source, kinds string, values string) {
	t.Helper()
	for page, kind := range []byte(kinds) {
		run, err := source.RunAt(uint64(page*4096), 4096)
		if err != nil {
			t.Fatal(err)
		}
		want := map[byte]sparse.RunKind{'H': sparse.Hole, 'Z': sparse.Zero, 'D': sparse.Data}[kind]
		if run.Kind() != want {
			t.Fatalf("page %d kind %v, want %v", page, run.Kind(), want)
		}
		if kind != 'H' {
			var one [1]byte
			n, err := run.ReadAt(context.Background(), one[:], 0)
			if n != 1 || err != nil && err != io.EOF || one[0] != values[page] {
				t.Fatalf("page %d read %d/%v/%x, want %x", page, n, err, one[0], values[page])
			}
		}
	}
}

func TestMemoryCheckpointOpenSingleViewPreservesDirtyAndReleased(t *testing.T) {
	fixture := newMemoryCheckpointFixture(t)
	root := openLocalFixture(t, fixture)
	defer root.Close()
	if root.ArchiveBase != 0 || root.Memory.Size() != localMemorySize || root.LocalCheckpoint == nil {
		t.Fatal("local descriptor did not resolve into its logical RAM geometry")
	}
	assertMemoryPages(t, root.Memory, "DDZDZD", "bc\x00x\x00x")
	assertMemoryPages(t, root.LocalCheckpoint.History, "DHZHHH", "b\x00\x00\x00\x00\x00")
	dirty, err := SnapshotDirtyMemory(root)
	if err != nil {
		t.Fatal(err)
	}
	assertMemoryPages(t, dirty, "DDZHZH", "bc\x00\x00\x00\x00")
	if err := root.Memory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.FullStream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCheckpointPortablePublicationKeepsWorkingSetAboveDirtyHistory(t *testing.T) {
	for _, mode := range []string{"publish", "rewrite", "reduce"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newMemoryCheckpointFixture(t)
			publisher := rewriteTarget(t, fixture.storage, t.TempDir())
			var opts RewriteOptions
			if mode == "rewrite" {
				ref, err := manifest.ParseRef(fixture.external)
				if err != nil {
					t.Fatal(err)
				}
				replacement := ref
				replacement.Path = filepath.Join(fixture.directory, ref.Path)
				opts, err = ParseRewriteOptions([]string{fixture.external + "=" + replacement.String()}, nil, false)
				if err != nil {
					t.Fatal(err)
				}
			} else if mode == "reduce" {
				opts.ReduceAny = true
			}
			result, err := publisher.PublishWithOptions(context.Background(), fixture.path, opts)
			if err != nil {
				t.Fatal(err)
			}
			root, err := snapshotfile.Open(context.Background(), rewriteOpen(t, publisher, result.Ref))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "reduce" {
				assertMemoryPages(t, root.Memory, "DDZDZD", "bc\x00x\x00x")
				if len(cfg.FromRefs) != 0 || root.MemoryDirty != nil {
					t.Fatal("reduced view retained a parent or clean annotation")
				}
				return
			}
			assertMemoryPages(t, root.Memory, "DDHDZD", "bc\x00x\x00x")
			if len(cfg.FromRefs) != 2 || len(root.MemoryDirty) != snapshotfile.DirtyHeaderSize+1 || root.MemoryDirty[snapshotfile.DirtyHeaderSize] != 0x12 {
				t.Fatalf("W provenance changed: refs=%v dirty=%x", cfg.FromRefs, root.MemoryDirty)
			}
			history, err := snapshotfile.Open(context.Background(), rewriteOpen(t, publisher, cfg.FromRefs[0]))
			if err != nil {
				t.Fatal(err)
			}
			defer history.Close()
			assertMemoryPages(t, history.Memory, "DHZHHH", "b\x00\x00\x00\x00\x00")
			if history.MemoryDirty != nil {
				t.Fatal("materialized history is not all dirty")
			}
		})
	}
}

func TestMemoryCheckpointNextDirtyCaptureDropsPreviousCleanWorkingSetCopies(t *testing.T) {
	fixture := newMemoryCheckpointFixture(t)
	fixture.ref, fixture.path, fixture.descriptor = writeLocalMemory(t, fixture.directory, "two", fixture.eRef,
		[]string{fixture.ref}, []string{fixture.external}, "bxdxxe", 0x25, 0x24, 0, false)
	root := openLocalFixture(t, fixture)
	assertMemoryPages(t, root.Memory, "DDDHZD", "bcd\x00\x00e")
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	publisher := rewriteTarget(t, fixture.storage, t.TempDir())
	result, err := publisher.Publish(context.Background(), fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	portable, err := snapshotfile.Open(context.Background(), rewriteOpen(t, publisher, result.Ref))
	if err != nil {
		t.Fatal(err)
	}
	defer portable.Close()
	assertMemoryPages(t, portable.Memory, "DDDHZD", "bcd\x00\x00e")
	cfg, err := snapshot.ParseConfig(portable.SnapshotConfig)
	if err != nil || len(cfg.FromRefs) != 1 || portable.MemoryDirty != nil {
		t.Fatalf("incremental output changed its external suffix: %+v, %v", cfg, err)
	}
}

func TestMemoryCheckpointKeepGraphAndReaderPin(t *testing.T) {
	fixture := newMemoryCheckpointFixture(t)
	root := openLocalFixture(t, fixture)
	c := Checkpoint{Directory: fixture.directory, SandboxID: "sid", SnapshotRef: fixture.ref, SandboxRef: fixture.eRef}
	garbage := filepath.Join(fixture.directory, strings.Repeat("f", 64)+".image")
	if err := os.WriteFile(garbage, []byte("obsolete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.CleanupCheckpoint(context.Background(), c); !errors.Is(err, ErrCheckpointMemoryReadersBusy) || !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("cleanup while memory reader is pinned: %v", err)
	}
	if _, err := os.Stat(garbage); err != nil {
		t.Fatalf("busy cleanup deleted data: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.CleanupCheckpoint(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(garbage); !os.IsNotExist(err) {
		t.Fatalf("unreferenced artifact survived: %v", err)
	}
	// The ordinary keep plan used to delete the hidden map, payload and parent.
	root = openLocalFixture(t, fixture)
	assertMemoryPages(t, root.Memory, "DDZDZD", "bc\x00x\x00x")
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	// A selected older/E-only source must still retain the latest pending/current
	// sandbox obligation's graph. No memory socket exists in this fixture.
	c.SnapshotRef = ""
	if err := fixture.storage.CleanupCheckpoint(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	root = openLocalFixture(t, fixture)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCheckpointKeepsRecoveryRecordsWithOriginalCheckpointID(t *testing.T) {
	fixture := newMemoryCheckpointFixture(t)
	start, err := memorycheckpoint.ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	writeAborted := func(id, memoryID string) string {
		t.Helper()
		writer, err := memorycheckpoint.CreateRecord(fixture.directory, memorycheckpoint.CaptureRecord{
			Version: memorycheckpoint.Version, Producer: memorycheckpoint.RunIdentity{SandboxID: "sid"},
			CheckpointID: id, MemoryID: memoryID, Epoch: 1, Aborted: true,
			Holder: fixtureMemoryHolder(t, fixture.directory, id, memoryID, start),
		})
		if err != nil {
			t.Fatal(err)
		}
		path := writer.Path()
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	obsolete := writeAborted("旧检查点/第一轮", "old-memory")
	current := writeAborted("当前检查点/第二轮", "current-memory")
	// A similarly named file is not evidence of a valid capture record identity.
	unknown := filepath.Join(fixture.directory, "memory", "unknown.json")
	b, err := os.ReadFile(obsolete)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown+".lock", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.CleanupCheckpoint(context.Background(), Checkpoint{
		Directory: fixture.directory, SandboxID: "sid", SnapshotRef: fixture.ref, SandboxRef: fixture.eRef,
	}); err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{obsolete, current, unknown, filepath.Dir(obsolete) + ".json.lock", obsolete + ".ram.lock"} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("retained/unknown/stable-lock file was deleted: %s: %v", kept, err)
		}
	}
	root := openLocalFixture(t, fixture)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCheckpointGCKeepsReleasedProofOutsideSourceGraph(t *testing.T) {
	fixture := newMemoryCheckpointFixture(t)
	_, orphanPath, orphan := writeLocalMemory(t, fixture.directory, "released-awaiting-resource-ack", fixture.eRef, nil, nil, "zzzzzz", 0x3f, 0x3f, 0, false)
	old, err := memorycheckpoint.ReadRecord(fixture.directory, orphan.CheckpointID)
	if err != nil {
		t.Fatal(err)
	}
	oldPath, err := memorycheckpoint.RecordPath(fixture.directory, old.CheckpointID, old.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(oldPath + ".ram.lock")
	if err != nil {
		t.Fatal(err)
	}
	// The prior RAMReleased fact may still await receipt settlement. A new
	// preparation replaces current-memory.json but then aborts, and the chosen
	// intact source does not reference the prior capture at all.
	next, err := memorycheckpoint.CreateRecord(fixture.directory, memorycheckpoint.CaptureRecord{
		Version: memorycheckpoint.Version, Producer: old.Producer,
		CheckpointID: "new-aborted", MemoryID: "new-memory", Epoch: 1, Aborted: true,
		Holder: fixtureMemoryHolder(t, fixture.directory, "new-aborted", "new-memory", old.Holder.StartTime),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.CleanupCheckpoint(context.Background(), Checkpoint{Directory: fixture.directory, SandboxID: "sid", SnapshotRef: fixture.ref, SandboxRef: fixture.eRef}); err != nil {
		t.Fatal(err)
	}
	proof, err := memorycheckpoint.ReadCaptureRecord(oldPath)
	if err != nil || *proof != *old || !proof.RAMReleased {
		t.Fatalf("GC erased exact unsettled release evidence: %+v %v", proof, err)
	}
	if current, err := memorycheckpoint.ReadCurrent(fixture.directory); err != nil || !current.Aborted || current.MemoryID != "new-memory" {
		t.Fatalf("new abort evidence=%+v %v", current, err)
	}
	after, err := os.Stat(oldPath + ".ram.lock")
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("GC replaced the original RAM proof inode: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(oldPath) + ".json.lock"); err != nil {
		t.Fatalf("writer proof removed: %v", err)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced descriptor carrier was kept by metadata retention: %v", err)
	}
	payload, err := manifest.ParseRef(old.PayloadRef)
	if err != nil {
		t.Fatal(err)
	}
	payloadPath := payload.Path
	if !filepath.IsAbs(payloadPath) {
		payloadPath = filepath.Join(fixture.directory, payloadPath)
	}
	if _, err := os.Stat(payloadPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced payload was kept by metadata retention: %v", err)
	}
}

func TestMemoryCheckpointAsMemoryLowerKeepsOnePortableSlot(t *testing.T) {
	for _, method := range []string{"publish", "rewrite", "located-publish", "located-rewrite", "located-historical-publish"} {
		t.Run(method, func(t *testing.T) {
			rewrite := strings.Contains(method, "rewrite")
			located := strings.HasPrefix(method, "located-")
			fixture := newMemoryCheckpointFixture(t)
			if method == "located-historical-publish" {
				previous, err := memorycheckpoint.ReadCurrent(fixture.directory)
				if err != nil {
					t.Fatal(err)
				}
				writer, err := memorycheckpoint.CreateRecord(fixture.directory, memorycheckpoint.CaptureRecord{
					Version: memorycheckpoint.Version, Producer: previous.Producer,
					CheckpointID: "later-aborted", MemoryID: "later-memory", Epoch: 1, Aborted: true, Holder: previous.Holder,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			sink := snapshot.NewFileSink(fixture.directory, "sid", nil, false, nil)
			view := &memory.CaptureView{Size: localMemorySize, Materialized: []byte{0x20}, Dirty: []byte{0x20}, Released: []byte{0}}
			memory, _, err := snapshot.CaptureMemory(bytes.NewReader(checkpointPageBytes("xxxxxq")), view, false)
			if err != nil {
				t.Fatal(err)
			}
			parentRef := fixture.ref
			if located {
				ref, err := manifest.ParseRef(parentRef)
				if err != nil {
					t.Fatal(err)
				}
				ref.Location = "memory-source"
				parentRef = ref.String()
			}
			raw := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: fixture.eRef,
				FromRefs: []string{parentRef, fixture.external}}, memory)
			parsed, err := manifest.ParseRef(raw)
			if err != nil {
				t.Fatal(err)
			}
			publisher := rewriteTarget(t, fixture.storage, t.TempDir())
			if located {
				publisher.locations["memory-source"] = fixture.directory
			}
			var opts RewriteOptions
			if rewrite {
				ref, err := manifest.ParseRef(fixture.external)
				if err != nil {
					t.Fatal(err)
				}
				replacement := ref
				replacement.Path = filepath.Join(fixture.directory, ref.Path)
				opts, err = ParseRewriteOptions([]string{fixture.external + "=" + replacement.String()}, nil, false)
				if err != nil {
					t.Fatal(err)
				}
			}
			result, err := publisher.PublishWithOptions(ctx, filepath.Join(fixture.directory, parsed.Path), opts)
			if err != nil {
				t.Fatal(err)
			}
			root, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
			if err != nil || len(cfg.FromRefs) != 2 {
				t.Fatalf("portable lower graph changed the outer reference budget: %+v, %v", cfg, err)
			}
			var sources = []sparse.Source{root.Memory}
			for i, ref := range cfg.FromRefs {
				parent, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, ref))
				if err != nil {
					t.Fatal(err)
				}
				defer parent.Close()
				if i == 0 && (len(parent.MemoryDirty) != snapshotfile.DirtyHeaderSize+1 || parent.MemoryDirty[snapshotfile.DirtyHeaderSize] != 0x17) {
					t.Fatalf("portable lower lost the top/history dirty union: %x", parent.MemoryDirty)
				}
				sources = append(sources, parent.Memory)
			}
			assertMemoryPages(t, layerMemorySources(sources...), "DDZDZD", "bc\x00x\x00q")
		})
	}
}

func TestMemoryCheckpointUnprovedParentSuffixKeepsExplicitCleanCopies(t *testing.T) {
	fixture := newMemoryCheckpointFixture(t)
	sink := snapshot.NewFileSink(fixture.directory, "sid", nil, false, nil)
	different := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: fixture.eRef}, rewriteSparse(t, checkpointPageBytes("yyyyyy")))
	fixture.ref, fixture.path, fixture.descriptor = writeLocalMemory(t, fixture.directory, "unproved", fixture.eRef,
		fixture.descriptor.Parents, []string{different}, "yyyyyy", 0, 0, 0, false)
	root := openLocalFixture(t, fixture)
	defer root.Close()
	assertMemoryPages(t, root.Memory, "DDZHDH", "bx\x00\x00x\x00")
}

func TestMemoryCheckpointInfoAndDiskTableDoNotOpenMemory(t *testing.T) {
	fixture := newMemoryCheckpointFixture(t)
	mapRef, err := manifest.ParseRef(fixture.descriptor.MapRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.directory, mapRef.Path)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(fixture.directory, "memory")); err != nil {
		t.Fatal(err)
	}
	info, err := fixture.storage.Inspect(context.Background(), fixture.path, nil)
	if err != nil || info.Role != RoleSnapshot || info.Snapshot.SandboxRef != fixture.eRef {
		t.Fatalf("metadata inspection depended on memory backing: %+v, %v", info, err)
	}
	publisher := newPublisher(fixture.storage, nil, nil, nil)
	called := false
	err = publisher.withDiskTable(context.Background(), ExportDiskRequest{SourceRef: fixture.path},
		func(portable *config.PortableSandboxConfig, _ *sandboxfile.Checkpoint, _ publishScope) error {
			called = portable != nil
			return nil
		})
	if err != nil || !called {
		t.Fatalf("disk-only source selection depended on memory backing: called=%v, err=%v", called, err)
	}
}

func TestMemoryCheckpointPortableSingleLayerKeepsDirtyUnion(t *testing.T) {
	fixture := newMemoryCheckpointFixture(t)
	root := openLocalFixture(t, fixture)
	stream, err := PortableMemoryLayer(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	portable, err := snapshotfile.Open(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	defer portable.Close()
	assertMemoryPages(t, portable.Memory, "DDZDZD", "bc\x00x\x00x")
	if len(portable.MemoryDirty) != snapshotfile.DirtyHeaderSize+1 || portable.MemoryDirty[snapshotfile.DirtyHeaderSize] != 0x17 {
		t.Fatalf("single-layer dirty union lost Top/History provenance: %x", portable.MemoryDirty)
	}
	// Close propagates to all imported local readers, releasing the directory pin.
	if err := portable.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.CleanupCheckpoint(context.Background(), Checkpoint{
		Directory: fixture.directory, SandboxID: "sid", SnapshotRef: fixture.ref, SandboxRef: fixture.eRef,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCheckpointPortableDirtyLegacyByteRangesStayConservative(t *testing.T) {
	values, err := sparse.NewSource(bytes.NewReader(make([]byte, 8192)), 8192, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := sparse.NewSource(bytes.NewReader(make([]byte, 8192)), 8192,
		[]sparse.Extent{{Offset: 0, Size: 11}, {Offset: 23, Size: 8192 - 23}})
	if err != nil {
		t.Fatal(err)
	}
	bits, err := portableMemoryDirty(context.Background(), values, candidate)
	if err != nil || bits != nil {
		t.Fatalf("legacy partial dirty coverage was expanded to page bits: %x, %v", bits, err)
	}
}

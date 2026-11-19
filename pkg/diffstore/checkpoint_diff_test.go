package diffstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/versioned"
)

func checkpointCOW(t *testing.T, path string, base BlockReader, size int64, encrypted bool, extra ...DiskOption) *Disk {
	t.Helper()
	options := []DiskOption{WithCheckpoint("disk-under-test", "immutable-base")}
	if encrypted {
		options = append(options, WithDiffEncryption([32]byte{1, 2, 3}, true))
	}
	options = append(options, extra...)
	cow, err := OpenDisk(path, base, DiffInit{CreateSize: size}, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cow.Close() })
	return cow
}

func readExport(t *testing.T, view ExportView) []byte {
	t.Helper()
	result := make([]byte, view.Size())
	n, err := view.ReadAt(context.Background(), result, 0)
	if err != nil || n != len(result) {
		t.Fatalf("read export: %d, %v", n, err)
	}
	return result
}

func TestCheckpointCOWProtectsDirtyVersionAndPinnedDelete(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "xts"}[encrypted], func(t *testing.T) {
			const size = 8 * cowBlockSize
			base := &fakeReader{data: bytes.Repeat([]byte{0x61}, size)}
			path := filepath.Join(t.TempDir(), "active.diff")
			cow := checkpointCOW(t, path, base, size, encrypted)
			// A partial write must preserve the base and an allocated all-zero page
			// must mask it; absent block 1 remains an authoritative Hole.
			if n, err := cow.WriteAt([]byte{9, 8, 7}, 19); err != nil || n != 3 {
				t.Fatal(n, err)
			}
			if n, err := cow.WriteAt(make([]byte, cowBlockSize), 2*cowBlockSize); err != nil || n != cowBlockSize {
				t.Fatal(n, err)
			}
			if err := cow.Checkpoint(context.Background(), "first"); err != nil {
				t.Fatal(err)
			}
			if !cow.CanResumeCheckpoint("first") {
				t.Fatal("new checkpoint cannot resume")
			}
			view, err := cow.Export(context.Background(), "first")
			if err != nil {
				t.Fatal(err)
			}
			defer view.Close()
			want := make([]byte, size)
			copy(want[:cowBlockSize], base.data[:cowBlockSize])
			copy(want[19:], []byte{9, 8, 7})
			if got := readExport(t, view); !bytes.Equal(got, want) {
				t.Fatal("wrong captured dirty content")
			}
			for block, kind := range map[uint64]sparse.RunKind{0: sparse.Data, 1: sparse.Hole, 2: sparse.Data} {
				run, err := view.RunAt(block*cowBlockSize, cowBlockSize)
				if err != nil || run.Kind() != kind {
					t.Fatalf("block %d kind: %v %v", block, run, err)
				}
			}
			if err := cow.DeleteCheckpoint(context.Background(), "first"); err != nil {
				t.Fatal(err)
			}
			if _, err := cow.Export(context.Background(), "first"); !errors.Is(err, versioned.ErrNotExist) {
				t.Fatalf("deleted version reopened: %v", err)
			}
			if err := cow.DeleteCheckpoint(context.Background(), "first"); err != nil {
				t.Fatal("retry", err)
			}
			if err := cow.Checkpoint(context.Background(), "first"); !errors.Is(err, versioned.ErrExists) {
				t.Fatalf("ID reused: %v", err)
			}
			// A withdrawn version remains physically protected for its old reader.
			if _, err := cow.WriteAt(bytes.Repeat([]byte{0xb4}, cowBlockSize), 0); err != nil {
				t.Fatal(err)
			}
			if err := cow.Discard(2*cowBlockSize, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			if err := cow.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := readExport(t, view); !bytes.Equal(got, want) {
				t.Fatal("delete released CoW protection before reader closed")
			}
			if err := cow.Close(); err != nil {
				t.Fatal(err)
			}
			if got := readExport(t, view); !bytes.Equal(got, want) {
				t.Fatal("view did not survive owner close")
			}
		})
	}
}

func TestCheckpointCOWReopenPresenceCapacityAndIdentity(t *testing.T) {
	const size = 512 * cowBlockSize
	path := filepath.Join(t.TempDir(), "active.diff")
	cow := checkpointCOW(t, path, nil, size, false)
	for _, block := range []int64{0, 255, 256, 511} {
		if _, err := cow.WriteAt(bytes.Repeat([]byte{byte(block + 1)}, cowBlockSize), block*cowBlockSize); err != nil {
			t.Fatal(err)
		}
	}
	if err := cow.Checkpoint(context.Background(), "reopen"); err != nil {
		t.Fatal(err)
	}
	if err := cow.Close(); err != nil {
		t.Fatal(err)
	}
	if capacity, err := ExistingDiffCapacity(path); err != nil || capacity != size {
		t.Fatalf("logical capacity: %d %v", capacity, err)
	}
	if _, err := OpenDisk(path, nil, DiffInit{Existing: true}, WithCheckpoint("wrong-disk", "immutable-base")); err == nil {
		t.Fatal("accepted wrong identity")
	}
	if _, err := OpenDisk(path, nil, DiffInit{Existing: true}, WithCheckpoint("disk-under-test", "wrong-base")); err == nil {
		t.Fatal("accepted wrong base")
	}
	reopened, err := OpenDisk(path, nil, DiffInit{Existing: true, CreateSize: size}, WithCheckpoint("disk-under-test", "immutable-base"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.DirtyCount() != 4 || reopened.BackendStats()["diff_presence_complete"] != true {
		t.Fatal("reopen did not load complete presence", reopened.BackendStats())
	}
	if !reopened.CanResumeCheckpoint("reopen") {
		t.Fatal("continuation marker lost")
	}
	for _, block := range []int64{0, 255, 256, 511} {
		got := make([]byte, cowBlockSize)
		if _, err := reopened.ReadAt(got, block*cowBlockSize); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{byte(block + 1)}, cowBlockSize)) {
			t.Fatalf("wrong block %d", block)
		}
	}
	ids, err := reopened.ListCheckpoints(context.Background())
	if err != nil || len(ids) != 1 || ids[0] != "reopen" {
		t.Fatal(ids, err)
	}
}

func TestCheckpointCOWInvalidatesBeforeCacheMutation(t *testing.T) {
	const size = 2 * cowBlockSize
	gate := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	cache, err := newCache(8*cowBlockSize, 4*cowBlockSize, cacheHooks{beforeWrite: func([]*cachePage) error { once.Do(func() { close(blocked) }); <-gate; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	path := filepath.Join(t.TempDir(), "active.diff")
	cow := checkpointCOW(t, path, nil, size, false, WithCache(cache))
	defer cow.Close()
	if err := cow.Checkpoint(context.Background(), "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := cow.WriteAt([]byte{1}, 1); err != nil {
		t.Fatal(err)
	}
	if cow.CanResumeCheckpoint("empty") {
		t.Fatal("checkpoint marker survives accepted cache write")
	}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		close(gate)
		t.Fatal("writeback did not start")
	}
	// While disk writeback is blocked, the cache must already expose the write.
	got := make([]byte, 1)
	if _, err := cow.ReadAt(got, 1); err != nil || got[0] != 1 {
		close(gate)
		t.Fatal(got, err)
	}
	close(gate)
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointCOWPrivateOverwriteSkipsIndexRedo(t *testing.T) {
	const size = 32 * cowBlockSize
	cow := checkpointCOW(t, filepath.Join(t.TempDir(), "active.diff"), nil, size, false)
	ctx := context.Background()
	page := bytes.Repeat([]byte{1}, cowBlockSize)
	if _, err := cow.WriteAt(page, 0); err != nil {
		t.Fatal(err)
	}
	if err := cow.Checkpoint(ctx, "cp"); err != nil {
		t.Fatal(err)
	}
	page[0] = 2
	if _, err := cow.WriteAt(page, 0); err != nil {
		t.Fatal(err)
	}
	if err := cow.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ := cow.checkpointBackend()
	before := d.store.Stats()
	for i := 0; i < 8; i++ {
		page[0]++
		if _, err := cow.WriteAt(page, 0); err != nil {
			t.Fatal(err)
		}
		if err := cow.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	after := d.store.Stats()
	if before.IndexWritten != after.IndexWritten || before.RedoWritten != after.RedoWritten || before.Allocated != after.Allocated {
		t.Fatalf("private overwrite mutated metadata: before=%+v after=%+v", before, after)
	}
}

func TestCheckpointCOWActiveExportMatchesSnapshotView(t *testing.T) {
	cow := checkpointCOW(t, filepath.Join(t.TempDir(), "active.diff"), nil, 8*cowBlockSize, false)
	if _, err := cow.WriteAt([]byte("pending"), 3*cowBlockSize+17); err != nil {
		t.Fatal(err)
	}
	export, err := cow.Export(context.Background(), ActiveDiff)
	if err != nil {
		t.Fatal(err)
	}
	want := readExport(t, export)
	legacy, holes, err := cow.SnapshotView()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(legacy)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("legacy view differs", err)
	}
	if len(holes) != 2 {
		t.Fatalf("holes=%v", holes)
	}
	if err := export.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := export.ReadAt(context.Background(), make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed export read=%v", err)
	}
}

func TestCheckpointCOWLegacyRefusalAndDefaultExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.diff")
	legacy, err := OpenDisk(path, nil, DiffInit{CreateSize: 8 * cowBlockSize})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Checkpoint(context.Background(), "cp"); !errors.Is(err, ErrCheckpointUnsupported) {
		t.Fatal(err)
	}
	view, err := legacy.Export(context.Background(), ActiveDiff)
	if err != nil {
		t.Fatal(err)
	}
	_ = view.Close()
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDisk(path, nil, DiffInit{Existing: true}, WithCheckpoint("id", "")); !errors.Is(err, versioned.ErrFormat) {
		t.Fatalf("adopted legacy active file: %v", err)
	}
}

func TestCheckpointCOWBatchesNoncontiguousOldestPages(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cache, err := newCache(16*cowBlockSize, 12*cowBlockSize, cacheHooks{beforeSelect: func() { once.Do(func() { close(entered) }); <-release }})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	cow := checkpointCOW(t, filepath.Join(t.TempDir(), "active.diff"), nil, 64*cowBlockSize, false, WithCache(cache))
	defer cow.Close()
	for _, block := range []int64{31, 2, 19, 0, 50, 12} {
		if _, err := cow.WriteAt(bytes.Repeat([]byte{byte(block)}, cowBlockSize), block*cowBlockSize); err != nil {
			close(release)
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("worker did not select")
	}
	close(release)
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := cache.Stats()
	if stats.WriteBatches != 1 {
		t.Fatalf("non-contiguous metadata batch fragmented: %+v", stats)
	}
}

func TestCheckpointCOWTemplateCaptureProgressesUnderWrites(t *testing.T) {
	const size = 8 << 20
	dir := t.TempDir()
	template := filepath.Join(dir, "template.raw")
	path := filepath.Join(dir, "active.diff")
	want := bytes.Repeat([]byte{0x63}, size)
	writeSparseTemplate(t, template, want, []sparse.Extent{{Size: uint64(len(want))}})
	cow, err := OpenDisk(path, nil, DiffInit{Template: rawTemplateFactory(template)}, WithCheckpoint("templated", "base"))
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()
	stop := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				writerDone <- nil
				return
			default:
			}
			if _, err := cow.WriteAt([]byte{0x79}, 19); err != nil {
				writerDone <- err
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = cow.Checkpoint(ctx, "captured")
	close(stop)
	if writerErr := <-writerDone; writerErr != nil {
		t.Fatal(writerErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("checkpoint references unpublished file: %v", err)
	}
	if partials, _ := filepath.Glob(filepath.Join(dir, ".active.diff.*.partial")); len(partials) != 0 {
		t.Fatalf("unpublished partials: %v", partials)
	}
	view, err := cow.Export(ctx, "captured")
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	got := readExport(t, view)
	if !bytes.Equal(got[cowBlockSize:], want[cowBlockSize:]) {
		t.Fatal("template content damaged during frontend writes")
	}
	if got[19] != 0x79 && got[19] != 0x63 {
		t.Fatalf("invalid captured foreground byte: %x", got[19])
	}
	if !bytes.Equal(got[:19], want[:19]) || !bytes.Equal(got[20:cowBlockSize], want[20:cowBlockSize]) {
		t.Fatal("partial frontend write lost template bytes")
	}
}

type failAfterCheckpointWrite struct {
	diffBackend
	discards int
}

func (b *failAfterCheckpointWrite) writePages(ctx context.Context, pages []pageWrite) error {
	if err := b.diffBackend.writePages(ctx, pages); err != nil {
		return err
	}
	return io.ErrShortWrite
}
func (b *failAfterCheckpointWrite) discard(ctx context.Context, off, length int64) error {
	b.discards++
	return b.diffBackend.discard(ctx, off, length)
}

func TestCheckpointCOWWriteFailureCannotPunchCommittedOrPinnedSlots(t *testing.T) {
	cow := checkpointCOW(t, filepath.Join(t.TempDir(), "active.diff"), nil, 8*cowBlockSize, false)
	ctx := context.Background()
	if _, err := cow.WriteAt(bytes.Repeat([]byte{0x11}, cowBlockSize), 0); err != nil {
		t.Fatal(err)
	}
	if err := cow.Checkpoint(ctx, "before"); err != nil {
		t.Fatal(err)
	}
	view, err := cow.Export(ctx, "before")
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	want := readExport(t, view)
	fault := &failAfterCheckpointWrite{diffBackend: cow.diff}
	cow.diff = fault
	// Two fresh logical pages ensure the old raw failure cleanup would punch
	// offsets that belong to v2 control/index space rather than those pages.
	if _, err := cow.WriteAt(bytes.Repeat([]byte{0x72}, 2*cowBlockSize), 4*cowBlockSize); err != nil {
		t.Fatal(err)
	}
	if err := cow.Drain(ctx); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("missing sticky failure: %v", err)
	}
	if err := cow.Flush(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Flush lost failure: %v", err)
	}
	if err := cow.Close(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if fault.discards != 0 {
		t.Fatalf("failed v2 write punched %d logical ranges", fault.discards)
	}
	if got := readExport(t, view); !bytes.Equal(got, want) {
		t.Fatal("failure cleanup corrupted protected version")
	}
}

func TestCheckpointCOWMultiPageWriteRetainsPartialCount(t *testing.T) {
	cache, err := newCache(cowBlockSize, cowBlockSize, cacheHooks{beforeWrite: func([]*cachePage) error { return io.ErrShortWrite }})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	cow := checkpointCOW(t, filepath.Join(t.TempDir(), "active.diff"), nil, 8*cowBlockSize, false, WithCache(cache))
	defer cow.Close()
	n, err := cow.WriteAt(bytes.Repeat([]byte{9}, 3*cowBlockSize), 0)
	if n != cowBlockSize || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial write count/error: %d %v", n, err)
	}
}

func TestCheckpointCOWEncryptedDispersedBatch(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cache, err := newCache(16*cowBlockSize, 12*cowBlockSize, cacheHooks{beforeSelect: func() { once.Do(func() { close(entered) }); <-release }})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	cow := checkpointCOW(t, filepath.Join(t.TempDir(), "active.diff"), nil, 64*cowBlockSize, true, WithCache(cache))
	defer cow.Close()
	blocks := []int64{31, 2, 19, 0, 50, 12}
	for _, block := range blocks {
		if _, err := cow.WriteAt(bytes.Repeat([]byte{byte(block + 1)}, cowBlockSize), block*cowBlockSize); err != nil {
			close(release)
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("worker did not select")
	}
	close(release)
	if err := cow.Checkpoint(context.Background(), "dispersed"); err != nil {
		t.Fatal(err)
	}
	view, err := cow.Export(context.Background(), "dispersed")
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	got := readExport(t, view)
	for _, block := range blocks {
		if !bytes.Equal(got[block*cowBlockSize:(block+1)*cowBlockSize], bytes.Repeat([]byte{byte(block + 1)}, cowBlockSize)) {
			t.Fatalf("XTS used physical instead of logical sector for block %d", block)
		}
	}
}

func TestCheckpointCOWBatchDeleteValidatesBeforeRevocation(t *testing.T) {
	ctx := context.Background()
	cow := checkpointCOW(t, filepath.Join(t.TempDir(), "active.diff"), nil, 8*cowBlockSize, false)
	page := bytes.Repeat([]byte{0x71}, cowBlockSize)
	if _, err := cow.WriteAt(page, 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range []CheckpointID{"a", "b"} {
		if err := cow.Checkpoint(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := cow.DeleteCheckpoints(ctx, []CheckpointID{"a", ActiveDiff}); err == nil {
		t.Fatal("accepted empty ID in batch")
	}
	ids, err := cow.ListCheckpoints(ctx)
	if err != nil || len(ids) != 2 {
		t.Fatalf("invalid batch revoked a prefix: %v %v", ids, err)
	}
	pinned, err := cow.Export(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	if err := cow.DeleteCheckpoints(ctx, []CheckpointID{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	ids, err = cow.ListCheckpoints(ctx)
	if err != nil || len(ids) != 0 {
		t.Fatalf("batch remains visible: %v %v", ids, err)
	}
	if err := cow.DeleteCheckpoints(ctx, []CheckpointID{"a", "b"}); err != nil {
		t.Fatal("batch retry", err)
	}
	if _, err := cow.WriteAt(bytes.Repeat([]byte{0x92}, cowBlockSize), 0); err != nil {
		t.Fatal(err)
	}
	if err := cow.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readExport(t, pinned); !bytes.Equal(got[:cowBlockSize], page) {
		t.Fatal("batch deletion released reader protection")
	}
}

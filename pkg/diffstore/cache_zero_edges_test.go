package diffstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

func TestCheckpointCachedZeroOnePageCacheMakesProgress(t *testing.T) {
	const pages = 129
	cache := testCache(t, 1, 1, cacheHooks{})
	base := &fakeReader{data: bytes.Repeat([]byte{0x6d}, pages*cowBlockSize)}
	cow := cachedCOW(t, cache, pages, base, WithCheckpoint("zero-one-page", "base"))
	if err := cow.Discard(0, pages*cowBlockSize); err != nil {
		t.Fatal(err)
	}
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, cache, 0, 0, 0)
	if cache.Stats().WrittenBytes != 0 {
		t.Fatal("pure Zero wrote payload")
	}
	// Completed Zero no longer occupies the cache. A subsequent partial write
	// must construct Data from the committed Zero and preserve untouched bytes.
	if _, err := cow.WriteAt([]byte{0xa7}, (pages-1)*cowBlockSize+11); err != nil {
		t.Fatal(err)
	}
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, pages*cowBlockSize)
	want[(pages-1)*cowBlockSize+11] = 0xa7
	checkRead(t, cow, 0, want)
	var payload [pages]bool
	if err := cow.diff.payloadPresence(context.Background(), 0, payload[:]); err != nil {
		t.Fatal(err)
	}
	for i, data := range payload {
		if data != (i == pages-1) {
			t.Fatalf("page %d payload=%t after Zero/partial write", i, data)
		}
	}
}

func TestCheckpointCompletedZeroDoesNotEvictCachedData(t *testing.T) {
	const pages = 32
	cache := testCache(t, 2, 2, cacheHooks{})
	cow := cachedCOW(t, cache, pages, nil, WithCheckpoint("zero-release-data", ""))
	if err := cow.WaitMaterialized(context.Background()); err != nil {
		t.Fatal(err)
	}
	writePage(t, cow, 0, 0xa5)
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	// One unrelated Data page and one in-flight Zero fit together. Do not
	// refresh the Data page's LRU age between requests: completed Zero must
	// leave the next reservation free rather than evicting the cached Data.
	for block := 1; block < pages; block++ {
		if err := cow.Discard(int64(block*cowBlockSize), cowBlockSize); err != nil {
			t.Fatal(err)
		}
		if err := cow.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	before := cow.versions.store.Stats().DataRead
	checkRead(t, cow, 0, bytes.Repeat([]byte{0xa5}, cowBlockSize))
	if delta := cow.versions.store.Stats().DataRead - before; delta != 0 {
		t.Fatalf("completed Zero evicted unrelated cached Data: backend read %d bytes", delta)
	}
	checkBudget(t, cache, 1, 0, 0)
	// Releasing the accepted cache mutation must leave readable backend Zero.
	checkRead(t, cow, 1, make([]byte, (pages-1)*cowBlockSize))
}

func TestCheckpointCachedZeroCloseRetainsAcceptedPrefix(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	waiting := make(chan struct{}, 16)
	cache := testCache(t, 1, 1, cacheHooks{
		beforeWrite: func([]*cachePage) error { gate.wait(); return nil },
		waiting:     func() { waiting <- struct{}{} },
	})
	base := &fakeReader{data: bytes.Repeat([]byte{0x6d}, 2*cowBlockSize)}
	cow := cachedCOW(t, cache, 2, base, WithCheckpoint("zero-prefix", "base"))
	path := cow.versions.path
	discarded := make(chan error, 1)
	go func() { discarded <- cow.Discard(0, 2*cowBlockSize) }()
	awaitSignal(t, gate.entered)
	awaitSignal(t, waiting)
	closed := make(chan error, 1)
	go func() { closed <- cow.Close() }()
	if err := awaitError(t, discarded); !errors.Is(err, context.Canceled) {
		t.Fatal("multi-page discard did not cancel its unaccepted suffix", err)
	}
	gate.open()
	if err := awaitError(t, closed); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDisk(path, base, DiffInit{Existing: true}, WithCheckpoint("zero-prefix", "base"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	checkRead(t, reopened, 0, make([]byte, cowBlockSize))
	checkRead(t, reopened, 1, base.data[cowBlockSize:])
}

type cachedZeroPresenceGate struct {
	diffBackend
	gate *workerGate
}

func (d *cachedZeroPresenceGate) payloadPresence(ctx context.Context, first uint64, dst []bool) error {
	d.gate.wait()
	return d.diffBackend.payloadPresence(ctx, first, dst)
}

func TestCheckpointCachedZeroExportSurvivesCommitAndEviction(t *testing.T) {
	worker, presence := newWorkerGate(), newWorkerGate()
	defer worker.open()
	defer presence.open()
	var pause atomic.Bool
	cache := testCache(t, 2, 2, cacheHooks{beforeSelect: func() {
		if pause.Load() {
			worker.wait()
		}
	}})
	cow := cachedCOW(t, cache, 2, nil, WithCheckpoint("zero-export-race", ""))
	// Join fresh initialization before the test replaces the backend wrapper.
	if err := cow.WaitMaterialized(context.Background()); err != nil {
		t.Fatal(err)
	}
	writePage(t, cow, 0, 0x7a)
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	pause.Store(true)
	if err := cow.Discard(0, cowBlockSize); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, worker.entered)
	writePage(t, cow, 1, 0)
	cow.diff = &cachedZeroPresenceGate{diffBackend: cow.diff, gate: presence}
	view, err := cow.Export(context.Background(), ActiveDiff)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	classified := make(chan error, 1)
	go func() {
		run, err := view.RunAt(0, cowBlockSize)
		if err == nil && run.Kind() != sparse.Zero {
			err = errors.New("captured cached Zero became Data after eviction")
		}
		classified <- err
	}()
	// RunAt captured the cache override before entering backend presence.
	awaitSignal(t, presence.entered)
	worker.open()
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	for _, p := range cache.pages {
		cache.releaseLocked(p)
	}
	cache.mu.Unlock()
	presence.open()
	if err := awaitError(t, classified); err != nil {
		t.Fatal(err)
	}
	// With no override remaining, the committed backend must give the same
	// classification, including distinguishing ordinary all-zero Data.
	requireActiveKinds(t, cow, []sparse.RunKind{sparse.Zero, sparse.Data})
	got := make([]byte, 2*cowBlockSize)
	if _, err := view.ReadAt(context.Background(), got, 0); err != nil || !bytes.Equal(got, make([]byte, len(got))) {
		t.Fatal("export bytes changed across Zero writeback", err)
	}
}

func TestCheckpointCachedZeroFatalReachesOwnerWithoutGuestRequest(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	cache := testCache(t, 1, 1, cacheHooks{beforeWrite: func([]*cachePage) error {
		gate.wait()
		return io.ErrUnexpectedEOF
	}})
	cow := cachedCOW(t, cache, 1, nil, WithCheckpoint("zero-fatal-owner", ""))
	// Fresh Export also joins initial publication, whose Drain must not be
	// trapped behind the deliberately blocked Zero writeback below.
	if err := cow.WaitMaterialized(context.Background()); err != nil {
		t.Fatal(err)
	}
	reported := make(chan error, 1)
	cow.SetFatalHandler(func(err error) { reported <- err })
	if err := cow.Discard(0, cowBlockSize); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, gate.entered)
	view, err := cow.Export(context.Background(), ActiveDiff)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	gate.open()
	// No Drain, read, FLUSH or further guest request drives this notification.
	if err := awaitError(t, reported); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("owner did not receive asynchronous Zero failure", err)
	}
	if _, err := view.RunAt(0, cowBlockSize); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("already opened active export ignored fatal Zero failure", err)
	}
	if err := cow.Close(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
}

package vhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type workerGate struct {
	entered, release       chan struct{}
	enterOnce, releaseOnce sync.Once
}

func newWorkerGate() *workerGate {
	return &workerGate{entered: make(chan struct{}), release: make(chan struct{})}
}
func (g *workerGate) wait() { g.enterOnce.Do(func() { close(g.entered) }); <-g.release }
func (g *workerGate) open() { g.releaseOnce.Do(func() { close(g.release) }) }
func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for deterministic gate")
	}
}
func awaitError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("operation did not finish")
		return nil
	}
}
func testCache(t *testing.T, total, dirty int, hooks cacheHooks) *COWCache {
	t.Helper()
	c, err := newCOWCache(uint64(total*cowBlockSize), uint64(dirty*cowBlockSize), hooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func cachedCOW(t *testing.T, cache *COWCache, blocks int, base BlockReader, options ...BlockCOWOption) *BlockCOW {
	t.Helper()
	options = append(options, WithCOWCache(cache))
	c, err := OpenBlockCOW(filepath.Join(t.TempDir(), "active.diff"), base, DiffInit{CreateSize: int64(blocks * cowBlockSize)}, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func writePage(t *testing.T, c *BlockCOW, block int, value byte) {
	t.Helper()
	if _, err := c.WriteAt(bytes.Repeat([]byte{value}, cowBlockSize), int64(block*cowBlockSize)); err != nil {
		t.Fatal(err)
	}
}
func checkBudget(t *testing.T, c *COWCache, used, dirty, wb int) {
	t.Helper()
	s := c.Stats()
	if s.Used != uint64(used*cowBlockSize) || s.DirtyUsed != uint64(dirty*cowBlockSize) || s.Writeback != uint64(wb*cowBlockSize) || s.DirtyUsed > s.Used || s.Used > s.Capacity || s.DirtyUsed > s.MaxDirty {
		t.Fatalf("budget=%+v, want used/dirty/wb=%d/%d/%d pages", s, used, dirty, wb)
	}
}
func checkRead(t *testing.T, c *BlockCOW, block int, want []byte) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := c.ReadAt(got, int64(block*cowBlockSize)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("latest cached plaintext mismatch")
	}
}

func TestCOWCacheSharedQuotaAdmissionSnapshotFlush(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	waiting := make(chan struct{}, 16)
	cache := testCache(t, 3, 2, cacheHooks{beforeSelect: gate.wait, waiting: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	root := cachedCOW(t, cache, 8, nil)
	data := cachedCOW(t, cache, 8, nil)
	var syncs atomic.Int32
	root.diff.syncFile = func() error { syncs.Add(1); return nil }
	data.diff.syncFile = root.diff.syncFile
	writePage(t, root, 0, 0x11)
	awaitSignal(t, gate.entered)
	writePage(t, data, 0, 0x22)
	payload := bytes.Repeat([]byte{0x33}, 512)
	for i := 0; i < 100; i++ {
		if _, err := root.WriteAt(payload, 512); err != nil {
			t.Fatal(err)
		}
	}
	clear(payload) // backend must own guest bytes
	checkBudget(t, cache, 2, 2, 0)
	cache.mu.Lock()
	first, last := cache.dirty.first, cache.dirty.last
	cache.mu.Unlock()
	if first.key.cow != root || last.key.cow != data || first.next != last || last.next != nil {
		t.Fatal("hot overwrites duplicated/reordered FIFO")
	}
	want := bytes.Repeat([]byte{0x11}, cowBlockSize)
	copy(want[512:], bytes.Repeat([]byte{0x33}, 512))
	checkRead(t, root, 0, want)
	view, holes, err := root.SnapshotView()
	if err != nil || len(holes) != 1 {
		t.Fatalf("SnapshotView %v %v", holes, err)
	}
	got := make([]byte, cowBlockSize)
	if _, err = io.ReadFull(view, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("unflushed snapshot: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := root.writeAt(ctx, []byte{1}, 2*cowBlockSize); done <- err }()
	awaitSignal(t, waiting)
	checkBudget(t, cache, 2, 2, 0) // free total slot, exhausted dirty subset
	cancel()
	if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("quota cancel: %v", err)
	}
	if err := root.Flush(); err != nil {
		t.Fatal(err)
	}
	if syncs.Load() != 0 || cache.Stats().WriteBatches != 0 {
		t.Fatal("guest FLUSH initiated write/sync")
	}
	gate.open()
	if err := cache.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, cache, 2, 0, 0)
	if syncs.Load() != 0 {
		t.Fatal("background/Drain called Sync")
	}
}

func TestCOWCacheFrozenPageReadsAndCancellableOverwrite(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	waiting := make(chan struct{}, 16)
	cache := testCache(t, 1, 1, cacheHooks{beforeWrite: func([]*cachePage) error { gate.wait(); return nil }, waiting: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	cow := cachedCOW(t, cache, 4, nil, WithDiffEncryption(testDiffKey(3), true))
	// A pre-existing upper page for a miss while the entire cache is in flight.
	diskPage := bytes.Repeat([]byte{0x55}, cowBlockSize)
	if _, err := cow.diff.WriteAt(diskPage, 2*cowBlockSize); err != nil {
		t.Fatal(err)
	}
	cow.markDirty(2)
	writePage(t, cow, 0, 0x44)
	awaitSignal(t, gate.entered)
	checkBudget(t, cache, 1, 1, 1)
	checkRead(t, cow, 0, bytes.Repeat([]byte{0x44}, cowBlockSize))
	checkRead(t, cow, 2, diskPage) // independent DIO read buffer; no cache reservation
	view, _, err := cow.SnapshotView()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, cowBlockSize)
	if _, err := io.ReadFull(view, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{0x44}, cowBlockSize)) {
		t.Fatal("frozen snapshot stale")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := cow.writeAt(ctx, []byte{9}, 0); done <- err }()
	awaitSignal(t, waiting)
	readDone := make(chan error, 1)
	go func() { _, err := cow.ReadAt(make([]byte, 512), 0); readDone <- err }()
	if err := awaitError(t, readDone); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("frozen overwrite cancel: %v", err)
	}
	gate.open()
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := cow.WriteAt([]byte{9}, 17); err != nil {
		t.Fatal(err)
	}
	expected := bytes.Repeat([]byte{0x44}, cowBlockSize)
	expected[17] = 9
	checkRead(t, cow, 0, expected)
}

func TestCOWCacheOnePageLargeRequestReopen(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			cache := testCache(t, 1, 1, cacheHooks{})
			var options []BlockCOWOption
			if encrypted {
				options = append(options, WithDiffEncryption(testDiffKey(4), true))
			}
			base := patternedBytes(19 * cowBlockSize)
			cow := cachedCOW(t, cache, 19, &fakeReader{data: base}, options...)
			payload := bytes.Repeat([]byte{0xb7}, 17*cowBlockSize+512)
			want := append([]byte(nil), base...)
			copy(want[512:], payload)
			if n, err := cow.WriteAt(payload, 512); err != nil || n != len(payload) {
				t.Fatalf("large write %d: %v", n, err)
			}
			path := cow.diff.f.Name()
			if err := cow.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenBlockCOW(path, &fakeReader{data: base}, DiffInit{Existing: true}, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got := make([]byte, len(want))
			if _, err := reopened.ReadAt(got, 0); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("large partial write lost untouched bytes")
			}
			s := cache.Stats()
			if s.PeakUsed != cowBlockSize || s.PeakDirty != cowBlockSize {
				t.Fatalf("one-page budget %+v", s)
			}
		})
	}
}

func TestCOWCacheCleanLRUAndPromotion(t *testing.T) {
	cache := testCache(t, 2, 1, cacheHooks{})
	cow := cachedCOW(t, cache, 4, nil)
	for i := 0; i < 2; i++ {
		writePage(t, cow, i, byte(i+1))
		if err := cow.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	checkBudget(t, cache, 2, 0, 0) // clean can use the entire pool, not capacity-dirty
	checkRead(t, cow, 0, bytes.Repeat([]byte{1}, cowBlockSize))
	writePage(t, cow, 2, 3)
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	has0 := cache.pages[cacheKey{cow, 0}] != nil
	has1 := cache.pages[cacheKey{cow, 1}] != nil
	cache.mu.Unlock()
	if !has0 || has1 {
		t.Fatal("clean LRU evicted the recently read page")
	}
	writePage(t, cow, 0, 7)
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, cache, 2, 0, 0)
	checkRead(t, cow, 1, bytes.Repeat([]byte{2}, cowBlockSize)) // reload evicted upper
	checkBudget(t, cache, 2, 0, 0)
}

// A clean hit must acquire the dirty subset before modification, even with a
// spare total slot. Once writeback frees that subset, promotion reuses the page.
func TestCOWCacheCleanPromotionWaitsForDirtyQuota(t *testing.T) {
	first, promoted := newWorkerGate(), newWorkerGate()
	defer first.open()
	defer promoted.open()
	waiting := make(chan struct{}, 8)
	var batches atomic.Int32
	cache := testCache(t, 3, 1, cacheHooks{
		waiting: func() {
			select {
			case waiting <- struct{}{}:
			default:
			}
		},
		beforeWrite: func([]*cachePage) error {
			if batches.Add(1) == 1 {
				first.wait()
			} else {
				promoted.wait()
			}
			return nil
		},
	})
	cow := cachedCOW(t, cache, 3, nil)
	original := bytes.Repeat([]byte{0x35}, cowBlockSize)
	if _, err := cow.diff.WriteAt(original, 0); err != nil {
		t.Fatal(err)
	}
	cow.markDirty(0)
	checkRead(t, cow, 0, original)
	writePage(t, cow, 1, 0x46)
	awaitSignal(t, first.entered)
	checkBudget(t, cache, 2, 1, 1)
	done := make(chan error, 1)
	go func() { _, err := cow.WriteAt([]byte{0x57}, 123); done <- err }()
	awaitSignal(t, waiting)
	checkBudget(t, cache, 2, 1, 1)
	checkRead(t, cow, 0, original)
	first.open()
	if err := awaitError(t, done); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, promoted.entered)
	checkBudget(t, cache, 2, 1, 1)
	original[123] = 0x57
	checkRead(t, cow, 0, original)
	promoted.open()
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, cache, 2, 0, 0)
}

type gatedReadBody struct {
	diffBodyIO
	gate *workerGate
}

func (r gatedReadBody) ReadAt(p []byte, off int64) (int, error) {
	r.gate.wait()
	return r.diffBodyIO.ReadAt(p, off)
}
func TestCOWCacheLoadingReservationAndCloseCancellation(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	waiting := make(chan struct{}, 16)
	cache := testCache(t, 1, 1, cacheHooks{waiting: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	cow := cachedCOW(t, cache, 4, nil)
	page := bytes.Repeat([]byte{5}, cowBlockSize)
	if _, err := cow.diff.WriteAt(page, 0); err != nil {
		t.Fatal(err)
	}
	cow.markDirty(0)
	cow.diff.bodyIO = gatedReadBody{cow.diff.bodyIO, gate}
	readDone := make(chan error, 1)
	go func() { _, err := cow.ReadAt(make([]byte, 512), 0); readDone <- err }()
	awaitSignal(t, gate.entered)
	checkBudget(t, cache, 1, 0, 0)
	if cache.Stats().Loading != cowBlockSize {
		t.Fatal("cold load was not reserved")
	}
	writeDone := make(chan error, 1)
	go func() { _, err := cow.WriteAt(page, cowBlockSize); writeDone <- err }()
	awaitSignal(t, waiting)
	closeDone := make(chan error, 1)
	go func() { closeDone <- cow.Close() }()
	awaitSignal(t, cow.lifeCtx.Done())
	if err := awaitError(t, writeDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close did not cancel quota wait: %v", err)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close released in-flight read: %v", err)
	default:
	}
	gate.open()
	if err := awaitError(t, readDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed load result: %v", err)
	}
	if err := awaitError(t, closeDone); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, cache, 0, 0, 0)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, p := range cache.free {
		if !allZero(p.data[:]) {
			t.Fatal("released plaintext retained")
		}
	}
}

func TestCOWCacheFIFOContiguousBatchBoundAndHoles(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	type record struct {
		cow   *BlockCOW
		start int64
		pages int
	}
	var records []record
	cache := testCache(t, 520, 520, cacheHooks{beforeSelect: gate.wait, beforeWrite: func(p []*cachePage) error {
		records = append(records, record{p[0].key.cow, p[0].key.block, len(p)})
		return nil
	}})
	a := cachedCOW(t, cache, 530, nil)
	b := cachedCOW(t, cache, 2, nil)
	for i := 0; i < 257; i++ {
		writePage(t, a, i, 1)
	}
	awaitSignal(t, gate.entered)
	writePage(t, b, 0, 2)
	for i := 258; i < 516; i++ {
		writePage(t, a, i, 3)
	} // block 257 remains a hole
	writePage(t, a, 0, 4) // hot oldest stays first
	gate.open()
	if err := cache.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(records) != 5 || records[0].pages != 256 || records[1].start != 256 || records[2].cow != b || records[3].start != 258 || records[3].pages != 256 || records[4].pages != 2 {
		t.Fatalf("FIFO batches: %+v", records)
	}
	bitmap, err := a.diff.scanDirtyBlocks()
	if err != nil {
		t.Fatal(err)
	}
	if bitmapBlockDirty(bitmap, 257) {
		t.Fatal("batch writeback materialized a hole")
	}
}

type prefixShortBody struct {
	diffBodyIO
	calls atomic.Int32
}

func (s *prefixShortBody) WriteAt(p []byte, off int64) (int, error) {
	s.calls.Add(1)
	if len(p) <= cowBlockSize {
		return 0, io.ErrShortWrite
	}
	return s.diffBodyIO.WriteAt(p[:cowBlockSize], off)
}
func TestCOWCacheFatalShortBatchStickyAndOwnerNotification(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	waiting := make(chan struct{}, 16)
	cache := testCache(t, 2, 2, cacheHooks{beforeSelect: gate.wait, waiting: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	cow := cachedCOW(t, cache, 4, nil, WithDiffEncryption(testDiffKey(7), true))
	if _, err := cow.diff.WriteAt(bytes.Repeat([]byte{0x11}, cowBlockSize), 0); err != nil {
		t.Fatal(err)
	}
	cow.markDirty(0)
	short := &prefixShortBody{diffBodyIO: cow.diff.bodyIO}
	cow.diff.bodyIO = short
	notified := make(chan error, 1)
	cow.SetFatalHandler(func(err error) { notified <- err })
	writePage(t, cow, 0, 0x22)
	awaitSignal(t, gate.entered)
	writePage(t, cow, 1, 0x33)
	view, _, err := cow.SnapshotView()
	if err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { _, err := cow.WriteAt([]byte{1}, 2*cowBlockSize); waitDone <- err }()
	awaitSignal(t, waiting)
	gate.open()
	fatal := awaitError(t, notified) // independent of another guest request
	if !errors.Is(fatal, io.ErrShortWrite) {
		t.Fatalf("fatal: %v", fatal)
	}
	if err := awaitError(t, waitDone); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("waiter: %v", err)
	}
	if _, err := view.Read(make([]byte, 1)); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("snapshot after fatal: %v", err)
	}
	if err := cow.Flush(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Flush after fatal: %v", err)
	}
	if err := cow.Drain(context.Background()); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Drain after fatal: %v", err)
	}
	checkBudget(t, cache, 2, 2, 2)
	if short.calls.Load() != 1 {
		t.Fatal("short write retried an unaligned remainder")
	}
	bitmap, err := cow.diff.scanDirtyBlocks()
	if err != nil {
		t.Fatal(err)
	}
	if !bitmapBlockDirty(bitmap, 0) || bitmapBlockDirty(bitmap, 1) {
		t.Fatal("failed mixed batch punched an existing upper or retained a new partial page")
	}
	raw := make([]byte, cowBlockSize)
	if _, err := cow.diff.ReadAt(raw, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, bytes.Repeat([]byte{0x22}, cowBlockSize)) {
		t.Fatal("existing upper was destroyed by batch cleanup")
	}
	if err := cow.Close(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Close did not propagate: %v", err)
	}
}

func TestCOWCacheCloseDrainsDespiteFrontendCancellation(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	cache := testCache(t, 1, 1, cacheHooks{beforeWrite: func([]*cachePage) error { gate.wait(); return nil }})
	cow := cachedCOW(t, cache, 2, nil)
	path := cow.diff.f.Name()
	writePage(t, cow, 0, 0x77)
	awaitSignal(t, gate.entered)
	done := make(chan error, 1)
	go func() { done <- cow.Close() }()
	awaitSignal(t, cow.lifeCtx.Done())
	if _, err := cow.WriteAt([]byte{1}, cowBlockSize); err == nil {
		t.Fatal("admission after Close")
	}
	select {
	case err := <-done:
		t.Fatalf("Close returned during I/O: %v", err)
	default:
	}
	gate.open()
	if err := awaitError(t, done); err != nil {
		t.Fatal(err)
	}
	if err := cow.Close(); err != nil {
		t.Fatal("Close not idempotent")
	}
	reopened, err := OpenBlockCOW(path, nil, DiffInit{Existing: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	checkRead(t, reopened, 0, bytes.Repeat([]byte{0x77}, cowBlockSize))
	checkBudget(t, cache, 0, 0, 0)
}

func TestCOWCacheDiscardCannotResurrectWriteback(t *testing.T) {
	for _, inflight := range []bool{false, true} {
		t.Run(fmt.Sprint(inflight), func(t *testing.T) {
			gate := newWorkerGate()
			defer gate.open()
			waiting := make(chan struct{}, 16)
			hooks := cacheHooks{waiting: func() {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}}
			if inflight {
				hooks.beforeWrite = func([]*cachePage) error { gate.wait(); return nil }
			} else {
				hooks.beforeSelect = gate.wait
			}
			cache := testCache(t, 1, 1, hooks)
			base := bytes.Repeat([]byte{0x61}, 2*cowBlockSize)
			cow := cachedCOW(t, cache, 2, &fakeReader{data: base})
			writePage(t, cow, 0, 0x62)
			awaitSignal(t, gate.entered)
			done := make(chan error, 1)
			go func() { done <- cow.Discard(0, cowBlockSize) }()
			if inflight {
				awaitSignal(t, waiting)
				gate.open()
			}
			if err := awaitError(t, done); err != nil {
				t.Fatal(err)
			}
			gate.open()
			if err := cow.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			checkRead(t, cow, 0, base[:cowBlockSize])
			bitmap, err := cow.diff.scanDirtyBlocks()
			if err != nil {
				t.Fatal(err)
			}
			if bitmapBlockDirty(bitmap, 0) || cow.blockDirty(0) {
				t.Fatal("discarded page resurrected")
			}
			checkBudget(t, cache, 0, 0, 0)
		})
	}
}

func TestCOWCacheRejectsInvalidOwnershipAndSizes(t *testing.T) {
	for _, sizes := range [][2]uint64{{0, 4096}, {4096, 0}, {4096, 8192}, {4097, 4096}, {8192, 1}} {
		if c, err := NewCOWCache(sizes[0], sizes[1]); err == nil {
			_ = c.Close()
			t.Fatalf("accepted %v", sizes)
		}
	}
	cache := testCache(t, 1, 1, cacheHooks{})
	cow := cachedCOW(t, cache, 1, nil)
	if err := cache.Close(); err == nil {
		t.Fatal("cache closed while a COW owns it")
	}
	if err := cow.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBlockCOW(filepath.Join(t.TempDir(), "diff"), nil, DiffInit{CreateSize: cowBlockSize}, WithCOWCache(cache)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed cache attach: %v", err)
	}
}

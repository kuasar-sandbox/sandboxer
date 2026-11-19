package diffstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestCOWCacheAggregationDeadlineAndBacklog(t *testing.T) {
	deadline := make(chan time.Time, 1)
	waiting := make(chan struct{}, 256)
	writes := make(chan struct{}, 32)
	var timers atomic.Int32
	cache := testCache(t, 16, 16, cacheHooks{
		newTimer:    func() (<-chan time.Time, func()) { timers.Add(1); return deadline, func() {} },
		aggregating: func() { waiting <- struct{}{} },
		beforeWrite: func([]*cachePage) error { writes <- struct{}{}; return nil },
	})
	var disks [8]*Disk
	for i := range disks {
		disks[i] = cachedCOW(t, cache, 32, nil)
	}
	writePage(t, disks[0], 0, 1)
	awaitSignal(t, waiting)
	// Consume the initial queued kick as well as later ordinary notifications.
	// An unfired manual timer makes premature selection deterministic.
	for i := 1; i < 8; i++ {
		writePage(t, disks[i], i*2, byte(i+1))
		select {
		case <-waiting:
		case <-writes:
			t.Fatal("ordinary kick ended aggregation")
		case <-time.After(10 * time.Second):
			t.Fatal("worker did not recheck aggregation")
		}
	}
	select {
	case <-writes:
		t.Fatal("write before deadline")
	default:
	}
	close(deadline)
	// Do not call Drain here: that would mask a fresh wait per singleton.
	for i := 0; i < 8; i++ {
		awaitSignal(t, writes)
	}
	if timers.Load() != 8 {
		t.Fatalf("backlog started %d aggregation windows", timers.Load())
	}
	if err := cache.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCOWCacheAggregationInterrupted(t *testing.T) {
	for _, pressure := range []bool{false, true} {
		t.Run(fmt.Sprintf("pressure=%t", pressure), func(t *testing.T) {
			waiting := make(chan struct{}, 16)
			cache := testCache(t, 2, 1, cacheHooks{
				newTimer:    func() (<-chan time.Time, func()) { return make(chan time.Time), func() {} },
				aggregating: func() { waiting <- struct{}{} },
			})
			cow := cachedCOW(t, cache, 2, nil)
			writePage(t, cow, 0, 1)
			awaitSignal(t, waiting)
			done := make(chan error, 1)
			go func() {
				if pressure {
					_, err := cow.WriteAt(bytes.Repeat([]byte{2}, cowBlockSize), cowBlockSize)
					done <- err
				} else {
					done <- cow.Drain(context.Background())
				}
			}()
			if err := awaitError(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCOWCacheSpatialNeighboursKeepOldestAndUnselectedAge(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	type record struct {
		cow   *Disk
		first int64
		count int
	}
	records := make(chan record, 16)
	cache := testCache(t, 16, 16, cacheHooks{beforeSelect: gate.wait, beforeWrite: func(p []*cachePage) error {
		records <- record{p[0].key.cow, p[0].key.block, len(p)}
		return nil
	}})
	a := cachedCOW(t, cache, 16, nil)
	b := cachedCOW(t, cache, 16, nil)
	seedUpper(t, a, 2, bytes.Repeat([]byte{9}, cowBlockSize))
	checkRead(t, a, 2, bytes.Repeat([]byte{9}, cowBlockSize)) // clean neighbour must not merge
	writePage(t, a, 5, 1)
	awaitSignal(t, gate.entered)
	writePage(t, b, 4, 2)
	for _, block := range []int{7, 3, 6, 4, 10, 0} {
		writePage(t, a, block, 3)
	}
	gate.open()
	if err := cache.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	expected := map[record]bool{{a, 3, 7}: true, {b, 4, 1}: true}
	for range 2 {
		select {
		case got := <-records:
			if !expected[got] {
				t.Fatalf("unexpected batch %+v", got)
			}
			delete(expected, got)
		default:
			t.Fatal("missing batch")
		}
	}
	select {
	case got := <-records:
		t.Fatalf("unexpected batch %+v", got)
	default:
	}
	bitmap, err := a.diff.scanDirtyBlocks()
	if err != nil {
		t.Fatal(err)
	}
	if bitmapBlockDirty(bitmap, 1) || bitmapBlockDirty(bitmap, 8) || bitmapBlockDirty(bitmap, 9) {
		t.Fatal("coalescing filled a hole")
	}
}

func TestCOWCacheDisjointFirstWritesMergePresence(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			gate := newWorkerGate()
			defer gate.open()
			cache := testCache(t, 128, 128, cacheHooks{beforeSelect: gate.wait})
			var options []DiskOption
			if encrypted {
				options = append(options, WithDiffEncryption(testDiffKey(33), true))
			}
			cow := cachedCOW(t, cache, 128, nil, options...)
			var writes atomic.Int32
			cow.diff.(*diffFile).beforePresenceWrite = func(int) error { writes.Add(1); return nil }
			for i := 0; i < 64; i++ {
				writePage(t, cow, i*2, byte(i+1))
			}
			awaitSignal(t, gate.entered)
			gate.open()
			if err := cow.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if writes.Load() != 1 {
				t.Fatal("rewrote the same P page within one disjoint batch", writes.Load())
			}
			for i := 0; i < 64; i++ {
				got := make([]byte, cowBlockSize)
				if _, err := cow.diff.ReadAt(got, int64(i*2*cowBlockSize)); err != nil || !bytes.Equal(got, bytes.Repeat([]byte{byte(i + 1)}, cowBlockSize)) {
					t.Fatal("persisted data", i, err)
				}
				if bitmap, err := cow.diff.scanDirtyBlocks(); err != nil || bitmapBlockDirty(bitmap, int64(i*2+1)) {
					t.Fatal("batch filled a hole", i, err)
				}
			}
		})
	}
}

func seedUpper(t *testing.T, cow *Disk, first int, payload []byte) {
	t.Helper()
	if err := writeFullAt(cow.diff, payload, int64(first*cowBlockSize)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(payload)/cowBlockSize; i++ {
		cow.markDirty(int64(first + i))
	}
}

type batchReadProbe struct {
	diffBodyIO
	calls, bytes atomic.Int64
}

func (p *batchReadProbe) ReadAt(buf []byte, off int64) (int, error) {
	p.calls.Add(1)
	n, err := p.diffBodyIO.ReadAt(buf, off)
	p.bytes.Add(int64(n))
	return n, err
}

func TestCOWCacheColdRunBoundedSyscallsAndSnapshot(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			cache := testCache(t, 2, 1, cacheHooks{}) // much smaller than a run
			var options []DiskOption
			if encrypted {
				options = append(options, WithDiffEncryption(testDiffKey(17), true))
			}
			cow := cachedCOW(t, cache, 3*maxWritebackPages, nil, options...)
			want := patternedBytes(3 * maxDiffScratchSize)
			seedUpper(t, cow, 0, want)
			probe := &batchReadProbe{diffBodyIO: cow.diff.(*diffFile).bodyIO}
			cow.diff.(*diffFile).bodyIO = probe
			for _, snapshot := range []bool{false, true} {
				cache.mu.Lock()
				for _, p := range cache.pages {
					cache.releaseLocked(p)
				}
				cache.mu.Unlock()
				probe.calls.Store(0)
				probe.bytes.Store(0)
				got := make([]byte, len(want))
				if snapshot {
					view, _, err := cow.SnapshotView()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := io.ReadFull(view, got); err != nil {
						t.Fatal(err)
					}
				} else {
					if n, err := cow.ReadAt(got, 0); err != nil || n != len(got) {
						t.Fatalf("read %d: %v", n, err)
					}
				}
				if !bytes.Equal(got, want) {
					t.Fatal("batched plaintext mismatch")
				}
				if probe.calls.Load() != 3 || probe.bytes.Load() != int64(len(want)) {
					t.Fatalf("read calls/bytes=%d/%d", probe.calls.Load(), probe.bytes.Load())
				}
				checkBudget(t, cache, 2, 0, 0)
			}
		})
	}
}

func TestCOWCacheColdRunOwnsPublishedPlaintext(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			output := make([]byte, 3*cowBlockSize)
			cache := testCache(t, 4, 1, cacheHooks{afterReadCopy: func() { clear(output) }})
			var options []DiskOption
			if encrypted {
				options = append(options, WithDiffEncryption(testDiffKey(18), true))
			}
			cow := cachedCOW(t, cache, 4, nil, options...)
			want := bytes.Repeat([]byte{0x79}, len(output))
			seedUpper(t, cow, 0, want)
			if _, err := cow.ReadAt(output, 0); err != nil {
				t.Fatal(err)
			}
			if !allZero(output) {
				t.Fatal("adversarial caller hook did not run")
			}
			got := make([]byte, len(output))
			if _, err := cow.ReadAt(got, 0); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("cache trusted mutable caller output")
			}
		})
	}
}

func TestCOWCacheReadMixedMissCleanDirtyWritebackAndBase(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	cache := testCache(t, 8, 4, cacheHooks{beforeWrite: func([]*cachePage) error { gate.wait(); return nil }})
	base := bytes.Repeat([]byte{0x88}, 16*cowBlockSize)
	cow := cachedCOW(t, cache, 16, &fakeReader{data: base})
	want := bytes.Repeat([]byte{0x31}, 16*cowBlockSize)
	// Leave page 8 genuinely unwritten in both frontend and backend state.
	seedUpper(t, cow, 0, want[:8*cowBlockSize])
	seedUpper(t, cow, 9, want[9*cowBlockSize:13*cowBlockSize])
	copy(want[8*cowBlockSize:9*cowBlockSize], base[:cowBlockSize])
	copy(want[13*cowBlockSize:], base[13*cowBlockSize:])
	checkRead(t, cow, 2, want[2*cowBlockSize:3*cowBlockSize])
	writePage(t, cow, 6, 0x66)
	awaitSignal(t, gate.entered)
	writePage(t, cow, 4, 0x44)
	writePage(t, cow, 10, 0xaa)
	for _, v := range []struct {
		block int
		value byte
	}{{4, 0x44}, {6, 0x66}, {10, 0xaa}} {
		copy(want[v.block*cowBlockSize:(v.block+1)*cowBlockSize], bytes.Repeat([]byte{v.value}, cowBlockSize))
	}
	probe := &batchReadProbe{diffBodyIO: cow.diff.(*diffFile).bodyIO}
	cow.diff.(*diffFile).bodyIO = probe
	got := make([]byte, len(want))
	if _, err := cow.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("mixed live read lost authoritative cached/base data")
	}
	if probe.calls.Load() != 6 || probe.bytes.Load() != 8*cowBlockSize {
		t.Fatalf("mixed calls/bytes=%d/%d", probe.calls.Load(), probe.bytes.Load())
	}
	view, _, err := cow.SnapshotView()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(view, got); err != nil {
		t.Fatal(err)
	}
	clear(want[8*cowBlockSize : 9*cowBlockSize])
	clear(want[13*cowBlockSize:])
	if !bytes.Equal(got, want) {
		t.Fatal("snapshot read fell through to base or stale disk")
	}
	gate.open()
}

func TestCOWCacheColdRunOverlappingLoadingCancellationAndPressure(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	waiting := make(chan struct{}, 32)
	cache := testCache(t, 2, 1, cacheHooks{waiting: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	cow := cachedCOW(t, cache, 5, nil)
	want := bytes.Repeat([]byte{0x37}, 4*cowBlockSize)
	seedUpper(t, cow, 0, want)
	cow.diff.(*diffFile).bodyIO = gatedReadBody{cow.diff.(*diffFile).bodyIO, gate}
	first, second := make([]byte, len(want)), make([]byte, len(want))
	firstDone, secondDone, writeDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() { _, err := cow.ReadAt(first, 0); firstDone <- err }()
	awaitSignal(t, gate.entered)
	if cache.Stats().Loading != 2*cowBlockSize {
		t.Fatal("run did not reserve bounded Loading pages")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, err := cow.ReadAtContext(ctx, second, 0); secondDone <- err }()
	awaitSignal(t, waiting)
	cancel()
	if err := awaitError(t, secondDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("loading cancel: %v", err)
	}
	go func() { _, err := cow.ReadAt(second, 0); secondDone <- err }()
	awaitSignal(t, waiting)
	go func() { _, err := cow.WriteAt([]byte{9}, 4*cowBlockSize); writeDone <- err }()
	awaitSignal(t, waiting)
	gate.open()
	for _, done := range []<-chan error{firstDone, secondDone, writeDone} {
		if err := awaitError(t, done); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(first, want) || !bytes.Equal(second, want) {
		t.Fatal("overlapping read/eviction corrupted a loading or bypass page")
	}
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCOWCacheColdRunUnalignedAndShortRead(t *testing.T) {
	cache := testCache(t, 2, 1, cacheHooks{})
	cow := cachedCOW(t, cache, 2*maxWritebackPages+3, nil, WithDiffEncryption(testDiffKey(19), true))
	want := patternedBytes(int(cow.size))
	seedUpper(t, cow, 0, want)
	// Cross stripe zero and two batch boundaries with partial edges.
	off := int64(cowBlockSize - 512)
	got := make([]byte, 2*maxDiffScratchSize+513)
	if n, err := cow.ReadAt(got, off); err != nil || n != len(got) {
		t.Fatalf("unaligned read %d: %v", n, err)
	}
	if !bytes.Equal(got, want[off:off+int64(len(got))]) {
		t.Fatal("unaligned batched read mismatch")
	}
	cache.mu.Lock()
	for _, p := range cache.pages {
		cache.releaseLocked(p)
	}
	cache.mu.Unlock()
	cow.diff.(*diffFile).bodyIO = shortBatchRead{cow.diff.(*diffFile).bodyIO}
	if n, err := cow.ReadAt(got[:3*cowBlockSize], 0); n != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short run %d: %v", n, err)
	}
	checkBudget(t, cache, 0, 0, 0)
}

type shortBatchRead struct{ diffBodyIO }

func (s shortBatchRead) ReadAt(p []byte, off int64) (int, error) {
	return s.diffBodyIO.ReadAt(p[:cowBlockSize], off)
}

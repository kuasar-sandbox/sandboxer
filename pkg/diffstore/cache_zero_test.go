package diffstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

func requireActiveKinds(t *testing.T, cow *Disk, kinds []sparse.RunKind) {
	t.Helper()
	view, err := cow.Export(context.Background(), ActiveDiff)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	for i, want := range kinds {
		run, err := view.RunAt(uint64(i*cowBlockSize), cowBlockSize)
		if err != nil || run.Kind() != want {
			t.Fatalf("active page %d kind=%v err=%v, want=%v", i, run, err, want)
		}
	}
}

func TestCheckpointCachedZeroPublishesBeforeWritebackAndExports(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			gate := newWorkerGate()
			defer gate.open()
			var pause atomic.Bool
			cache := testCache(t, 8, 8, cacheHooks{beforeSelect: func() {
				if pause.Load() {
					gate.wait()
				}
			}})
			opts := []DiskOption{WithCheckpoint("cached-zero", "base")}
			if encrypted {
				opts = append(opts, WithDiffEncryption(testDiffKey(73), true))
			}
			base := &fakeReader{data: bytes.Repeat([]byte{0x6d}, 4*cowBlockSize)}
			cow := cachedCOW(t, cache, 4, base, opts...)
			path := cow.versions.path
			writePage(t, cow, 0, 0xa5)
			if err := cow.Checkpoint(context.Background(), "before"); err != nil {
				t.Fatal(err)
			}
			retained, err := cow.Export(context.Background(), "before")
			if err != nil {
				t.Fatal(err)
			}
			defer retained.Close()
			writtenBefore := cache.Stats().WrittenBytes
			pause.Store(true)
			if err := cow.Discard(0, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			awaitSignal(t, gate.entered)
			if err := cow.Discard(cowBlockSize, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			writePage(t, cow, 2, 0) // ordinary all-zero Data is deliberately distinct
			checkBudget(t, cache, 3, 3, 0)
			payload := make([]bool, 4)
			if err := cow.diff.payloadPresence(context.Background(), 0, payload); err != nil {
				t.Fatal(err)
			}
			if !payload[0] || payload[1] || payload[2] || payload[3] {
				t.Fatalf("ACK unexpectedly awaited physical writeback: %v", payload)
			}
			requireActiveKinds(t, cow, []sparse.RunKind{sparse.Zero, sparse.Zero, sparse.Data, sparse.Hole})
			view, _, err := cow.SnapshotView()
			if err != nil {
				t.Fatal(err)
			}
			upper, err := io.ReadAll(view)
			if err != nil || !bytes.Equal(upper, make([]byte, 4*cowBlockSize)) {
				t.Fatal("snapshot exposed old Data or lower bytes", err)
			}
			// Reuse dirty reservations in both directions, before the worker runs.
			if err := cow.Discard(2*cowBlockSize, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			if _, err := cow.WriteAt([]byte{0x71}, cowBlockSize+37); err != nil {
				t.Fatal(err)
			}
			writePage(t, cow, 3, 0)
			requireActiveKinds(t, cow, []sparse.RunKind{sparse.Zero, sparse.Data, sparse.Zero, sparse.Data})
			want := make([]byte, 4*cowBlockSize)
			want[cowBlockSize+37] = 0x71
			checkRead(t, cow, 0, want)
			checkBudget(t, cache, 4, 4, 0)
			gate.open()
			if err := cow.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := cow.diff.payloadPresence(context.Background(), 0, payload); err != nil {
				t.Fatal(err)
			}
			if payload[0] || !payload[1] || payload[2] || !payload[3] {
				t.Fatalf("completed mixed mapping kinds: %v", payload)
			}
			if got := cache.Stats().WrittenBytes - writtenBefore; got != 2*cowBlockSize {
				t.Fatalf("Zero was charged as payload I/O: %d", got)
			}
			old := make([]byte, 4*cowBlockSize)
			copy(old, bytes.Repeat([]byte{0xa5}, cowBlockSize))
			if !bytes.Equal(readExport(t, retained), old) {
				t.Fatal("accepted Zero or its writeback changed retained Data")
			}
			if err := retained.Close(); err != nil {
				t.Fatal(err)
			}
			if err := cow.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenDisk(path, base, DiffInit{Existing: true}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			checkRead(t, reopened, 0, want)
			requireActiveKinds(t, reopened, []sparse.RunKind{sparse.Zero, sparse.Data, sparse.Zero, sparse.Data})
		})
	}
}

func TestCheckpointCachedZeroWaitsForOlderWritebackAndOrdersPartialWrite(t *testing.T) {
	dataGate, zeroGate := newWorkerGate(), newWorkerGate()
	defer dataGate.open()
	defer zeroGate.open()
	waiting := make(chan struct{}, 16)
	var calls atomic.Int32
	cache := testCache(t, 2, 2, cacheHooks{
		beforeWrite: func(pages []*cachePage) error {
			switch calls.Add(1) {
			case 1:
				if pages[0].zero {
					t.Error("first writeback was not Data")
				}
				dataGate.wait()
			case 2:
				if !pages[0].zero {
					t.Error("second writeback was not Zero")
				}
				zeroGate.wait()
			}
			return nil
		},
		waiting: func() { waiting <- struct{}{} },
	})
	cow := cachedCOW(t, cache, 2, &fakeReader{data: bytes.Repeat([]byte{0x6d}, 2*cowBlockSize)}, WithCheckpoint("zero-order", "base"))
	writePage(t, cow, 0, 0xa1)
	awaitSignal(t, dataGate.entered)
	discarded := make(chan error, 1)
	go func() { discarded <- cow.Discard(0, cowBlockSize) }()
	awaitSignal(t, waiting)
	cache.mu.Lock()
	p := cache.pages[cacheKey{cow, 0}]
	unchanged := p.state == cacheWriteback && !p.zero && bytes.Equal(p.data[:], bytes.Repeat([]byte{0xa1}, cowBlockSize))
	cache.mu.Unlock()
	if !unchanged {
		t.Fatal("pending DISCARD changed a borrowed Data buffer")
	}
	dataGate.open()
	if err := awaitError(t, discarded); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, zeroGate.entered)
	checkRead(t, cow, 0, make([]byte, cowBlockSize))
	written := make(chan error, 1)
	go func() {
		_, err := cow.WriteAt([]byte{0xb2}, 19)
		written <- err
	}()
	awaitSignal(t, waiting)
	zeroGate.open()
	if err := awaitError(t, written); err != nil {
		t.Fatal(err)
	}
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, cowBlockSize)
	want[19] = 0xb2
	checkRead(t, cow, 0, want)
	checkRead(t, cow, 1, bytes.Repeat([]byte{0x6d}, cowBlockSize))
}

func TestCheckpointCachedZeroCloseDrainsAndCancelsQuotaWait(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	waiting := make(chan struct{}, 16)
	cache := testCache(t, 1, 1, cacheHooks{
		beforeWrite: func(pages []*cachePage) error {
			if !pages[0].zero {
				t.Error("discard did not stage explicit Zero")
			}
			gate.wait()
			return nil
		},
		waiting: func() { waiting <- struct{}{} },
	})
	base := &fakeReader{data: bytes.Repeat([]byte{0x6d}, 2*cowBlockSize)}
	cow := cachedCOW(t, cache, 2, base, WithCheckpoint("zero-close", "base"))
	path := cow.versions.path
	if err := cow.Discard(0, cowBlockSize); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, gate.entered)
	queued := make(chan error, 1)
	go func() { queued <- cow.Discard(cowBlockSize, cowBlockSize) }()
	awaitSignal(t, waiting)
	closed := make(chan error, 1)
	go func() { closed <- cow.Close() }()
	awaitSignal(t, cow.lifeCtx.Done())
	if err := awaitError(t, queued); !errors.Is(err, context.Canceled) {
		t.Fatal("Close did not cancel the quota waiter", err)
	}
	select {
	case err := <-closed:
		t.Fatal("Close returned before accepted Zero completed", err)
	default:
	}
	gate.open()
	if err := awaitError(t, closed); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, cache, 0, 0, 0)
	reopened, err := OpenDisk(path, base, DiffInit{Existing: true}, WithCheckpoint("zero-close", "base"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	checkRead(t, reopened, 0, make([]byte, cowBlockSize))
	checkRead(t, reopened, 1, base.data[cowBlockSize:])
}

func TestCheckpointCachedZeroWritebackFailureIsSticky(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	cache := testCache(t, 2, 2, cacheHooks{beforeWrite: func([]*cachePage) error {
		gate.wait()
		return io.ErrUnexpectedEOF
	}})
	cow := cachedCOW(t, cache, 2, nil, WithCheckpoint("zero-failure", ""))
	if err := cow.Discard(0, cowBlockSize); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, gate.entered)
	checkRead(t, cow, 0, make([]byte, cowBlockSize))
	gate.open()
	if err := cow.Drain(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if err := cow.Discard(cowBlockSize, cowBlockSize); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("new discard ignored sticky failure", err)
	}
	if _, err := cow.ReadAt(make([]byte, 1), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("read ignored sticky failure", err)
	}
	if s := cache.Stats(); s.DirtyUsed != cowBlockSize {
		t.Fatalf("failed Zero released its dirty charge: %+v", s)
	}
	if err := cow.Close(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("Close lost writeback failure", err)
	}
}

func TestCheckpointCachedZeroSameKindBatchBounds(t *testing.T) {
	const pages = 1280
	gate := newWorkerGate()
	defer gate.open()
	batches := make(chan [2]int, 16)
	cache := testCache(t, pages, pages, cacheHooks{
		beforeSelect: gate.wait,
		beforeWrite: func(batch []*cachePage) error {
			data := 0
			for _, p := range batch {
				if !p.zero {
					data++
				}
			}
			batches <- [2]int{len(batch), data}
			return nil
		},
	})
	cow := cachedCOW(t, cache, pages, nil, WithCheckpoint("zero-batches", ""))
	if err := cow.Discard(0, cowBlockSize); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, gate.entered)
	for block := 1; block <= 240; block++ {
		writePage(t, cow, block, 0x5a)
	}
	if err := cow.Discard(241*cowBlockSize, (pages-241)*cowBlockSize); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, cache, pages, pages, 0)
	gate.open()
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(batches) // Drain joined the beforeWrite callbacks for this owner.
	large := false
	for batch := range batches {
		if batch[0] > maxWritebackMutations || batch[1] > maxWritebackPages {
			t.Fatalf("unbounded Data/Zero batch: %v", batch)
		}
		if batch[1] != 0 && batch[1] != batch[0] {
			t.Fatalf("Data and Zero shared a backend batch: %v", batch)
		}
		large = large || batch[0] == maxWritebackMutations
	}
	if !large {
		t.Fatal("Zero did not enable a 1024-operation metadata batch")
	}
	if got := cache.Stats().WrittenBytes; got != 240*cowBlockSize {
		t.Fatalf("mixed payload accounting: %d", got)
	}
	want := make([]byte, pages*cowBlockSize)
	copy(want[cowBlockSize:], bytes.Repeat([]byte{0x5a}, 240*cowBlockSize))
	checkRead(t, cow, 0, want)
}

package uffd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"golang.org/x/sys/unix"
)

const unitCHVA = uint64(0x4000_0000)

func TestChunkRunBufferedTail(t *testing.T) {
	plain := bytes.Repeat([]byte{0x6c}, zeroFaultFillBytes)
	m := &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: sha256.Sum256(plain),
		}},
	}
	stream, getter := openSnapshotManifest(t, m, map[store.ContentKey][]byte{sha256.Sum256(plain): plain})
	source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	h := newUnitHandler(t, uint64(len(plain)), source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 9}, make([]byte, PageSize))
	waitUnitTail(t, h)

	if got := getter.chunkCalls.Load(); got != 1 {
		t.Fatalf("chunk Get calls = %d, want 1", got)
	}
	calls := ioctls.snapshot()
	if len(calls) != 2 || calls[0].kind != "copy" || calls[0].length != PageSize || calls[1].length != dataNeighborTailBytes {
		t.Fatalf("ioctl calls = %+v, want urgent 4KiB then buffered 4KiB COPY", calls)
	}
	if !bytes.Equal(calls[0].data, plain[:PageSize]) || !bytes.Equal(calls[1].data, plain[PageSize:2*PageSize]) {
		t.Fatal("buffered ChunkRun data was not reused for the tail")
	}
	for page := uint64(0); page < 2; page++ {
		if got := h.state.Get(page); got != StateLoaded {
			t.Fatalf("page %d state = %v, want Loaded", page, got)
		}
	}
	if got := h.state.Get(2); got != StateAbsent {
		t.Fatalf("page 2 state = %v, want Absent", got)
	}
	stats := h.Stats()
	if stats["source_read_calls"] != 1 || stats["source_read_bytes"] != dataFaultFillBytes {
		t.Fatalf("source metrics = %d calls/%d bytes", stats["source_read_calls"], stats["source_read_bytes"])
	}
	if stats["tail_buffered_data"] != 1 || stats["tail_pages_completed"] != 1 || stats["pages_copied"] != 2 {
		t.Fatalf("tail/copy metrics = %#v", stats)
	}
}

func TestChunkRunBusyReadsOnlyUrgentPage(t *testing.T) {
	plain := bytes.Repeat([]byte{0x2d}, 3*PageSize)
	m := &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: sha256.Sum256(plain),
		}},
	}
	stream, getter := openSnapshotManifest(t, m, map[store.ContentKey][]byte{sha256.Sum256(plain): plain})
	source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	h := newUnitHandler(t, uint64(len(plain)), source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()
	h.tailBusy.Store(true)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 9}, make([]byte, PageSize))
	h.tailBusy.Store(false)

	if got := getter.chunkCalls.Load(); got != 1 {
		t.Fatalf("chunk Get calls = %d, want 1", got)
	}
	stats := h.Stats()
	if stats["source_read_bytes"] != PageSize || stats["tail_dropped_busy"] != 1 || stats["tail_submitted"] != 0 {
		t.Fatalf("busy-path metrics = %#v", stats)
	}
	if h.state.Get(0) != StateLoaded || h.state.Get(1) != StateAbsent {
		t.Fatalf("states = %v/%v, want Loaded/Absent", h.state.Get(0), h.state.Get(1))
	}
	if calls := ioctls.snapshot(); len(calls) != 1 || calls[0].length != PageSize {
		t.Fatalf("ioctl calls = %+v", calls)
	}
}

func TestOrdinaryDataFaultReadsFourKiBBeforeDeferredTail(t *testing.T) {
	const pages = 20
	source := newRecordingSnapshot(pages*PageSize, sparse.Data, 0x91)
	h := newUnitHandler(t, pages*PageSize, source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 7}, make([]byte, PageSize))
	if got := source.readLengths(); !equalUint64s(got, []uint64{PageSize}) {
		t.Fatalf("foreground Run.ReadAt lengths = %v, want [4096]", got)
	}
	if got := source.runLimits(); !equalUint64s(got, []uint64{zeroFaultFillBytes}) {
		t.Fatalf("metadata RunAt limits = %v, want [%d]", got, zeroFaultFillBytes)
	}
	if !h.tailBusy.Load() || len(h.tailQ) != 1 {
		t.Fatalf("deferred tail reservation: busy=%v queued=%d", h.tailBusy.Load(), len(h.tailQ))
	}

	startUnitTail(t, h)
	waitUnitTail(t, h)
	if got := source.readLengths(); !equalUint64s(got, []uint64{PageSize, dataNeighborTailBytes}) {
		t.Fatalf("all Run.ReadAt lengths = %v", got)
	}
	if calls := ioctls.snapshot(); len(calls) != 2 || calls[0].length != PageSize || calls[1].length != dataNeighborTailBytes {
		t.Fatalf("ioctl calls = %+v", calls)
	}
	if h.state.Get(0) != StateLoaded || h.state.Get(1) != StateLoaded || h.state.Get(2) != StateAbsent {
		t.Fatalf("states = %v/%v/%v, want Loaded/Loaded/Absent", h.state.Get(0), h.state.Get(1), h.state.Get(2))
	}
}

func TestZeroTailStopsAtStateBoundary(t *testing.T) {
	const pages = 20
	h := newUnitHandler(t, pages*PageSize, ZeroSource{})
	h.state.Set(8, StateLoaded)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 5}, make([]byte, PageSize))
	startUnitTail(t, h)
	waitUnitTail(t, h)

	calls := ioctls.snapshot()
	if len(calls) != 2 || calls[0].length != PageSize || calls[1].length != 7*PageSize {
		t.Fatalf("zero calls = %+v, want urgent page then seven-page tail", calls)
	}
	for page := uint64(0); page <= 8; page++ {
		if got := h.state.Get(page); got != StateLoaded {
			t.Fatalf("page %d state = %v, want Loaded", page, got)
		}
	}
	if got := h.state.Get(9); got != StateAbsent {
		t.Fatalf("page 9 state = %v, want Absent", got)
	}
}

func TestZeroAndReleasedFaultsUseSerialZeroTail(t *testing.T) {
	for _, tt := range []struct {
		name     string
		expected PageState
	}{
		{name: "snapshot zero", expected: StateAbsent},
		{name: "released", expected: StateReleased},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const pages = 20
			h := newUnitHandler(t, pages*PageSize, ZeroSource{})
			if tt.expected == StateReleased {
				h.state.SetRange(0, pages, StateReleased)
			}
			ioctls := newFakeIoctls()
			h.ops = ioctls.ops()

			h.handleFault(faultEvent{address: unitCHVA, uffdFD: 5}, make([]byte, PageSize))
			calls := ioctls.snapshot()
			if len(calls) != 1 || calls[0].kind != "zero" || calls[0].length != PageSize {
				t.Fatalf("urgent calls = %+v", calls)
			}
			startUnitTail(t, h)
			waitUnitTail(t, h)
			calls = ioctls.snapshot()
			if len(calls) != 2 || calls[1].kind != "zero" || calls[1].length != zeroNeighborTailBytes {
				t.Fatalf("zero calls = %+v", calls)
			}
			if h.Stats()["source_read_calls"] != 0 {
				t.Fatal("zero path read source")
			}
			for page := uint64(0); page < zeroFaultFillBytes/PageSize; page++ {
				if h.state.Get(page) != StateLoaded {
					t.Fatalf("page %d was not populated", page)
				}
			}
			if page := uint64(zeroFaultFillBytes / PageSize); h.state.Get(page) != tt.expected {
				t.Fatalf("page %d = %v, want %v", page, h.state.Get(page), tt.expected)
			}
		})
	}
}

func TestConcurrentFaultsKeepSingleTailReservation(t *testing.T) {
	const pages = 64
	source := newRecordingSnapshot(pages*PageSize, sparse.Data, 0x4a)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	source.readHook = func(ctx context.Context, _ uint64, _ []byte) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h := newUnitHandler(t, pages*PageSize, source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()

	var wg sync.WaitGroup
	for _, page := range []uint64{0, 32} {
		wg.Add(1)
		go func(page uint64) {
			defer wg.Done()
			h.handleFault(faultEvent{address: unitCHVA + page*PageSize, uffdFD: 3}, make([]byte, PageSize))
		}(page)
	}
	receiveN(t, entered, 2)
	close(release)
	wg.Wait()
	if !h.tailBusy.Load() || len(h.tailQ) != 1 {
		t.Fatalf("tail reservation busy=%v queue=%d", h.tailBusy.Load(), len(h.tailQ))
	}
	stats := h.Stats()
	if stats["tail_submitted"] != 1 || stats["tail_dropped_busy"] != 1 {
		t.Fatalf("tail reservation metrics = %#v", stats)
	}
	if stats["fault_inflight_hwm"] != 2 {
		t.Fatalf("fault inflight HWM = %d, want 2", stats["fault_inflight_hwm"])
	}
	startUnitTail(t, h)
	waitUnitTail(t, h)
}

func TestFaultQueueDepthHWMCountsTotalAcrossQueues(t *testing.T) {
	h := newUnitHandler(t, 16*PageSize, newRecordingSnapshot(16*PageSize, sparse.Data, 0x31))
	h.queue = []chan faultEvent{make(chan faultEvent, 4), make(chan faultEvent, 4)}
	h.ops = newFakeIoctls().ops()
	startUnitTail(t, h)

	// Pick pages that hash to different worker queues, then dispatch 2+1
	// events: no single queue ever holds more than 2, so a per-queue max
	// would read 2 while the total-depth gauge must read 3.
	var a, b uint64
	for idx := uint64(0); a == 0 || b == 0; idx++ {
		switch pageIdxHash(idx) % uint64(len(h.queue)) {
		case 0:
			if a == 0 {
				a = idx
			}
		case 1:
			if b == 0 {
				b = idx
			}
		}
		if idx > 64 {
			t.Fatal("hash did not spread across two queues")
		}
	}

	var msg uffdMsg
	msg.Event = uffdEventPagefault
	for _, page := range []uint64{a, a, b} {
		binary.LittleEndian.PutUint64(msg.Arg[8:16], unitCHVA+page*PageSize)
		h.dispatch(&msg, 21)
	}
	if got := h.Stats()["fault_queue_depth_hwm"]; got != 3 {
		t.Fatalf("queue depth HWM = %d, want total of 3 across both queues", got)
	}
	if got := h.Stats()["fault_queue_depth"]; got != 3 {
		t.Fatalf("queue depth = %d, want 3", got)
	}

	h.wg.Add(2)
	for i := range h.queue {
		go h.runWorker(i)
	}
	// Drain before shutdown: each event increments exactly one
	// classification counter, so the sum reaching 3 proves every event
	// entered handleFault. Queue depth alone is not enough — a worker can
	// hold a received event before inflight is incremented, so depth and
	// inflight would transiently read as drained with an event pending.
	deadline := time.Now().Add(5 * time.Second)
	for {
		classified := h.stats.faultsAbsent.Load() +
			h.stats.faultsReleased.Load() +
			h.stats.faultsLoaded.Load()
		if classified >= 3 && h.stats.inflight.Load() == 0 && !h.tailBusy.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipeline did not drain: classified=%d/3 inflight=%d tailBusy=%v",
				classified, h.stats.inflight.Load(), h.tailBusy.Load())
		}
		time.Sleep(time.Millisecond)
	}
	close(h.stop)
	h.wg.Wait()

	stats := h.Stats()
	if stats["fault_queue_depth"] != 0 || stats["fault_queue_depth_hwm"] != 3 || stats["errors"] != 0 {
		t.Fatalf("post-drain queue metrics = %#v", stats)
	}
}

func TestFaultQueueWaitDepthAndInflightMetrics(t *testing.T) {
	h := newUnitHandler(t, PageSize, newRecordingSnapshot(PageSize, sparse.Data, 0x17))
	h.queue = []chan faultEvent{make(chan faultEvent, 4)}
	ioctls := newFakeIoctls()
	var completed atomic.Int32
	done := make(chan struct{})
	record := func() {
		if completed.Add(1) == 3 {
			close(done)
		}
	}
	ioctls.copyHook = func(_ int, _ uint64, data []byte) (int64, error) {
		record()
		return int64(len(data)), nil
	}
	ioctls.zeroHook = func(_ int, _ uint64, length uint64) (int64, error) {
		record()
		return int64(length), nil
	}
	h.ops = ioctls.ops()

	var msg uffdMsg
	msg.Event = uffdEventPagefault
	binary.LittleEndian.PutUint64(msg.Arg[8:16], unitCHVA)
	for range 3 {
		h.dispatch(&msg, 21)
	}
	if got := h.Stats()["fault_queue_depth_hwm"]; got != 3 {
		t.Fatalf("queue depth HWM before worker = %d, want 3", got)
	}
	h.wg.Add(1)
	go h.runWorker(0)
	receiveSignal(t, done)
	close(h.stop)
	h.wg.Wait()

	stats := h.Stats()
	if stats["fault_queue_wait_ns"] == 0 || stats["fault_queue_wait_p50"] == 0 || stats["fault_queue_wait_p95"] == 0 || stats["fault_queue_wait_p99"] == 0 {
		t.Fatalf("queue wait metrics = %#v", stats)
	}
	if stats["fault_inflight_hwm"] != 1 || stats["fault_queue_depth"] != 0 {
		t.Fatalf("queue/inflight gauges = %#v", stats)
	}
}

func TestUrgentConflictConvergence(t *testing.T) {
	for _, tt := range []struct {
		name      string
		err       error
		wantState PageState
	}{
		{name: "EEXIST", err: unix.EEXIST, wantState: StateLoaded},
		{name: "EAGAIN", err: unix.EAGAIN, wantState: StateAbsent},
		{name: "ENOENT", err: unix.ENOENT, wantState: StateAbsent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := newRecordingSnapshot(2*PageSize, sparse.Data, 0x38)
			h := newUnitHandler(t, 2*PageSize, source)
			ioctls := newFakeIoctls()
			ioctls.copyHook = func(int, uint64, []byte) (int64, error) { return 0, tt.err }
			h.ops = ioctls.ops()
			h.handleFault(faultEvent{address: unitCHVA, uffdFD: 4}, make([]byte, PageSize))
			if got := h.state.Get(0); got != tt.wantState {
				t.Fatalf("state = %v, want %v", got, tt.wantState)
			}
			stats := h.Stats()
			if stats["wakes"] != 1 || stats["errors"] != 0 || stats["pages_copied"] != 0 {
				t.Fatalf("conflict metrics = %#v", stats)
			}
			if h.tailBusy.Load() {
				t.Fatal("urgent conflict left tail reserved")
			}
		})
	}
}

func TestStaleAbsentFaultConvergesWithoutError(t *testing.T) {
	source := newRecordingSnapshot(2*PageSize, sparse.Data, 0x28)
	h := newUnitHandler(t, 2*PageSize, source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()

	// handleAbsentFault is entered only after handleFault observed Absent.
	// Model a tail completion changing the page before stateHardEnd scans it.
	h.state.Set(0, StateLoaded)
	h.handleAbsentFault(faultEvent{address: unitCHVA, uffdFD: 7}, make([]byte, PageSize))

	stats := h.Stats()
	if stats["errors"] != 0 || stats["wakes"] != 1 {
		t.Fatalf("stale fault metrics = %#v, want one wake and no error", stats)
	}
	if got := source.runLimits(); len(got) != 0 {
		t.Fatalf("stale fault queried source with limits %v", got)
	}
	if calls := ioctls.snapshot(); len(calls) != 0 {
		t.Fatalf("stale fault issued data ioctl calls: %+v", calls)
	}
	if got := h.state.Get(0); got != StateLoaded {
		t.Fatalf("stale fault changed state to %v", got)
	}
}

func TestTailNoCompletionLeavesNeighborAbsent(t *testing.T) {
	source := newRecordingSnapshot(4*PageSize, sparse.Data, 0x59)
	h := newUnitHandler(t, 4*PageSize, source)
	ioctls := newFakeIoctls()
	ioctls.copyHook = func(_ int, dst uint64, data []byte) (int64, error) {
		length := uint64(len(data))
		if dst == unitCHVA+PageSize {
			return 0, unix.EAGAIN
		}
		return int64(length), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 6}, make([]byte, PageSize))
	waitUnitTail(t, h)
	if h.state.Get(0) != StateLoaded || h.state.Get(1) != StateAbsent || h.state.Get(2) != StateAbsent {
		t.Fatalf("states = %v/%v/%v, want Loaded/Absent/Absent", h.state.Get(0), h.state.Get(1), h.state.Get(2))
	}
	stats := h.Stats()
	if stats["pages_copied"] != 1 || stats["tail_pages_completed"] != 0 || stats["tail_partial"] != 1 || stats["tail_conflicts"] != 1 || stats["errors"] != 0 {
		t.Fatalf("tail completion metrics = %#v", stats)
	}
}

func TestZeroTailNoCompletionLeavesNeighborAbsent(t *testing.T) {
	h := newUnitHandler(t, 4*PageSize, ZeroSource{})
	ioctls := newFakeIoctls()
	ioctls.zeroHook = func(_ int, dst uint64, length uint64) (int64, error) {
		if dst == unitCHVA+PageSize {
			return 0, unix.EAGAIN
		}
		return int64(length), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 6}, make([]byte, PageSize))
	waitUnitTail(t, h)
	if h.state.Get(0) != StateLoaded || h.state.Get(1) != StateAbsent || h.state.Get(2) != StateAbsent {
		t.Fatalf("zero states = %v/%v/%v, want Loaded/Absent/Absent", h.state.Get(0), h.state.Get(1), h.state.Get(2))
	}
	stats := h.Stats()
	if stats["pages_zeroed"] != 1 || stats["tail_pages_completed"] != 0 || stats["tail_partial"] != 1 || stats["tail_conflicts"] != 1 || stats["errors"] != 0 {
		t.Fatalf("zero tail completion metrics = %#v", stats)
	}
}

func TestUrgentRejectsInvalidIoctlCompletion(t *testing.T) {
	for _, completed := range []int64{-1, 1, 2 * PageSize} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			h := newUnitHandler(t, PageSize, newRecordingSnapshot(PageSize, sparse.Data, 0x29))
			ioctls := newFakeIoctls()
			ioctls.copyHook = func(int, uint64, []byte) (int64, error) { return completed, nil }
			h.ops = ioctls.ops()
			h.handleFault(faultEvent{address: unitCHVA, uffdFD: 17}, make([]byte, PageSize))
			if got := h.state.Get(0); got != StateAbsent {
				t.Fatalf("invalid completion changed state to %v", got)
			}
			if h.Stats()["errors"] != 1 || h.Stats()["pages_copied"] != 0 {
				t.Fatalf("invalid completion metrics = %#v", h.Stats())
			}
		})
	}
}

func TestEventRemoveWinsTailConditionalCommit(t *testing.T) {
	source := newRecordingSnapshot(4*PageSize, sparse.Data, 0x77)
	h := newUnitHandler(t, 4*PageSize, source)
	ioctls := newFakeIoctls()
	ioctls.copyHook = func(_ int, dst uint64, data []byte) (int64, error) {
		length := uint64(len(data))
		if dst == unitCHVA+PageSize {
			h.state.SetRange(1, 4, StateReleased)
		}
		return int64(length), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 6}, make([]byte, PageSize))
	waitUnitTail(t, h)
	for page := uint64(1); page < 4; page++ {
		if got := h.state.Get(page); got != StateReleased {
			t.Fatalf("stale tail changed page %d to %v", page, got)
		}
	}
	if h.Stats()["tail_conflicts"] == 0 {
		t.Fatal("EVENT_REMOVE race did not record tail conflict")
	}
}

func TestUrgentWinsRunningTail(t *testing.T) {
	const pages = 20
	source := newRecordingSnapshot(pages*PageSize, sparse.Data, 0x45)
	h := newUnitHandler(t, pages*PageSize, source)
	tailEntered := make(chan struct{})
	releaseTail := make(chan struct{})
	var tailBlocked atomic.Bool
	ioctls := newFakeIoctls()
	ioctls.copyHook = func(_ int, dst uint64, data []byte) (int64, error) {
		length := uint64(len(data))
		if dst == unitCHVA+PageSize && tailBlocked.CompareAndSwap(false, true) {
			close(tailEntered)
			<-releaseTail
			return 0, unix.EEXIST
		}
		return int64(length), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 8}, make([]byte, PageSize))
	receiveSignal(t, tailEntered)
	h.handleFault(faultEvent{address: unitCHVA + PageSize, uffdFD: 8}, make([]byte, PageSize))
	close(releaseTail)
	waitUnitTail(t, h)
	if h.state.Get(1) != StateLoaded {
		t.Fatalf("urgent competitor state = %v", h.state.Get(1))
	}
	stats := h.Stats()
	if stats["tail_conflicts"] == 0 || stats["tail_dropped_busy"] == 0 || stats["errors"] != 0 {
		t.Fatalf("urgent/tail race metrics = %#v", stats)
	}
}

func TestFaultRunBoundStopsAtCHRegion(t *testing.T) {
	for _, kind := range []sparse.RunKind{sparse.Data, sparse.Zero} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			const pages = 4
			source := newRecordingSnapshot(pages*PageSize, kind, 0x63)
			h := newUnitHandlerWithoutMap(t, pages*PageSize, source)
			if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA, 2*PageSize, 0); err != nil {
				t.Fatal(err)
			}
			if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA+0x10_0000, 2*PageSize, 2*PageSize); err != nil {
				t.Fatal(err)
			}
			ioctls := newFakeIoctls()
			h.ops = ioctls.ops()

			h.handleFault(faultEvent{address: unitCHVA + PageSize, uffdFD: 11}, make([]byte, PageSize))
			if got := source.runLimits(); !equalUint64s(got, []uint64{PageSize}) {
				t.Fatalf("RunAt limits = %v, want one page at split", got)
			}
			if h.tailBusy.Load() || len(h.tailQ) != 0 {
				t.Fatal("fault crossed CH region into a tail")
			}
		})
	}
}

func TestCheckedCompletion(t *testing.T) {
	for _, tt := range []struct {
		name      string
		completed int64
		requested uint64
		want      uint64
		wantErr   bool
	}{
		{name: "zero", completed: 0, requested: PageSize},
		{name: "full", completed: PageSize, requested: PageSize, want: PageSize},
		{name: "partial pages", completed: PageSize, requested: 2 * PageSize, want: PageSize},
		{name: "negative", completed: -1, requested: PageSize, wantErr: true},
		{name: "too large", completed: 2 * PageSize, requested: PageSize, wantErr: true},
		{name: "unaligned", completed: 1, requested: PageSize, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkedCompletion(tt.completed, tt.requested)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("checkedCompletion = (%d,%v), want (%d, err=%v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

type recordingSnapshot struct {
	size uint64
	kind sparse.RunKind
	fill byte

	mu       sync.Mutex
	reads    []uint64
	limits   []uint64
	readHook func(context.Context, uint64, []byte) error
	readErr  error
}

func newRecordingSnapshot(size uint64, kind sparse.RunKind, fill byte) *recordingSnapshot {
	return &recordingSnapshot{size: size, kind: kind, fill: fill}
}

func (s *recordingSnapshot) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.size {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, errors.New("zero limit")
	}
	s.mu.Lock()
	s.limits = append(s.limits, limit)
	s.mu.Unlock()
	end := s.size
	if limit < s.size-offset {
		end = offset + limit
	}
	return recordingRun{source: s, offset: offset, end: end, kind: s.kind}, nil
}

func (s *recordingSnapshot) readLengths() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.reads...)
}

func (s *recordingSnapshot) runLimits() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.limits...)
}

type recordingRun struct {
	source *recordingSnapshot
	offset uint64
	end    uint64
	kind   sparse.RunKind
}

func (r recordingRun) Offset() uint64       { return r.offset }
func (r recordingRun) End() uint64          { return r.end }
func (r recordingRun) Kind() sparse.RunKind { return r.kind }

func (r recordingRun) ReadAt(ctx context.Context, buf []byte, innerOffset uint64) (int, error) {
	length := r.end - r.offset
	if innerOffset > length || uint64(len(buf)) > length-innerOffset {
		return 0, fmt.Errorf("recording Run read outside [0,%d)", length)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.kind != sparse.Data {
		clear(buf)
		return len(buf), nil
	}
	r.source.mu.Lock()
	r.source.reads = append(r.source.reads, uint64(len(buf)))
	hook := r.source.readHook
	err := r.source.readErr
	r.source.mu.Unlock()
	if hook != nil {
		if hookErr := hook(ctx, r.offset+innerOffset, buf); hookErr != nil {
			return 0, hookErr
		}
	}
	if err != nil {
		return 0, err
	}
	for i := range buf {
		buf[i] = r.source.fill
	}
	return len(buf), nil
}

type fakeIoctlCall struct {
	kind   string
	fd     int
	dst    uint64
	length uint64
	data   []byte
}

type fakeIoctls struct {
	mu       sync.Mutex
	calls    []fakeIoctlCall
	wakes    []fakeIoctlCall
	copyHook func(int, uint64, []byte) (int64, error)
	zeroHook func(int, uint64, uint64) (int64, error)
}

func newFakeIoctls() *fakeIoctls { return &fakeIoctls{} }

func (f *fakeIoctls) ops() uffdOps {
	return uffdOps{
		copy: func(fd int, dst uint64, src []byte) (int64, error) {
			data := append([]byte(nil), src...)
			length := uint64(len(src))
			f.mu.Lock()
			f.calls = append(f.calls, fakeIoctlCall{kind: "copy", fd: fd, dst: dst, length: length, data: data})
			hook := f.copyHook
			f.mu.Unlock()
			if hook != nil {
				return hook(fd, dst, src)
			}
			return int64(length), nil
		},
		zeropage: func(fd int, dst, length uint64) (int64, error) {
			f.mu.Lock()
			f.calls = append(f.calls, fakeIoctlCall{kind: "zero", fd: fd, dst: dst, length: length})
			hook := f.zeroHook
			f.mu.Unlock()
			if hook != nil {
				return hook(fd, dst, length)
			}
			return int64(length), nil
		},
		wake: func(fd int, start, length uint64) error {
			f.mu.Lock()
			f.wakes = append(f.wakes, fakeIoctlCall{kind: "wake", fd: fd, dst: start, length: length})
			f.mu.Unlock()
			return nil
		},
	}
}

func (f *fakeIoctls) snapshot() []fakeIoctlCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeIoctlCall(nil), f.calls...)
}

func newUnitHandler(t *testing.T, size uint64, source SnapshotReader) *Handler {
	t.Helper()
	h := newUnitHandlerWithoutMap(t, size, source)
	if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA, size, 0); err != nil {
		t.Fatal(err)
	}
	return h
}

func newUnitHandlerWithoutMap(t *testing.T, size uint64, source SnapshotReader) *Handler {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{
		cfg: Config{
			Size:   int(size),
			Source: source,
		},
		addrMap:  NewAddressMap(size),
		state:    NewPageStateMap(int(size / PageSize)),
		logf:     t.Logf,
		stop:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
		tailQ:    make(chan tailTask, 1),
		tailBuf:  make([]byte, dataFaultFillBytes),
		tailIdle: make(chan struct{}, 1),
	}
	return h
}

func startUnitTail(t *testing.T, h *Handler) {
	t.Helper()
	h.tailWG.Add(1)
	go h.runTailWorker()
	t.Cleanup(func() {
		h.closing.Store(true)
		h.tailSubmit.Lock()
		h.cancel()
		h.tailSubmit.Unlock()
		h.waitTailIdle()
		h.tailWG.Wait()
	})
}

func waitUnitTail(t *testing.T, h *Handler) {
	t.Helper()
	if !h.tailBusy.Load() {
		return
	}
	select {
	case <-h.tailIdle:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tail reservation release")
	}
	if h.tailBusy.Load() {
		t.Fatal("tail idle notification arrived while still busy")
	}
}

func receiveSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for signal")
	}
}

func receiveN(t *testing.T, ch <-chan struct{}, n int) {
	t.Helper()
	for range n {
		receiveSignal(t, ch)
	}
}

func equalUint64s(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

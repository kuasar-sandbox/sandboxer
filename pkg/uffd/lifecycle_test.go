package uffd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"golang.org/x/sys/unix"
)

func TestHandlerCloseJoinsReservedButNotEnqueuedTail(t *testing.T) {
	var pipeFDs [2]int
	if err := unix.Pipe2(pipeFDs[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipeFDs[1])
	memfd, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer memfd.Close()

	addrMap := NewAddressMap(2 * PageSize)
	if err := addrMap.RegisterVMA(ProcessCH, unitCHVA, 2*PageSize, 0); err != nil {
		t.Fatal(err)
	}
	if err := addrMap.RegisterVMA(ProcessBackend, unitCHVA+0x10_0000, 2*PageSize, 0); err != nil {
		t.Fatal(err)
	}
	h, err := NewWithBackendUffd(pipeFDs[0], addrMap, Config{
		MemfdFD:    int(memfd.Fd()),
		BackendVA:  uintptr(unitCHVA + 0x10_0000),
		Size:       2 * PageSize,
		Source:     ZeroSource{},
		NumWorkers: MinWorkers,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Start()
	if !h.tryReserveTail() {
		t.Fatal("failed to reserve test tail slot")
	}
	enqueueDone := make(chan struct{})
	go func() {
		<-h.ctx.Done()
		h.enqueueReservedTail(tailTask{kind: tailBufferedData})
		close(enqueueDone)
	}()

	closeDone := make(chan error, 1)
	go func() { closeDone <- h.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Handler.Close did not join reserved tail")
	}
	receiveSignal(t, enqueueDone)
	if h.tailBusy.Load() {
		t.Fatal("Close left tailBusy reserved")
	}
	if h.Stats()["tail_canceled"] == 0 {
		t.Fatal("reserved tail cancellation was not recorded")
	}
}

func TestDeferredRunReadCanceledReleasesTail(t *testing.T) {
	source := newRecordingSnapshot(20*PageSize, sparse.Data, 0x23)
	tailReadStarted := make(chan struct{})
	source.readHook = func(ctx context.Context, _ uint64, buf []byte) error {
		if len(buf) == PageSize {
			return nil
		}
		close(tailReadStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	h := newUnitHandler(t, 20*PageSize, source)
	h.ops = newFakeIoctls().ops()
	h.tailWG.Add(1)
	go h.runTailWorker()

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 12}, make([]byte, PageSize))
	receiveSignal(t, tailReadStarted)
	h.closing.Store(true)
	h.tailSubmit.Lock()
	h.cancel()
	h.tailSubmit.Unlock()
	h.waitTailIdle()
	h.tailWG.Wait()

	if h.tailBusy.Load() {
		t.Fatal("canceled deferred read left tailBusy set")
	}
	if h.Stats()["tail_canceled"] == 0 {
		t.Fatal("deferred read cancellation was not recorded")
	}
}

func TestDeferredRunReadFailureIsBestEffort(t *testing.T) {
	source := newRecordingSnapshot(20*PageSize, sparse.Data, 0x42)
	source.readHook = func(_ context.Context, _ uint64, buf []byte) error {
		if len(buf) > PageSize {
			return errors.New("injected deferred read failure")
		}
		return nil
	}
	h := newUnitHandler(t, 20*PageSize, source)
	h.ops = newFakeIoctls().ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 12}, make([]byte, PageSize))
	waitUnitTail(t, h)
	if h.tailBusy.Load() {
		t.Fatal("failed deferred read left tailBusy set")
	}
	if h.state.Get(0) != StateLoaded || h.state.Get(1) != StateAbsent {
		t.Fatalf("states after deferred failure = %v/%v", h.state.Get(0), h.state.Get(1))
	}
	if h.Stats()["errors"] != 0 {
		t.Fatalf("best-effort tail failure raised handler errors: %#v", h.Stats())
	}
}

func TestChunkReservationReleasedOnEveryUrgentExit(t *testing.T) {
	tests := []struct {
		name       string
		pages      int
		withChunk  bool
		urgentErr  error
		wantErrors uint64
	}{
		{name: "chunk read error", pages: 2, withChunk: false, wantErrors: 1},
		{name: "single page", pages: 1, withChunk: true},
		{name: "urgent EEXIST", pages: 2, withChunk: true, urgentErr: unix.EEXIST},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plain := bytes.Repeat([]byte{0x35}, tt.pages*PageSize)
			hash := sha256.Sum256(plain)
			m := &codec.Manifest{
				Version:   codec.Version1,
				ImageSize: uint64(len(plain)),
				Entries: []codec.ChunkEntry{{
					Offset:         0,
					Size:           uint32(len(plain)),
					CiphertextHash: hash,
				}},
			}
			chunks := map[store.ContentKey][]byte(nil)
			if tt.withChunk {
				chunks = map[store.ContentKey][]byte{hash: plain}
			}
			stream, _ := openSnapshotManifest(t, m, chunks)
			source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
			if err != nil {
				t.Fatal(err)
			}
			h := newUnitHandler(t, uint64(len(plain)), source)
			ioctls := newFakeIoctls()
			if tt.urgentErr != nil {
				ioctls.copyHook = func(int, uint64, []byte) (int64, error) {
					return 0, tt.urgentErr
				}
			}
			h.ops = ioctls.ops()

			h.handleFault(faultEvent{address: unitCHVA, uffdFD: 13}, make([]byte, PageSize))
			if h.tailBusy.Load() || len(h.tailQ) != 0 {
				t.Fatalf("exit left reservation: busy=%v queued=%d", h.tailBusy.Load(), len(h.tailQ))
			}
			if got := h.Stats()["errors"]; got != tt.wantErrors {
				t.Fatalf("errors = %d, want %d", got, tt.wantErrors)
			}
		})
	}
}

package uffd

import (
	"bytes"
	"context"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"golang.org/x/sys/unix"
)

// dispatchFirstPageFault dispatches one event on the registered shmem mapping.
// Tests that assert clean installation need to exclude independently queued
// duplicate faults, whose EEXIST is intentionally tracked as dirty.
func dispatchFirstPageFault(t *testing.T, h *Handler) {
	t.Helper()
	va, fd, covered, err := h.addrMap.registration(0, PageSize)
	if err != nil || fd <= 0 || covered != PageSize {
		t.Fatalf("invalid CH registration: fd=%d covered=%d err=%v", fd, covered, err)
	}
	h.handleFault(faultEvent{address: va, uffdFD: fd}, make([]byte, PageSize))
	if h.state.Get(0) != StateLoaded {
		t.Fatal("first page remains uninstalled")
	}
}

func TestRealMissingCleanFillRecordedWriteAndReleaseCapture(t *testing.T) {
	h, backend, cpu := realShmemHandler(t, 32, newRecordingSnapshot(32*PageSize, sparse.Data, 0x37))
	dispatchFirstPageFault(t, h)
	if cpu[0] != 0x37 {
		t.Fatal("urgent fill content")
	}
	if err := h.EnsureLoaded(context.Background(), 20*PageSize, PageSize); err != nil {
		t.Fatal(err)
	}
	v, err := h.CaptureView()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range v.Dirty {
		if b != 0 {
			t.Fatalf("ordinary materialization invented dirty: %x", v.Dirty)
		}
	}
	// A backend writer supplies D before modifying its already loaded alias.
	if err := h.cfg.Dirty.Mark(0, 1); err != nil {
		t.Fatal(err)
	}
	backend[0] = 0x71
	if backend[0] != 0x71 {
		t.Fatal("recorded write content")
	}
	v, err = h.CaptureView()
	if err != nil {
		t.Fatal(err)
	}
	if v.Dirty[0]&1 == 0 {
		t.Fatal("recorded write lost")
	}
	// Prove this nonzero parent page is still unmaterialized before release.
	if v.Materialized[3]&1 != 0 {
		t.Fatal("release fixture page was already materialized")
	}
	// Real release ordering, including an already sparse parent page.
	if err := unix.Madvise(cpu[24*PageSize:25*PageSize], unix.MADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
	waitRemove(t, h)
	if err := unix.Fallocate(h.cfg.MemfdFD, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 24*PageSize, PageSize); err != nil {
		t.Fatal(err)
	}
	v, err = h.CaptureView()
	if err != nil {
		t.Fatal(err)
	}
	if v.Released[3]&1 == 0 || v.Materialized[3]&1 != 0 {
		t.Fatalf("released hole: %+v", v)
	}
	// Feed this exact real R/M/D through C/W and the real S carrier, before
	// refilling the released page. No CPU writer has touched page 24.
	for _, working := range []bool{false, true} {
		selected, dirty, err := snapshot.CaptureMemory(bytes.NewReader(backend), v, working)
		if err != nil {
			t.Fatal(err)
		}
		run, err := selected.RunAt(24*PageSize, PageSize)
		if err != nil || run.Kind() != sparse.Zero {
			t.Fatalf("necessary released Zero lost: %v %v", run, err)
		}
		source, err := snapshotfile.BuildSourceWithDirty(selected, []byte(`{}`), []byte(`{}`), []byte(`{"version":1}`), dirty)
		if err != nil {
			t.Fatal(err)
		}
		var carrier bytes.Buffer
		if _, _, err = tarstream.WriteTo(context.Background(), &carrier, "snapshot", source); err != nil {
			t.Fatal(err)
		}
		encoded, _, err := tarstream.SourceAt(bytes.NewReader(carrier.Bytes()), int64(carrier.Len()), "")
		if err != nil {
			t.Fatal(err)
		}
		root, err := snapshotfile.Open(context.Background(), captureTestStream{encoded})
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		base := bytes.Repeat([]byte{0x37}, 32*PageSize)
		parent, _ := sparse.NewSource(bytes.NewReader(base), uint64(len(base)), nil)
		full := fetch.NewLayered(root.Memory, captureTestStream{parent})
		want := append([]byte(nil), base...)
		want[0] = 0x71
		clear(want[24*PageSize : 25*PageSize])
		got := make([]byte, len(want))
		if _, err = full.ReadAt(context.Background(), got, 0); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("real release carrier working=%t content mismatch: %v", working, err)
		}
	}
	if err := h.EnsureLoaded(context.Background(), 24*PageSize, PageSize); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backend[24*PageSize:25*PageSize], make([]byte, PageSize)) {
		t.Fatal("released parent refilled stale bytes")
	}
	if err := h.cfg.Dirty.Mark(24*PageSize, 1); err != nil {
		t.Fatal(err)
	}
	backend[24*PageSize] = 0x5d
	v, err = h.CaptureView()
	if err != nil {
		t.Fatal(err)
	}
	if v.Released[3]&1 == 0 || v.Materialized[3]&1 == 0 || v.Dirty[3]&1 == 0 {
		t.Fatal("reuse cleared R/D or lost Data")
	}
}
func TestRealMissingZeroAndTailDoNotInventDirtyWrites(t *testing.T) {
	for _, source := range []SnapshotReader{ZeroSource{}, newRecordingSnapshot(32*PageSize, sparse.Zero, 0), newRecordingSnapshot(32*PageSize, sparse.Hole, 0)} {
		h, backend, cpu := realShmemHandler(t, 32, source)
		dispatchFirstPageFault(t, h)
		if cpu[0] != 0 {
			t.Fatal("zero content")
		}
		if err := h.EnsureLoaded(context.Background(), 20*PageSize, PageSize); err != nil {
			t.Fatal(err)
		}
		v, err := h.CaptureView()
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range v.Dirty {
			if b != 0 {
				t.Fatalf("clean Zero/Hole dirty: %x", v.Dirty)
			}
		}
		if err := h.cfg.Dirty.Mark(20*PageSize, 1); err != nil {
			t.Fatal(err)
		}
		backend[20*PageSize] = 9
		if backend[20*PageSize] != 9 {
			t.Fatal("zero clean page not writable")
		}
		v, err = h.CaptureView()
		if err != nil {
			t.Fatal(err)
		}
		if v.Dirty[2]&(1<<4) == 0 {
			t.Fatal("recorded zero-page write lost")
		}
	}
}

func TestRealMissingDuplicateFaultPreservesResidentWrite(t *testing.T) {
	h, backend, cpu := realShmemHandler(t, 1, ZeroSource{})
	// EnsureLoaded installs an actual folio before the stale event arrives.
	if err := h.EnsureLoaded(context.Background(), 0, PageSize); err != nil {
		t.Fatal(err)
	}
	backend[0] = 0x57
	dispatchFirstPageFault(t, h)
	v, err := h.CaptureView()
	if err != nil || !bytes.Equal(v.Materialized, []byte{1}) ||
		!bytes.Equal(v.Dirty, []byte{1}) || !bytes.Equal(v.Released, []byte{0}) || cpu[0] != 0x57 {
		t.Fatalf("duplicate fault lost live data: view=%+v cpu=%x err=%v", v, cpu[0], err)
	}
	for _, working := range []bool{false, true} {
		selected, _, err := snapshot.CaptureMemory(bytes.NewReader(backend), v, working)
		if err != nil {
			t.Fatal(err)
		}
		run, err := selected.RunAt(0, PageSize)
		if err != nil || run.Kind() != sparse.Data {
			t.Fatalf("missing Data: run=%v err=%v", run, err)
		}
		buf := make([]byte, PageSize)
		if _, err := run.ReadAt(context.Background(), buf, 0); err != nil || !bytes.Equal(buf, backend) {
			t.Fatalf("capture failed to preserve resident contents: %v", err)
		}
	}
}

func TestCaptureIncludesKnownLoadedHoleAsZero(t *testing.T) {
	h, _, _ := realShmemHandler(t, 1, ZeroSource{})
	h.installMu.Lock()
	h.state.Set(0, StateLoaded)
	h.installMu.Unlock()
	v, err := h.CaptureView()
	if err != nil || v.Released[0]&1 == 0 || v.Dirty[0]&1 == 0 || v.Materialized[0]&1 != 0 {
		t.Fatalf("Loaded hole must retain Zero: view=%+v err=%v", v, err)
	}
}

func TestMissingHandlerRejectsOtherFaultKindsWithoutFilling(t *testing.T) {
	for name, flags := range map[string]uint64{"write-protect": uffdPagefaultFlagWP, "minor": uffdPagefaultFlagMinor} {
		t.Run(name, func(t *testing.T) {
			h := newUnitHandler(t, PageSize, ZeroSource{})
			defer h.cancel()
			var fatal error
			h.cfg.ReadFatal = func(err error) { fatal = err }
			h.ops = uffdOps{
				copy:     func(int, uint64, []byte) (int64, error) { t.Fatal("unexpected copy"); return 0, nil },
				zeropage: func(int, uint64, uint64) (int64, error) { t.Fatal("unexpected zero"); return 0, nil },
				wake:     func(int, uint64, uint64) error { t.Fatal("unexpected wake"); return nil },
			}
			h.handleFault(faultEvent{uffdFD: unitFD, address: unitCHVA, flags: flags}, nil)
			if fatal == nil || h.ctx.Err() == nil || h.state.Get(0) != StateAbsent {
				t.Fatal("unsupported fault did not fail closed")
			}
		})
	}
}

type captureTestStream struct{ sparse.Source }

func (captureTestStream) Close() error { return nil }

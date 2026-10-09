package uffd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"golang.org/x/sys/unix"
)

// Pipes exercise real epoll ownership and shutdown while ioctl results remain
// injectable. The real shared-memfd regression lives in pkg/vhost.
func loadHandler(t *testing.T, pages, registered int, source SnapshotReader, ops uffdOps) (*Handler, int) {
	t.Helper()
	fds := loadPipe(t)
	memfd, err := unix.MemfdCreate("load-unit", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(memfd) })
	m := NewAddressMap(uint64(pages * PageSize))
	if err := m.RegisterVMA(ProcessCH, unitCHVA, uint64(registered*PageSize), 0); err != nil {
		t.Fatal(err)
	}
	h, err := NewWithBackendUffd(fds[0], m, Config{MemfdFD: memfd, Size: pages * PageSize, Source: source, NumWorkers: 2})
	if err != nil {
		unix.Close(fds[0])
		t.Fatal(err)
	}
	h.ops = ops
	h.Start()
	t.Cleanup(func() { h.Close() })
	return h, fds[0]
}

func loadPipe(t *testing.T) [2]int {
	t.Helper()
	var fds [2]int
	if err := unix.Pipe2(fds[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fds[1]) })
	return fds
}

func loadResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("load did not finish")
		return nil
	}
}

func TestEnsureLoadedRangesAndLiveFastPath(t *testing.T) {
	source := newRecordingSnapshot(4*PageSize, sparse.Data, 0xa5)
	ops := newFakeIoctls()
	h, _ := loadHandler(t, 4, 4, source, ops.ops())
	if err := h.EnsureLoaded(context.Background(), PageSize-1, PageSize+2); err != nil {
		t.Fatal(err)
	}
	calls := ops.snapshot()
	if len(calls) != 3 {
		t.Fatalf("copies=%d want 3 intersecting pages", len(calls))
	}
	for i, c := range calls {
		if c.dst != unitCHVA+uint64(i*PageSize) || !bytes.Equal(c.data, bytes.Repeat([]byte{0xa5}, PageSize)) {
			t.Fatalf("bad page: %+v", c)
		}
	}
	if err := h.EnsureLoaded(context.Background(), 0, 3*PageSize); err != nil {
		t.Fatal(err)
	}
	if len(ops.snapshot()) != 3 {
		t.Fatal("Loaded range was installed again")
	}
	for _, r := range [][2]uint64{{4 * PageSize, 1}, {^uint64(0), 2}, {1, ^uint64(0)}} {
		if err := h.EnsureLoaded(context.Background(), r[0], r[1]); err == nil {
			t.Fatalf("accepted invalid range %v", r)
		}
	}
	if err := h.EnsureLoaded(context.Background(), 4*PageSize, 0); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureLoadedConfirmsCopyResult(t *testing.T) {
	for _, tc := range []struct {
		name      string
		completed int64
		err       error
		success   bool
	}{
		{"full", PageSize, nil, true}, {"exists", 0, unix.EEXIST, true},
		{"short", 0, nil, false}, {"unaligned", 1, nil, false}, {"negative", -1, nil, false},
		{"oversized", 2 * PageSize, nil, false}, {"unmapped", 0, unix.ENOENT, false}, {"invalid", 0, unix.EINVAL, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := newFakeIoctls()
			ops.copyHook = func(int, uint64, []byte) (int64, error) { return tc.completed, tc.err }
			h, _ := loadHandler(t, 1, 1, ZeroSource{}, ops.ops())
			err := h.EnsureLoaded(context.Background(), 1, 1)
			if (err == nil) != tc.success || (h.state.Get(0) == StateLoaded) != tc.success {
				t.Fatalf("err=%v state=%v", err, h.state.Get(0))
			}
		})
	}
}

func TestEnsureLoadedEagainRetriesAndCancels(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "cancel"}[cancelRequest], func(t *testing.T) {
			entered := make(chan struct{})
			var attempts atomic.Int32
			ops := newFakeIoctls()
			ops.copyHook = func(int, uint64, []byte) (int64, error) {
				n := attempts.Add(1)
				if n == 1 {
					close(entered)
				}
				if cancelRequest || n == 1 {
					return 0, unix.EAGAIN
				}
				return PageSize, nil
			}
			h, _ := loadHandler(t, 1, 1, ZeroSource{}, ops.ops())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- h.EnsureLoaded(ctx, 0, 1) }()
			receiveSignal(t, entered)
			if cancelRequest {
				cancel()
			}
			err := loadResult(t, done)
			if cancelRequest {
				if !errors.Is(err, context.Canceled) || h.state.Get(0) != StateAbsent {
					t.Fatalf("err=%v state=%v", err, h.state.Get(0))
				}
			} else if err != nil || attempts.Load() != 2 {
				t.Fatalf("err=%v attempts=%d", err, attempts.Load())
			}
		})
	}
}

func TestEnsureLoadedCallerCancellationDoesNotStrandWorker(t *testing.T) {
	source := newRecordingSnapshot(PageSize, sparse.Data, 0x55)
	entered := make(chan struct{})
	var calls atomic.Int32
	source.readHook = func(ctx context.Context, _ uint64, _ []byte) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	h, _ := loadHandler(t, 1, 1, source, newFakeIoctls().ops())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.EnsureLoaded(ctx, 0, 1) }()
	receiveSignal(t, entered)
	cancel()
	if err := loadResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Same page hashes to the same worker. This also tests buffered reply after
	// the original caller has already returned.
	go func() { done <- h.EnsureLoaded(context.Background(), 0, 1) }()
	if err := loadResult(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureLoadedPermanentSourceError(t *testing.T) {
	source := newRecordingSnapshot(PageSize, sparse.Data, 0x55)
	source.readErr = readerr.Mark(io.ErrUnexpectedEOF, false)
	ops := newFakeIoctls()
	h, _ := loadHandler(t, 1, 1, source, ops.ops())
	if err := h.EnsureLoaded(context.Background(), 0, 1); !readerr.IsPermanent(err) {
		t.Fatalf("err=%v", err)
	}
	if len(ops.snapshot()) != 0 {
		t.Fatal("failed source installed bytes")
	}
}

func TestEnsureLoadedReleaseAndCPUWinnerDuringRead(t *testing.T) {
	for _, release := range []bool{false, true} {
		t.Run(map[bool]string{false: "cpu-winner", true: "remove"}[release], func(t *testing.T) {
			source := newRecordingSnapshot(PageSize, sparse.Data, 0x77)
			entered, resume := make(chan struct{}), make(chan struct{})
			source.readHook = func(ctx context.Context, _ uint64, _ []byte) error {
				close(entered)
				select {
				case <-resume:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			ops := newFakeIoctls()
			h, fd := loadHandler(t, 1, 1, source, ops.ops())
			done := make(chan error, 1)
			go func() { done <- h.EnsureLoaded(context.Background(), 0, 1) }()
			receiveSignal(t, entered)
			if release {
				h.handleRemove(unitCHVA, unitCHVA+PageSize, fd)
			} else {
				if _, err := h.urgentCopy(fd, unitCHVA, 0, StateAbsent, bytes.Repeat([]byte{0x42}, PageSize)); err != nil {
					t.Fatal(err)
				}
			}
			close(resume)
			if err := loadResult(t, done); err != nil {
				t.Fatal(err)
			}
			copies := ops.snapshot()
			want := byte(0x42)
			if release {
				want = 0
			}
			if len(copies) != 1 || !bytes.Equal(copies[0].data, bytes.Repeat([]byte{want}, PageSize)) {
				t.Fatalf("stale source installed: %+v", copies)
			}
			if release {
				// Released->Loaded->Released never restores historical source bytes.
				h.handleRemove(unitCHVA, unitCHVA+PageSize, fd)
				if err := h.EnsureLoaded(context.Background(), 0, PageSize); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(ops.snapshot()[1].data, make([]byte, PageSize)) {
					t.Fatal("historical bytes revived")
				}
			}
		})
	}
}

func TestEnsureLoadedRegistrationReadinessSplitAndUnmap(t *testing.T) {
	ops := newFakeIoctls()
	h, fd := loadHandler(t, 3, 1, ZeroSource{}, ops.ops())
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() { canceled <- h.EnsureLoaded(ctx, PageSize, 1) }()
	cancel()
	if err := loadResult(t, canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.EnsureLoaded(context.Background(), PageSize-1, PageSize+2) }()
	other := loadPipe(t)
	if err := h.AddUffd(other[0], unitCHVA+0x100000, 2*PageSize, PageSize); err != nil {
		unix.Close(other[0])
		t.Fatal(err)
	}
	if err := loadResult(t, done); err != nil {
		t.Fatal(err)
	}
	copies := ops.snapshot()
	if len(copies) != 3 || copies[0].fd != fd || copies[1].fd != other[0] || copies[1].dst != unitCHVA+0x100000 || copies[2].dst != unitCHVA+0x100000+PageSize {
		t.Fatalf("wrong owner: %+v", copies)
	}
	msg := uffdMsg{Event: uffdEventUnmap}
	rm := (*uffdMsgRemove)(unsafe.Pointer(&msg.Arg[0]))
	rm.Start = unitCHVA + 0x100000
	rm.End = rm.Start + PageSize
	h.dispatch(&msg, other[0])
	if err := h.EnsureLoaded(context.Background(), PageSize, 1); err == nil {
		t.Fatal("UNMAP accepted")
	}
	if err := h.EnsureLoaded(context.Background(), 2*PageSize, 1); err != nil {
		t.Fatalf("unaffected split registration lost: %v", err)
	}
}

func TestAddUffdFailureRollsBackAndCloseRejectsAdmission(t *testing.T) {
	h, _ := loadHandler(t, 2, 1, ZeroSource{}, newFakeIoctls().ops())
	bad, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(bad)
	if err := h.AddUffd(bad, unitCHVA+PageSize, PageSize, PageSize); err == nil {
		t.Fatal("epoll accepted regular fd")
	}
	if _, ok := h.addrMap.Locate(unitCHVA + PageSize); ok {
		t.Fatal("failed registration leaked map")
	}
	if _, err := unix.FcntlInt(uintptr(bad), unix.F_GETFD, 0); err != nil {
		t.Fatal("failed adoption took fd ownership")
	}
	h.Close()
	if err := h.AddUffd(bad, unitCHVA+PageSize, PageSize, PageSize); err == nil {
		t.Fatal("Add after Close succeeded")
	}
	if err := h.EnsureLoaded(context.Background(), 0, 1); err == nil {
		t.Fatal("load after Close succeeded")
	}
}

func TestCloseDrainsIoctlBeforeFDReuse(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	ops := newFakeIoctls()
	ops.copyHook = func(fd int, _ uint64, _ []byte) (int64, error) {
		close(entered)
		<-release
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			t.Errorf("fd closed during ioctl: %v", err)
		}
		return PageSize, nil
	}
	h, _ := loadHandler(t, 1, 1, ZeroSource{}, ops.ops())
	done := make(chan error, 1)
	go func() { done <- h.EnsureLoaded(context.Background(), 0, 1) }()
	receiveSignal(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()
	receiveSignal(t, h.ctx.Done())
	select {
	case <-closed:
		t.Fatal("Close returned before ioctl drained")
	default:
	}
	close(release)
	if err := loadResult(t, closed); err != nil {
		t.Fatal(err)
	}
	_ = loadResult(t, done)
}

func TestConcurrentBackendLoadsShareWinningPage(t *testing.T) {
	ops := newFakeIoctls()
	h, _ := loadHandler(t, 1, 1, newRecordingSnapshot(PageSize, sparse.Data, 0x41), ops.ops())
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.EnsureLoaded(context.Background(), 0, 1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(ops.snapshot()) != 1 {
		t.Fatalf("copies=%d", len(ops.snapshot()))
	}
}

func TestCloseCancelsRegistrationWaitAndConcurrentAdoption(t *testing.T) {
	h, _ := loadHandler(t, 2, 1, ZeroSource{}, newFakeIoctls().ops())
	done := make(chan error, 1)
	go func() { done <- h.EnsureLoaded(context.Background(), PageSize, 1) }()
	other := loadPipe(t)
	adopted := make(chan error, 1)
	go func() { adopted <- h.AddUffd(other[0], unitCHVA+PageSize, PageSize, PageSize) }()
	h.Close()
	if err := loadResult(t, adopted); err != nil {
		if _, err := unix.FcntlInt(uintptr(other[0]), unix.F_GETFD, 0); err != nil {
			t.Fatal("rejected fd was closed by handler")
		}
		unix.Close(other[0])
	} else {
		if _, err := unix.FcntlInt(uintptr(other[0]), unix.F_GETFD, 0); err != unix.EBADF {
			t.Fatalf("adopted fd was not closed: %v", err)
		}
	}
	// Completion may win the race, but neither readiness nor a reply can hang.
	_ = loadResult(t, done)
}

func TestUrgentReleaseDoesNotPublishStaleChunkTail(t *testing.T) {
	h := newUnitHandler(t, 4*PageSize, newRecordingSnapshot(4*PageSize, sparse.Data, 0x77))
	ops := newFakeIoctls()
	h.ops = ops.ops()
	h.state.Set(0, StateReleased)
	out, err := h.urgentCopy(unitFD, unitCHVA, 0, StateAbsent, bytes.Repeat([]byte{0x77}, PageSize))
	if err != nil || out.tailEligible {
		t.Fatalf("obsolete source plan remained tail eligible: %+v %v", out, err)
	}
	calls := ops.snapshot()
	if len(calls) != 1 || calls[0].kind != "zero" {
		t.Fatalf("stale snapshot copied: %+v", calls)
	}
}

func TestMandatoryConflictReleasesWorkerForCPUFault(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	ops := newFakeIoctls()
	ops.copyHook = func(int, uint64, []byte) (int64, error) { once.Do(func() { close(entered) }); return 0, unix.EAGAIN }
	h, fd := loadHandler(t, 1, 1, ZeroSource{}, ops.ops())
	done := make(chan error, 1)
	go func() { done <- h.EnsureLoaded(context.Background(), 0, 1) }()
	receiveSignal(t, entered)
	// Same hashed worker: a retry loop inside loadPage would monopolize it,
	// preventing this CPU fault from resolving the page and the caller's wait.
	q := h.queue[pageIdxHash(0)%uint64(len(h.queue))]
	q <- faultEvent{address: unitCHVA, uffdFD: fd}
	if err := loadResult(t, done); err != nil {
		t.Fatal(err)
	}
	if h.state.Get(0) != StateLoaded {
		t.Fatal("CPU fault could not progress")
	}
}

func TestReaderPublishesBatchReleaseBeforeBlockedFaultDispatch(t *testing.T) {
	fds := loadPipe(t)
	defer unix.Close(fds[0])
	h := newUnitHandler(t, PageSize, ZeroSource{})
	defer h.cancel()
	h.addrMap.mu.Lock()
	h.addrMap.vmas[0].uffdFD = fds[0]
	h.addrMap.mu.Unlock()
	h.queue = []chan faultEvent{make(chan faultEvent, 1)}
	h.queue[0] <- faultEvent{} // No worker; dispatch must wait outside the gate.
	h.removeQ = make(chan removeReq, 1)
	var raw [64]byte
	raw[0] = uffdEventPagefault
	*(*uint64)(unsafe.Pointer(&raw[16])) = unitCHVA
	raw[32] = uffdEventRemove
	*(*uint64)(unsafe.Pointer(&raw[40])) = unitCHVA
	*(*uint64)(unsafe.Pointer(&raw[48])) = unitCHVA + PageSize
	if _, err := unix.Write(fds[1], raw[:]); err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() { h.drain(fds[0], make([]byte, 32*16), 32); close(drained) }()
	waitRemove(t, h)
	if h.state.Get(0) != StateReleased {
		t.Fatal("read acknowledged REMOVE without publishing Released")
	}
	if !h.installMu.TryLock() {
		t.Fatal("blocking fault dispatch retained installation gate")
	}
	h.installMu.Unlock()
	h.cancel()
	receiveSignal(t, drained)
}

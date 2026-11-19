package uffd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"golang.org/x/sys/unix"
)

func TestPageTraceStrictSetting(t *testing.T) {
	const size = uint64(8 << 20)
	for _, value := range []string{"0", "0000", "4096", "4194304", "04194304", "0x400000", "0X400000", "8384512"} {
		if got := newPageTrace(value, size); got == nil || got.page%PageSize != 0 {
			t.Errorf("rejected aligned page %q", value)
		}
	}
	for _, value := range []string{"", " ", " 4096", "4096\n", "+4096", "-0", "-4096", "0x", "0b1000000000000", "0o10000", "4_096", "0x100_000", "0xg000", "1", "4095", "8388608", "18446744073709551616"} {
		if got := newPageTrace(value, size); got != nil {
			t.Errorf("accepted invalid setting %q", value)
		}
	}
	for _, size := range []uint64{0, PageSize - 1} {
		if got := newPageTrace("0", size); got != nil {
			t.Errorf("accepted incomplete page, size=%d", size)
		}
	}
}

type pageTraceLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *pageTraceLog) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *pageTraceLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func pageTraceRecords(t *testing.T, log *pageTraceLog) []pageTraceRecord {
	t.Helper()
	var result []pageTraceRecord
	for _, line := range log.snapshot() {
		if !strings.HasPrefix(line, pageTracePrefix) || strings.Contains(line, "\n") || len(line) >= 2048 {
			t.Fatalf("invalid trace line: %q", line)
		}
		var record pageTraceRecord
		decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(line, pageTracePrefix)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record.Version != 1 {
			t.Fatalf("trace version=%d", record.Version)
		}
		result = append(result, record)
	}
	return result
}

// These tests use a pipe for epoll/Close ownership and injectable ioctl results.
// They do not claim native UFFD installation or KVM coverage.
func newPageTraceHandler(t *testing.T, pages int, source SnapshotReader, ops uffdOps, logf func(string, ...any), seedPage int) *Handler {
	t.Helper()
	fds := loadPipe(t)
	memfd, err := unix.MemfdCreate("page-trace", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(memfd) })
	if err := unix.Ftruncate(memfd, int64(pages*PageSize)); err != nil {
		t.Fatal(err)
	}
	if seedPage >= 0 {
		if n, err := unix.Pwrite(memfd, bytes.Repeat([]byte{0xff}, PageSize), int64(seedPage*PageSize)); err != nil || n != PageSize {
			t.Fatalf("seed write=%d, %v", n, err)
		}
	}
	m := NewAddressMap(uint64(pages * PageSize))
	if err := m.RegisterVMA(ProcessCH, unitCHVA, uint64(pages*PageSize), 0); err != nil {
		t.Fatal(err)
	}
	h, err := NewWithBackendUffd(fds[0], m, Config{MemfdFD: memfd, Size: pages * PageSize, Source: source, NumWorkers: 2, Logf: logf})
	if err != nil {
		unix.Close(fds[0])
		t.Fatal(err)
	}
	h.ops = ops
	h.Start()
	t.Cleanup(func() { h.Close() })
	return h
}

func TestPageTraceDisabledDoesNotRecordOrChangeLoad(t *testing.T) {
	for _, setting := range []string{"", "invalid", "1", "4096"} {
		t.Run(fmt.Sprintf("setting_%q", setting), func(t *testing.T) {
			t.Setenv(pageTraceEnv, setting)
			var log pageTraceLog
			ops := newFakeIoctls()
			h := newPageTraceHandler(t, 1, newRecordingSnapshot(PageSize, sparse.Data, 0xff), ops.ops(), log.logf, -1)
			if h.pageTrace != nil {
				t.Fatal("disabled/invalid setting allocated a trace")
			}
			if err := h.EnsureLoaded(context.Background(), 0, PageSize); err != nil {
				t.Fatal(err)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if calls := ops.snapshot(); len(calls) != 1 || !bytes.Equal(calls[0].data, bytes.Repeat([]byte{0xff}, PageSize)) {
				t.Fatal("disabled trace changed the source load")
			}
			if len(log.snapshot()) != 0 {
				t.Fatal("disabled trace emitted output")
			}
		})
	}
}

func TestPageTraceCopyReleaseAndZero(t *testing.T) {
	const target = uint64(4 << 20)
	h := newUnitHandler(t, target+PageSize, newRecordingSnapshot(target+PageSize, sparse.Data, 0xff))
	h.pageTrace = newPageTrace("0x400000", target+PageSize)
	h.epfd = -1
	h.removeQ = make(chan removeReq, 1)
	h.ops = newFakeIoctls().ops()
	var log pageTraceLog
	h.logf = log.logf
	ev := faultEvent{address: unitCHVA + target, uffdFD: unitFD}
	h.handleFault(ev, make([]byte, PageSize))
	if err := h.loadPage(context.Background(), target, make([]byte, PageSize)); err != nil {
		t.Fatal(err)
	}
	h.handleRemove(ev.address, ev.address+PageSize, unitFD)
	h.handleFault(ev, make([]byte, PageSize))
	if len(log.snapshot()) != 0 {
		t.Fatal("trace emitted before Close")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	records := pageTraceRecords(t, &log)
	wantEvents := []string{"fault", "source", "copy", "load-hit", "remove", "fault", "zero", "summary"}
	if len(records) != len(wantEvents) {
		t.Fatalf("events=%+v", records)
	}
	for i, record := range records {
		if record.Event != wantEvents[i] || record.Page != target || record.Sequence != uint64(i+1) {
			t.Fatalf("record %d=%+v", i, record)
		}
	}
	ffHash := sha256.Sum256(bytes.Repeat([]byte{0xff}, PageSize))
	zeroHash := sha256.Sum256(make([]byte, PageSize))
	copy, removed, zero := records[2], records[4], records[6]
	if records[1].Kind != "data" || copy.State != "absent" || copy.SHA256 != hex.EncodeToString(ffHash[:]) || *copy.NonFF != 0 || *copy.Done != PageSize || *copy.Errno != 0 {
		t.Fatalf("FF COPY evidence=%+v", copy)
	}
	if removed.State != "loaded" || zero.State != "released" || zero.SHA256 != hex.EncodeToString(zeroHash[:]) || *zero.NonFF != PageSize || *zero.Done != PageSize || *zero.Errno != 0 {
		t.Fatalf("release/zero evidence=%+v/%+v", removed, zero)
	}
	if h.state.Get(target/PageSize) != StateLoaded || *records[7].Recorded != 7 || *records[7].Dropped != 0 {
		t.Fatal("trace changed the state transition or summary")
	}
}

func TestPageTraceEexistIsInputEvidenceAndFallbackIsVisible(t *testing.T) {
	h := newUnitHandler(t, PageSize, newRecordingSnapshot(PageSize, sparse.Data, 0xff))
	h.pageTrace = newPageTrace("0", PageSize)
	defer h.cancel()
	ops := newFakeIoctls()
	ops.copyHook = func(int, uint64, []byte) (int64, error) { return 0, unix.EEXIST }
	ops.zeroHook = func(int, uint64, uint64) (int64, error) { return 0, unix.EINVAL }
	h.ops = ops.ops()
	ev := faultEvent{address: unitCHVA, uffdFD: unitFD}
	h.handleFault(ev, make([]byte, PageSize))
	h.handleFault(ev, make([]byte, PageSize))
	var log pageTraceLog
	h.pageTrace.emit(log.logf)
	records := pageTraceRecords(t, &log)
	if len(records) != 7 || records[2].Event != "copy" || *records[2].Errno != int(unix.EEXIST) || *records[2].Done != 0 || *records[2].NonFF != 0 {
		t.Fatalf("EEXIST evidence=%+v", records)
	}
	if records[4].Event != "zero" || *records[4].Errno != int(unix.EINVAL) || records[5].Event != "copy" || *records[5].Errno != int(unix.EEXIST) || *records[5].NonFF != PageSize {
		t.Fatalf("zero fallback evidence=%+v", records)
	}
	if records[4].State != "loaded" || records[5].State != "loaded" || h.state.Get(0) != StateLoaded {
		t.Fatal("EEXIST changed the established Loaded state")
	}
}

func TestPageTraceNeighborSourceRunsDoNotConsumeTargetBudget(t *testing.T) {
	const target = uint64(4 << 20)
	h := newUnitHandler(t, target+PageSize, newRecordingSnapshot(target+PageSize, sparse.Data, 0xff))
	defer h.cancel()
	h.pageTrace = newPageTrace("0x400000", target+PageSize)
	h.ops = newFakeIoctls().ops()
	// Neighbor RunAt results cover the target, but the busy tail means those
	// faults select only their own urgent page for installation.
	h.tailBusy.Store(true)
	for off := target - ordinaryDataFaultFillBytes + PageSize; off < target; off += PageSize {
		h.handleFault(faultEvent{address: unitCHVA + off, uffdFD: unitFD}, make([]byte, PageSize))
	}
	if len(h.pageTrace.records) != 0 || h.pageTrace.dropped != 0 {
		t.Fatal("neighbor metadata probes consumed the target-page trace budget")
	}
	h.handleFault(faultEvent{address: unitCHVA + target, uffdFD: unitFD}, make([]byte, PageSize))
	h.tailBusy.Store(false)
	var log pageTraceLog
	h.pageTrace.emit(log.logf)
	records := pageTraceRecords(t, &log)
	if len(records) != 4 || records[0].Event != "fault" || records[1].Event != "source" || records[2].Event != "copy" || *records[3].Dropped != 0 {
		t.Fatalf("target installation evidence=%+v", records)
	}
}

func TestPageTraceBoundedConcurrentRecords(t *testing.T) {
	trace := newPageTrace("4096", 2*PageSize)
	trace.record("fault", 0, PageSize, StateAbsent, nil)
	var wg sync.WaitGroup
	var fills atomic.Int32
	for range 8 {
		wg.Go(func() {
			for range 40 {
				trace.record("fault", PageSize, PageSize, StateLoaded, func(*pageTraceRecord) { fills.Add(1) })
			}
		})
	}
	wg.Wait()
	var log pageTraceLog
	trace.emit(log.logf)
	records := pageTraceRecords(t, &log)
	if len(records) != pageTraceLimit+1 || fills.Load() != pageTraceLimit {
		t.Fatalf("record/fill counts=%d/%d", len(records), fills.Load())
	}
	for i, record := range records[:pageTraceLimit] {
		if record.Sequence != uint64(i+1) {
			t.Fatalf("out-of-order record=%+v", record)
		}
	}
	summary := records[pageTraceLimit]
	if summary.Event != "summary" || summary.Sequence != 321 || *summary.Recorded != pageTraceLimit || *summary.Dropped != 320-pageTraceLimit {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestPageTracePreservesSignedCompletionAndError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		done  int64
		err   error
		errno int
	}{
		{"kernel", -int64(unix.EFAULT), unix.EFAULT, int(unix.EFAULT)},
		{"other", 0, errors.New("injected non-errno failure"), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUnitHandler(t, PageSize, ZeroSource{})
			defer h.cancel()
			h.pageTrace = newPageTrace("0", PageSize)
			h.ops.copy = func(int, uint64, []byte) (int64, error) { return tc.done, tc.err }
			h.installMu.Lock()
			done, err := h.copyCleanLocked(unitFD, unitCHVA, make([]byte, PageSize))
			h.installMu.Unlock()
			if done != tc.done || err != tc.err {
				t.Fatalf("trace changed ioctl result: %d, %v", done, err)
			}
			var log pageTraceLog
			h.pageTrace.emit(log.logf)
			records := pageTraceRecords(t, &log)
			if len(records) != 2 || *records[0].Done != tc.done || *records[0].Errno != tc.errno {
				t.Fatalf("signed completion/error lost: %+v", records)
			}
		})
	}
}

func TestPageTraceSeedExistingFolio(t *testing.T) {
	t.Setenv(pageTraceEnv, "4096")
	var log pageTraceLog
	h := newPageTraceHandler(t, 2, ZeroSource{}, newFakeIoctls().ops(), log.logf, 1)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	records := pageTraceRecords(t, &log)
	if len(records) != 2 || records[0].Event != "seed" || records[0].State != "absent" || records[0].SHA256 != "" {
		t.Fatalf("seed must report existence without reading contents: %+v", records)
	}
}

func TestPageTraceCloseEmitsAfterWorkersAndUsers(t *testing.T) {
	t.Setenv(pageTraceEnv, "0")
	entered, release := make(chan struct{}), make(chan struct{})
	var workerFinished, userFinished atomic.Bool
	ops := newFakeIoctls()
	ops.copyHook = func(int, uint64, []byte) (int64, error) {
		close(entered)
		<-release
		workerFinished.Store(true)
		return PageSize, nil
	}
	var log pageTraceLog
	var h *Handler
	h = newPageTraceHandler(t, 1, newRecordingSnapshot(PageSize, sparse.Data, 0xff), ops.ops(), func(format string, args ...any) {
		if !workerFinished.Load() || !userFinished.Load() {
			t.Error("trace emitted before workers/users joined")
		}
		if !h.installMu.TryLock() {
			t.Error("trace logger called under installMu")
		} else {
			h.installMu.Unlock()
		}
		log.logf(format, args...)
	}, -1)
	// Hold a separately admitted user until the worker has actually returned.
	h.uffdsMu.Lock()
	h.users.Add(1)
	h.uffdsMu.Unlock()
	loadDone := make(chan error, 1)
	go func() { loadDone <- h.EnsureLoaded(context.Background(), 0, PageSize) }()
	receiveSignal(t, entered)
	closeDone := make(chan error, 1)
	go func() { closeDone <- h.Close() }()
	<-h.ctx.Done()
	if len(log.snapshot()) != 0 {
		t.Error("trace emitted during blocked worker")
	}
	close(release)
	_ = loadResult(t, loadDone) // cancellation is expected when Close wins.
	workersDone := make(chan struct{})
	go func() { h.wg.Wait(); close(workersDone) }()
	receiveSignal(t, workersDone)
	if len(log.snapshot()) != 0 {
		t.Error("trace emitted during blocked user")
	}
	h.pageTrace.record("load-hit", 0, PageSize, StateLoaded, nil)
	userFinished.Store(true)
	h.users.Done()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not finish after worker and user release")
	}
	records := pageTraceRecords(t, &log)
	if len(records) != 4 || records[2].Event != "load-hit" || records[3].Event != "summary" || *records[3].Recorded != 3 {
		t.Fatalf("joined final record missing: %+v", records)
	}
}

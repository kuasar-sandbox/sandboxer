package resctl

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

// Block outside the embedded adapter's mutex: snapshot diagnostics must still
// be able to read reservation state while the node-only commit is waiting.
type snapshotBlockingReservation struct {
	*fakeReservationAdapter
	entered chan struct{}
	resume  <-chan struct{}
	once    sync.Once
}

func (f *snapshotBlockingReservation) RequestBudget(current, delta uint64, urgency, reason string) (uint64, uint64, time.Duration, error) {
	f.once.Do(func() { close(f.entered) })
	<-f.resume
	return f.fakeReservationAdapter.RequestBudget(current, delta, urgency, reason)
}

func TestMemoryProgressNodeCommitDoesNotBlockBeginSnapshot(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 320<<20, 705<<20
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	entered, resume := make(chan struct{}), make(chan struct{})
	m.reservation = &snapshotBlockingReservation{
		fakeReservationAdapter: reservation, entered: entered, resume: resume,
	}
	report := proto.MemReport{Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 449 << 20}
	if !m.SubmitGuestReport(report) {
		t.Fatal("fresh report rejected")
	}
	report = <-m.reportEvents
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	processed := make(chan error, 1)
	joined := false
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(resume) }) }
	defer func() {
		unblock()
		cancel()
		if !joined {
			select {
			case <-processed:
			case <-time.After(5 * time.Second):
				t.Error("report worker did not exit after cleanup")
			}
		}
	}()
	go func() {
		m.controlMu.Lock()
		defer m.controlMu.Unlock()
		processed <- m.processReportLocked(ctx, report)
	}()
	select {
	case <-entered:
	case err := <-processed:
		joined = true
		t.Fatalf("report ended before node commit: %v", err)
	case <-ctx.Done():
		t.Fatal("shrink did not reach blocking node commit")
	}

	snapshotCtx, snapshotCancel := context.WithTimeout(context.Background(), time.Second)
	defer snapshotCancel()
	release, err := m.BeginSnapshot(snapshotCtx)
	if err != nil {
		t.Fatalf("BeginSnapshot waited for node-only commit: %v", err)
	}
	defer release()
	if got := reservation.ReservationMemory(); got != 768<<20 {
		t.Fatalf("blocked commit changed reservation: %d", got)
	}
	unblock()
	// Keep the snapshot gate held while the node reply finishes. No further
	// CH/high mutation is needed to complete this already-validated round.
	select {
	case err := <-processed:
		joined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("node reply could not finish while the snapshot gate was held")
	}
	if got := reservation.ReservationMemory(); got != 705<<20 {
		t.Fatalf("reservation=%d, want partial Budget=%d", got, 705<<20)
	}
	if m.txn != nil || len(fakeCH.calls()) != 0 {
		t.Fatal("partial settlement retained work or issued an unnecessary resize")
	}
}

func TestMemoryProgressStaticCommitSynchronizesSnapshotCapture(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 320<<20, 705<<20
	})
	initial := &fakeReservationAdapter{current: 768 << 20}
	m := newProgressController(t, fakeCH, cgroup, initial)
	// A nil adapter selects the production static-reservation path. The initial
	// baseline was copied into staticReservation by NewMemoryController.
	m.reservation = nil

	report := proto.MemReport{Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 449 << 20}
	if !m.SubmitGuestReport(report) {
		t.Fatal("fresh report rejected")
	}
	report = <-m.reportEvents
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	processed := make(chan error, 1)
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-processed:
			case <-time.After(5 * time.Second):
				t.Error("static report worker did not exit after cleanup")
			}
		}
	}()
	go func() {
		m.controlMu.Lock()
		defer m.controlMu.Unlock()
		processed <- m.processReportLocked(ctx, report)
	}()

	for {
		release, err := m.BeginSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		state, captureErr := m.CaptureState(ctx)
		release()
		if captureErr != nil {
			t.Fatal(captureErr)
		}
		if state.Reservation != 768<<20 && state.Reservation != 705<<20 {
			t.Fatalf("snapshot captured invalid reservation=%d", state.Reservation)
		}
		select {
		case err := <-processed:
			joined = true
			if err != nil {
				t.Fatal(err)
			}
			finalRelease, err := m.BeginSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			final, captureErr := m.CaptureState(ctx)
			finalRelease()
			if captureErr != nil {
				t.Fatal(captureErr)
			}
			if final.Reservation != 705<<20 {
				t.Fatalf("snapshot reservation=%d after static settlement, want %d", final.Reservation, 705<<20)
			}
			return
		default:
		}
	}
}

func TestMemorySettlementReboundRepairsHighBeforeFailedRetry(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 320<<20, 705<<20
		f.onInfo = func(f *fakeCHMemory) {
			if f.infoCalls == 3 {
				f.currentBudget = 710 << 20
			}
		}
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 449<<20); err == nil {
		t.Fatal("rebounded actual did not invalidate settlement")
	}
	want, err := CalculateMemoryHigh(capacity, 64<<20, 710<<20, 261<<20,
		resource.DefaultWatermarkHighRatio, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := readProgressHigh(t, cgroup); got != want.HostMemoryHigh {
		t.Fatalf("memory.high=%d after rebound, want repaired value %d", got, want.HostMemoryHigh)
	}
	fakeCH.configure(func(f *fakeCHMemory) { f.infoFailures = 2 })
	for range 2 {
		m.controlMu.Lock()
		err = m.retryLocked(context.Background())
		m.controlMu.Unlock()
		if err == nil {
			t.Fatal("injected CH query failure was hidden")
		}
	}
	if got := reservation.ReservationMemory(); got != 768<<20 || len(reservation.seen()) != 0 {
		t.Fatalf("failed retries returned reservation: current=%d calls=%+v", got, reservation.seen())
	}
}

func TestMemorySettlementReboundAboveReservationUsesAuthorizedHigh(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 320<<20, 705<<20
		f.onInfo = func(f *fakeCHMemory) {
			if f.infoCalls == 3 {
				f.currentBudget = 800 << 20
			}
		}
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 449<<20); err == nil {
		t.Fatal("rebound beyond reservation did not invalidate settlement")
	}
	want, err := CalculateMemoryHigh(capacity, 64<<20, 768<<20, 351<<20,
		resource.DefaultWatermarkHighRatio, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := readProgressHigh(t, cgroup); got != want.HostMemoryHigh {
		t.Fatalf("memory.high=%d, want authorized-baseline value %d", got, want.HostMemoryHigh)
	}
	if reservation.ReservationMemory() != 768<<20 || len(reservation.seen()) != 0 {
		t.Fatal("unreserved actual movement was treated as a grant")
	}
}

func TestMemorySettlementHighRepairFailureRetainsReservation(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 320<<20, 705<<20
		f.onInfo = func(f *fakeCHMemory) {
			if f.infoCalls == 3 {
				f.currentBudget = 710 << 20
				if err := os.Remove(filepath.Join(cgroup, "memory.high")); err != nil {
					t.Errorf("remove memory.high: %v", err)
				}
			}
		}
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	err := processProgressReport(t, m, 1, 449<<20)
	if err == nil || !strings.Contains(err.Error(), "observation changed") || !strings.Contains(err.Error(), "repair memory.high") {
		t.Fatalf("repair failure did not preserve both errors: %v", err)
	}
	if reservation.ReservationMemory() != 768<<20 || len(reservation.seen()) != 0 {
		t.Fatal("failed high repair returned reservation")
	}
}

func TestMemorySettlementReboundRetainsRatioReserveDemand(t *testing.T) {
	const capacity = uint64(8 << 30)
	const initialBudget = uint64(1537 << 20)
	const reboundBudget = uint64(1545 << 20)
	const initialDemand = uint64(512 << 20)
	const repairedDemand = initialDemand + reboundBudget - initialBudget
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 6656<<20, initialBudget
		f.onInfo = func(f *fakeCHMemory) {
			if f.infoCalls == 3 {
				f.currentBudget = reboundBudget
			}
		}
	})
	reservation := &fakeReservationAdapter{current: 1600 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, initialBudget-initialDemand); err == nil {
		t.Fatal("rebound did not defer the smaller settlement")
	}
	want, err := CalculateMemoryHigh(capacity, 64<<20, reboundBudget, repairedDemand,
		resource.DefaultWatermarkHighRatio, 1)
	if err != nil || want.PressureReserve <= resource.MemoryStep {
		t.Fatalf("test did not exercise ratio reserve: %+v, %v", want, err)
	}
	if got := readProgressHigh(t, cgroup); got != want.HostMemoryHigh {
		t.Fatalf("repair high=%d, want conservative-demand high=%d", got, want.HostMemoryHigh)
	}
	if m.txn == nil || m.txn.demand != repairedDemand {
		t.Fatalf("repair demand was not retained: %+v", m.txn)
	}

	fakeCH.configure(func(f *fakeCHMemory) { f.infoFailures = 2 })
	for range 2 {
		m.controlMu.Lock()
		err = m.retryLocked(context.Background())
		m.controlMu.Unlock()
		if err == nil {
			t.Fatal("injected CH failure was hidden")
		}
		if got := readProgressHigh(t, cgroup); got != want.HostMemoryHigh {
			t.Fatalf("failed retry changed repaired high=%d", got)
		}
	}
	if reservation.ReservationMemory() != 1600<<20 || len(reservation.seen()) != 0 {
		t.Fatal("failed CH retries released reservation")
	}

	// A later confirmed retry must not undo the raise by using the old demand.
	// Retain the transaction through a failed node call, then retry once more.
	reservation.failures = 1
	for attempt := range 2 {
		m.controlMu.Lock()
		err = m.retryLocked(context.Background())
		m.controlMu.Unlock()
		if (attempt == 0) != (err != nil) {
			t.Fatalf("retry %d error=%v", attempt, err)
		}
		if got := readProgressHigh(t, cgroup); got != want.HostMemoryHigh {
			t.Fatalf("confirmed retry undid repaired high=%d", got)
		}
	}
	if reservation.ReservationMemory() != reboundBudget || m.txn != nil {
		t.Fatal("confirmed retry did not settle the covered rebound Budget")
	}
}

func TestMemoryTransactionRepairDemandCountsNewActualOnly(t *testing.T) {
	txn := &memoryTransaction{demand: 512 << 20}
	for _, sample := range []uint64{1537 << 20, 1537 << 20, 1545 << 20} {
		if got := txn.retainRepairDemand(sample, 1545<<20, 1600<<20); got != 520<<20 {
			t.Fatalf("repeated rebound double-counted demand=%d", got)
		}
	}
	if got := txn.retainRepairDemand(1545<<20, 1553<<20, 1600<<20); got != 528<<20 {
		t.Fatalf("new rebound demand=%d, want %d", got, 528<<20)
	}
	if got := txn.retainRepairDemand(1553<<20, math.MaxUint64, 1600<<20); got != 1600<<20 {
		t.Fatalf("overflow/excess actual escaped authorized Budget: %d", got)
	}
}

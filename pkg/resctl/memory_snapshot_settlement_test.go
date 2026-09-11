package resctl

import (
	"context"
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
	want, err := CalculateMemoryHigh(capacity, 64<<20, 710<<20, 256<<20,
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
	want, err := CalculateMemoryHigh(capacity, 64<<20, 768<<20, 256<<20,
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

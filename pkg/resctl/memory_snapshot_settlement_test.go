package resctl

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
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

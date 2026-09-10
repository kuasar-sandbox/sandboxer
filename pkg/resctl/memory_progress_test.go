package resctl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func newProgressController(t *testing.T, fakeCH *fakeCHMemory, cgroup string, reservation *fakeReservationAdapter) *MemoryController {
	t.Helper()
	cfg := &config.SandboxConfig{Resources: config.ResourcesConfig{
		Capacity:    config.CapacityConfig{CPU: 1, Memory: fmt.Sprintf("%dB", fakeCH.capacity)},
		Allocatable: config.AllocatableConfig{CPU: 1, Memory: "256MiB"},
		Overhead:    &config.OverheadConfig{Memory: "64MiB"},
		Control:     config.ControlConfig{CgroupPath: cgroup},
	}}
	cfg.ApplyDefaults()
	balloon := NewBalloonController(fakeCH.sock, fakeCH.capacity, time.Second, nil)
	if err := balloon.SeedColdTarget(fakeCH.acceptedTarget); err != nil {
		t.Fatal(err)
	}
	m, err := NewMemoryController(MemoryControllerOptions{
		Config: cfg, CgroupPath: cgroup, Balloon: balloon,
		InitialBudget: reservation.ReservationMemory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	m.reservation = reservation
	m.openReportBarrier()
	return m
}

func processProgressReport(t *testing.T, m *MemoryController, seq, available uint64) error {
	t.Helper()
	if !m.SubmitGuestReport(proto.MemReport{
		Epoch: 1, Seq: seq, MemTotalBytes: m.capacity, MemAvailableBytes: available,
	}) {
		t.Fatal("report rejected by lifecycle barrier")
	}
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	select {
	case report := <-m.reportEvents:
		return m.processReportLocked(context.Background(), report)
	default:
		t.Fatal("fresh report was not queued")
		return nil
	}
}

func readProgressHigh(t *testing.T, cgroup string) uint64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	high, err := strconv.ParseUint(string(data), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return high
}

func TestMemoryProgressSettlesCappedStepAndLastPage(t *testing.T) {
	const capacity = uint64(8 << 30)
	const target = uint64(6656 << 20)
	for _, residual := range []uint64{1 << 20, balloonPageSize} {
		t.Run(strconv.FormatUint(residual, 10), func(t *testing.T) {
			cgroup := newMemoryCgroup(t, "max", "1")
			fakeCH := newFakeCHMemory(t, capacity)
			fakeCH.configure(func(f *fakeCHMemory) {
				f.acceptedTarget = 6592 << 20
				f.currentBudget = 1600 << 20
				f.onResize = func(requested uint64) {
					fakeCH.configure(func(f *fakeCHMemory) {
						f.currentBudget = capacity - min(requested, target-residual)
					})
				}
			})
			reservation := &fakeReservationAdapter{current: 1600 << 20}
			m := newProgressController(t, fakeCH, cgroup, reservation)
			if err := processProgressReport(t, m, 1, 1088<<20); err != nil {
				t.Fatal(err)
			}
			budget := capacity - target + residual
			if got := reservation.ReservationMemory(); got != budget {
				t.Fatalf("reservation=%d, want observed Budget=%d", got, budget)
			}
			if m.txn != nil || m.balloon.State().TargetReached(capacity) {
				t.Fatal("partial settlement required/fabricated target attainment")
			}
			wantHigh, err := CalculateMemoryHigh(capacity, 64<<20, budget, 512<<20,
				resource.DefaultWatermarkHighRatio, 1)
			if err != nil || readProgressHigh(t, cgroup) != wantHigh.HostMemoryHigh {
				t.Fatalf("partial high did not use observed Budget: %+v, %v", wantHigh, err)
			}

			// A fixed cap is a normal no-new-action state. Repeated valid reports
			// must neither reissue resize nor rewrite identical high/reservation.
			highPath := filepath.Join(cgroup, "memory.high")
			stamp := time.Unix(123456789, 0)
			if err := os.Chtimes(highPath, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			for seq := uint64(2); seq <= 5; seq++ {
				if err := processProgressReport(t, m, seq, budget-512<<20); err != nil {
					t.Fatal(err)
				}
			}
			info, err := os.Stat(highPath)
			if err != nil || !info.ModTime().Equal(stamp) {
				t.Fatalf("plateau rewrote high: %v", err)
			}
			if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{target}) {
				t.Fatalf("plateau accumulated/reverted targets: %v", calls)
			}
			if calls := reservation.seen(); len(calls) != 1 || calls[0].current != budget || calls[0].delta != 0 {
				t.Fatalf("plateau repeated or rounded the partial settlement: %+v", calls)
			}

			// Later observed progress, including a single final page, settles on
			// the next report without restarting the controller or retargeting.
			fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget = capacity - target })
			if err := processProgressReport(t, m, 6, 256<<20); err != nil {
				t.Fatal(err)
			}
			if got := reservation.ReservationMemory(); got != capacity-target {
				t.Fatalf("final residual was left reserved: %d", got)
			}
			if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{target}) {
				t.Fatalf("settling residual manufactured a resize: %v", calls)
			}
		})
	}
}

func TestMemoryProgressGrowReservesReturned63MiB(t *testing.T) {
	const capacity = uint64(8 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 6656 << 20
		f.currentBudget = 1537 << 20
	})
	reservation := &fakeReservationAdapter{current: 1600 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 1025<<20); err != nil {
		t.Fatal(err)
	}
	fakeCH.configure(func(f *fakeCHMemory) {
		f.onResize = func(uint64) {
			if got := reservation.ReservationMemory(); got != 1600<<20 {
				t.Errorf("grow resized without returned 63MiB grant: reservation=%d", got)
			}
		}
	})
	m.controlMu.Lock()
	err := m.processPressureLocked(context.Background(), pressureGrow{urgency: resource.UrgencyHigh, reason: "oom"})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := reservation.seen(); !reflect.DeepEqual(calls, []reservationCall{
		{current: 1537 << 20, reason: "shrink_commit"},
		{current: 1537 << 20, delta: 63 << 20, reason: "oom"},
	}) {
		t.Fatalf("partial settlement/grow calls = %+v", calls)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{6592 << 20}) {
		t.Fatalf("grow target calls = %v", calls)
	}
}

func TestMemoryProgressPendingGrowRetainsAccumulatedReservation(t *testing.T) {
	cgroup := newMemoryCgroup(t, "1", "1")
	fakeCH := newFakeCHMemory(t, 1<<30)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 512<<20, 512<<20
	})
	reservation := &fakeReservationAdapter{current: 512 << 20, grantMax: 50 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := processProgressReport(t, m, 2, 512<<20); err != nil {
		t.Fatal(err)
	}
	if got := reservation.ReservationMemory(); got != 612<<20 {
		t.Fatalf("smaller report released pending grow reservation: %d", got)
	}
	for _, call := range reservation.seen() {
		if call.delta == 0 {
			t.Fatalf("pending grow entered partial settlement: %+v", call)
		}
	}
	if m.txn == nil || m.txn.kind != memoryTransactionGrow || m.txn.growObjective() != 768<<20 {
		t.Fatalf("pending objective was lost: %+v", m.txn)
	}
}

func TestMemoryProgressHighFailureDefersPartialSettlement(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, 1<<30)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 320<<20, 705<<20
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	highPath := filepath.Join(cgroup, "memory.high")
	if err := os.Remove(highPath); err != nil {
		t.Fatal(err)
	}
	if err := processProgressReport(t, m, 1, 449<<20); err == nil {
		t.Fatal("missing high did not fail settlement")
	}
	if len(reservation.seen()) != 0 || reservation.ReservationMemory() != 768<<20 {
		t.Fatal("high failure released partial reservation")
	}
	if err := os.WriteFile(highPath, []byte("max"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.controlMu.Lock()
	err := m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil || reservation.ReservationMemory() != 705<<20 || m.txn != nil {
		t.Fatalf("forward partial retry failed: %v, reservation=%d", err, reservation.ReservationMemory())
	}
}

func TestMemoryProgressRetryReappliesHighForChangedBudget(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 320<<20, 705<<20
	})
	reservation := &fakeReservationAdapter{current: 768 << 20, failures: 1}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 449<<20); err == nil {
		t.Fatal("injected node failure was hidden")
	}
	const laterBudget = uint64(704<<20 + 4096)
	fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget = laterBudget })
	m.controlMu.Lock()
	err := m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil || reservation.ReservationMemory() != laterBudget {
		t.Fatalf("later partial Budget did not settle: %v", err)
	}
	want, err := CalculateMemoryHigh(capacity, 64<<20, laterBudget, 256<<20,
		resource.DefaultWatermarkHighRatio, 1)
	if err != nil || readProgressHigh(t, cgroup) != want.HostMemoryHigh {
		t.Fatalf("highApplied reused an older Budget: %+v, %v", want, err)
	}
}

func TestMemoryProgressLostSettlementReplyUsesReducedBaseline(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, 1<<30)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 320<<20, 705<<20
	})
	reservation := &fakeReservationAdapter{current: 768 << 20, failures: 1}
	// Model the existing ControllerHooks contract after a lost shrink reply:
	// the smaller submitted baseline is the only locally reusable reservation.
	reservation.onCall = func(call reservationCall) {
		if call.delta == 0 {
			reservation.current = call.current // RequestBudget holds its mutex.
		}
	}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 449<<20); err == nil {
		t.Fatal("lost partial reply was hidden")
	}
	if err := processProgressReport(t, m, 2, 0); err != nil {
		t.Fatal(err)
	}
	calls := reservation.seen()
	if len(calls) != 2 || calls[1].current != 705<<20 || calls[1].delta != 319<<20 {
		t.Fatalf("grow reused a possibly returned reservation after reply loss: %+v", calls)
	}
}

func TestMemoryProgressRechecksBeforeReturningReservation(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, 1<<30)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 320<<20, 705<<20
		f.onInfo = func(f *fakeCHMemory) {
			if f.infoCalls == 3 { // after high, immediately before node commit
				f.currentBudget = 710 << 20
			}
		}
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 449<<20); err == nil {
		t.Fatal("rebounded actual did not invalidate the smaller commit")
	}
	if len(reservation.seen()) != 0 || reservation.ReservationMemory() != 768<<20 {
		t.Fatal("rebound released an obsolete smaller baseline")
	}
	if err := processProgressReport(t, m, 2, 454<<20); err != nil {
		t.Fatal(err)
	}
	if got := reservation.ReservationMemory(); got != 710<<20 {
		t.Fatalf("fresh observation did not settle the revised Budget: %d", got)
	}
}

func TestMemoryProgressDiscardsUnexecutedReversedCandidate(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, 1<<30)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 100<<20, 930<<20
		f.onInfo = func(f *fakeCHMemory) {
			if f.infoCalls == 2 { // immediately before the proposed 128MiB target
				f.currentBudget = 931 << 20
			}
		}
	})
	reservation := &fakeReservationAdapter{current: 940 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 674<<20); err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); len(calls) != 0 || m.txn != nil {
		t.Fatalf("reversed decision was executed/retained: calls=%v txn=%+v", calls, m.txn)
	}
	m.controlMu.Lock()
	err := m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil || len(fakeCH.calls()) != 0 {
		t.Fatalf("ticker revived discarded candidate: %v", err)
	}
	if err := processProgressReport(t, m, 2, 675<<20); err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{128 << 20}) {
		t.Fatalf("fresh bounded non-equal observation did not progress: %v", calls)
	}
	if got := reservation.ReservationMemory(); got != 931<<20 {
		t.Fatalf("partial settlement after revised decision = %d", got)
	}
}

func TestMemoryProgressConfirmsAmbiguousShrinkWithoutRepeatingResize(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, 1<<30)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 256<<20, 768<<20
		f.dropResizeACKs = 1
		f.onInfo = func(f *fakeCHMemory) {
			if f.infoCalls == 3 { // post-resize confirmation fails once
				f.infoFailures = 1
			}
		}
		f.onResize = func(uint64) {
			fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget = 705 << 20 })
		}
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 512<<20); err == nil {
		t.Fatal("ambiguous resize was reported as confirmed")
	}
	if len(reservation.seen()) != 0 {
		t.Fatal("ambiguous target allowed settlement")
	}
	m.controlMu.Lock()
	err := m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil || reservation.ReservationMemory() != 705<<20 || m.txn != nil {
		t.Fatalf("confirmation did not settle partial Budget: %v", err)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20}) {
		t.Fatalf("confirmation repeated/rolled back resize: %v", calls)
	}
}

func TestMemoryProgressPendingCandidateCannotInflateUnreservedActual(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, 1<<30)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 100<<20, 930<<20
		f.onInfo = func(f *fakeCHMemory) {
			if f.infoCalls == 2 {
				f.infoFailures = 1 // retain the not-yet-issued 128MiB candidate
			}
		}
	})
	reservation := &fakeReservationAdapter{current: 940 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 674<<20); err == nil {
		t.Fatal("missing immediate observation did not defer the candidate")
	}
	fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget = 941 << 20 })
	if err := processProgressReport(t, m, 2, 685<<20); err != nil {
		t.Fatal(err)
	}
	if m.txn != nil || len(fakeCH.calls()) != 0 || len(reservation.seen()) != 0 {
		t.Fatalf("uncovered actual refreshed/executed the candidate: txn=%+v resize=%v reservations=%+v",
			m.txn, fakeCH.calls(), reservation.seen())
	}
	raw, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil || string(raw) != "max" {
		t.Fatalf("uncovered actual changed high: %q, %v", raw, err)
	}
}

func TestMemoryProgressChangedAcceptedTargetInvalidatesCandidate(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, 1<<30)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 100<<20, 930<<20
		f.onInfo = func(f *fakeCHMemory) {
			if f.infoCalls == 2 {
				f.acceptedTarget = 104 << 20
			}
		}
	})
	reservation := &fakeReservationAdapter{current: 940 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	if err := processProgressReport(t, m, 1, 674<<20); err != nil {
		t.Fatal(err)
	}
	if m.txn != nil || len(fakeCH.calls()) != 0 {
		t.Fatal("an unexpected accepted target did not invalidate the old decision")
	}
	if m.balloon.State().DesiredTarget != 104<<20 {
		t.Fatal("discarded candidate remained as local intent")
	}
}

func TestMemoryProgressStaticPartialBudgetUsesSameSettlement(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, 1<<30)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 320<<20, 705<<20
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newProgressController(t, fakeCH, cgroup, reservation)
	m.reservation = nil // static mode: the same controller without a node adapter
	if err := processProgressReport(t, m, 1, 449<<20); err != nil {
		t.Fatal(err)
	}
	if m.reservationNow() != 705<<20 || m.txn != nil || len(reservation.seen()) != 0 {
		t.Fatal("static mode did not use the same exact partial settlement")
	}
}

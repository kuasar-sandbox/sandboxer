package resctl

import (
	"context"
	"errors"
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

			// Fresh reports may retry a stalled accepted target, but must not
			// accumulate new targets or rewrite identical high/reservation.
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
			if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{target, target, target}) {
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
			if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{target, target, target}) {
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
	// Cancelling the unissued candidate does not itself retry anything. Two
	// later fresh reports may wake only the old, already authorized target.
	for seq := uint64(3); seq <= 4; seq++ {
		if err := processProgressReport(t, m, seq, 685<<20); err != nil {
			t.Fatal(err)
		}
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{100 << 20}) {
		t.Fatalf("stalled recovery revived the unissued candidate: %v", calls)
	}
	if len(reservation.seen()) != 0 {
		t.Fatal("same-target recovery changed reservation")
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

func newStalledProgressController(t *testing.T, reserved uint64) (*MemoryController, *fakeCHMemory, *fakeReservationAdapter, string) {
	t.Helper()
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, 8<<30)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget, f.currentBudget = 7552<<20, 801<<20
	})
	reservation := &fakeReservationAdapter{current: reserved}
	return newProgressController(t, fakeCH, cgroup, reservation), fakeCH, reservation, cgroup
}

func TestMemoryProgressRetriesShortBurstWithoutNewReservation(t *testing.T) {
	for _, static := range []bool{false, true} {
		t.Run(fmt.Sprintf("static=%v", static), func(t *testing.T) {
			m, fakeCH, reservation, cgroup := newStalledProgressController(t, 640<<20)
			reservation.zeroGrants = 100
			if static {
				m.reservation = nil
			}
			for seq := uint64(1); seq <= 2; seq++ {
				if err := processProgressReport(t, m, seq, 545<<20); err != nil {
					t.Fatal(err)
				}
				if got := len(fakeCH.calls()); got != int(seq-1) {
					t.Fatalf("seq=%d resize count=%d; need two fresh samples", seq, got)
				}
			}
			if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{7552 << 20}) {
				t.Fatalf("recovery changed target: %v", calls)
			}
			if m.txn != nil || m.reservationNow() != 640<<20 || len(reservation.seen()) != 0 {
				t.Fatalf("recovery retained a transaction or requested Budget: txn=%+v calls=%+v", m.txn, reservation.seen())
			}
			if m.state.CurrentBudget != 801<<20 || m.state.ObservedBudget != 801<<20 {
				t.Fatalf("successful notification fabricated actual progress: %+v", m.state)
			}
			if high, err := os.ReadFile(filepath.Join(cgroup, "memory.high")); err != nil || string(high) != "max" {
				t.Fatalf("uncovered recovery changed high: %q, %v", high, err)
			}

			// Real observed convergence unlocks the ordinary next 64MiB step.
			reservation.mu.Lock()
			reservation.zeroGrants = 0
			reservation.mu.Unlock()
			fakeCH.configure(func(f *fakeCHMemory) {
				f.currentBudget, f.autoConverge = 640<<20, true
			})
			if err := processProgressReport(t, m, 3, 384<<20); err != nil {
				t.Fatal(err)
			}
			if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{7552 << 20, 7616 << 20}) {
				t.Fatalf("ordinary actual-bounded progress did not resume: %v", calls)
			}
			if m.reservationNow() != 576<<20 || m.state.CurrentBudget != 576<<20 {
				t.Fatalf("observed progress did not settle exactly: %+v", m.state)
			}
			for _, call := range reservation.seen() {
				if call.delta != 0 {
					t.Fatalf("recovery requested additional Budget: %+v", call)
				}
			}
		})
	}
}

func TestMemoryProgressStalledRetryRequiresEligibleControlState(t *testing.T) {
	for _, name := range []string{"unreserved_target", "queued_pressure", "pressure_floor", "normalization", "pending_grow", "higher_demand", "report_fence", "different_intent", "already_reached"} {
		t.Run(name, func(t *testing.T) {
			m, fakeCH, reservation, _ := newStalledProgressController(t, 640<<20)
			available := uint64(545 << 20)
			switch name {
			case "unreserved_target":
				reservation.current = 576 << 20
			case "queued_pressure":
				m.RequestPressureGrow(resource.UrgencyHigh, "oom")
			case "pressure_floor":
				m.pressureFloor = 512 << 20
			case "normalization":
				m.normalizationPending = true
			case "pending_grow":
				reservation.zeroGrants = 100
				m.txn = &memoryTransaction{kind: memoryTransactionGrow, objectiveBudget: 832 << 20}
			case "higher_demand":
				reservation.zeroGrants = 100
				available = 0
			case "report_fence":
				m.shrinkReportFence = 2
			case "different_intent":
				if err := m.balloon.SetDesiredTarget(7616 << 20); err != nil {
					t.Fatal(err)
				}
			case "already_reached":
				fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget = 640 << 20 })
				available = 256 << 20
			}
			for seq := uint64(1); seq <= 2; seq++ {
				if err := processProgressReport(t, m, seq, available); err != nil {
					t.Fatal(err)
				}
			}
			if calls := fakeCH.calls(); len(calls) != 0 {
				t.Fatalf("ineligible state retried a target: %v", calls)
			}
			if name == "pending_grow" || name == "higher_demand" {
				if m.txn == nil || m.txn.kind != memoryTransactionGrow || len(reservation.seen()) == 0 {
					t.Fatal("recovery bypassed the normal reservation-backed grow")
				}
			}
		})
	}
}

func TestMemoryProgressStalledRetryRechecksBeforeNotification(t *testing.T) {
	for _, name := range []string{"target", "progress_one_page", "rebound", "reached", "new_report", "queued_pressure", "reservation", "info_failure", "capacity"} {
		t.Run(name, func(t *testing.T) {
			m, fakeCH, reservation, _ := newStalledProgressController(t, 640<<20)
			if err := processProgressReport(t, m, 1, 545<<20); err != nil {
				t.Fatal(err)
			}
			fakeCH.configure(func(f *fakeCHMemory) {
				f.onInfo = func(f *fakeCHMemory) {
					if f.infoCalls != 3 { // second report's immediate pre-retry observation
						return
					}
					switch name {
					case "target":
						f.acceptedTarget -= 64 << 20
					case "progress_one_page":
						f.currentBudget -= balloonPageSize
					case "rebound":
						f.currentBudget += balloonPageSize
					case "reached":
						f.currentBudget = 640 << 20
					case "new_report":
						m.SubmitGuestReport(proto.MemReport{Epoch: 1, Seq: 3, MemAvailableBytes: 0})
					case "queued_pressure":
						m.RequestPressureGrow(resource.UrgencyHigh, "oom")
					case "reservation":
						reservation.mu.Lock()
						reservation.current = 576 << 20
						reservation.mu.Unlock()
					case "info_failure":
						f.infoFailures = 1
					case "capacity":
						f.reportedCap -= balloonPageSize
					}
				}
			})
			err := processProgressReport(t, m, 2, 545<<20)
			wantError := name == "info_failure" || name == "capacity"
			if (err != nil) != wantError {
				t.Fatalf("error=%v, want error=%v", err, wantError)
			}
			if calls := fakeCH.calls(); len(calls) != 0 {
				t.Fatalf("changed observation/report authorized retry: %v", calls)
			}
			if m.balloonStall != nil {
				t.Fatal("cancelled retry retained its two-report evidence")
			}
		})
	}
}

func TestMemoryProgressStalledRetryNeedsTwoFreshReportsPerAttempt(t *testing.T) {
	m, fakeCH, _, _ := newStalledProgressController(t, 640<<20)
	if err := processProgressReport(t, m, 1, 545<<20); err != nil {
		t.Fatal(err)
	}
	m.SubmitGuestReport(proto.MemReport{Epoch: 1, Seq: 1, MemAvailableBytes: 545 << 20})
	if len(m.reportEvents) != 0 {
		t.Fatal("duplicate report became fresh retry evidence")
	}
	for range 4 {
		m.controlMu.Lock()
		err := m.retryLocked(context.Background())
		m.controlMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(fakeCH.calls()) != 0 {
		t.Fatal("ticker used an old report to retry")
	}
	// Even one page of forward or reverse motion starts a new sample pair.
	fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget -= balloonPageSize })
	if err := processProgressReport(t, m, 2, 545<<20); err != nil {
		t.Fatal(err)
	}
	fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget += balloonPageSize })
	if err := processProgressReport(t, m, 3, 545<<20); err != nil {
		t.Fatal(err)
	}
	if len(fakeCH.calls()) != 0 {
		t.Fatal("actual motion counted as a plateau")
	}
	for seq := uint64(4); seq <= 5; seq++ {
		if err := processProgressReport(t, m, seq, 545<<20); err != nil {
			t.Fatal(err)
		}
	}
	if len(fakeCH.calls()) != 1 {
		t.Fatalf("attempt did not consume its samples: %v", fakeCH.calls())
	}
	// A legitimate new report epoch cannot reuse the previous epoch's sample.
	m.openReportBarrier()
	for seq := uint64(1); seq <= 2; seq++ {
		report := proto.MemReport{Epoch: 2, Seq: seq, MemAvailableBytes: 545 << 20}
		if !m.SubmitGuestReport(report) {
			t.Fatal("new epoch report was not accepted")
		}
		m.controlMu.Lock()
		err := m.processReportLocked(context.Background(), <-m.reportEvents)
		m.controlMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if got := len(fakeCH.calls()); got != int(seq) {
			t.Fatalf("new epoch seq=%d resize count=%d", seq, got)
		}
	}
}

func TestMemoryProgressSameTargetRetryLostACKIsNotConfirmation(t *testing.T) {
	m, fakeCH, _, _ := newStalledProgressController(t, 640<<20)
	fakeCH.configure(func(f *fakeCHMemory) { f.dropResizeACKs = 1 })
	if err := processProgressReport(t, m, 1, 545<<20); err != nil {
		t.Fatal(err)
	}
	if err := processProgressReport(t, m, 2, 545<<20); err == nil {
		t.Fatal("same target incorrectly confirmed delivery after lost ACK")
	}
	if m.balloonStall != nil || m.txn != nil || m.state.CurrentBudget != 801<<20 {
		t.Fatal("ambiguous retry retained evidence/transaction or fabricated actual")
	}
	// Ordinary Apply remains idempotent confirmation, even with lagging actual.
	if _, err := m.balloon.ApplyTarget(context.Background(), 7552<<20); err != nil {
		t.Fatal(err)
	}
	if len(fakeCH.calls()) != 1 {
		t.Fatal("ordinary Apply started retrying already accepted targets")
	}
	for seq := uint64(3); seq <= 4; seq++ {
		if err := processProgressReport(t, m, seq, 545<<20); err != nil {
			t.Fatal(err)
		}
		if got := len(fakeCH.calls()); got != int(seq-2) {
			t.Fatalf("retry after lost ACK reused samples: seq=%d calls=%v", seq, fakeCH.calls())
		}
	}
}

func TestMemoryProgressStalledRetryHonorsSnapshotAndCancellation(t *testing.T) {
	m, fakeCH, _, _ := newStalledProgressController(t, 640<<20)
	if err := processProgressReport(t, m, 1, 545<<20); err != nil {
		t.Fatal(err)
	}
	release, err := m.BeginSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := make(chan struct{})
	fakeCH.configure(func(f *fakeCHMemory) {
		f.onInfo = func(f *fakeCHMemory) {
			if f.infoCalls == 2 {
				close(observed)
			}
		}
	})
	m.SubmitGuestReport(proto.MemReport{Epoch: 1, Seq: 2, MemAvailableBytes: 545 << 20})
	done := make(chan error, 1)
	go func() {
		m.controlMu.Lock()
		defer m.controlMu.Unlock()
		done <- m.processReportLocked(ctx, <-m.reportEvents)
	}()
	select {
	case <-observed:
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not observe while snapshot held the mutation gate")
	}
	if calls := fakeCH.calls(); len(calls) != 0 {
		t.Fatalf("retry crossed snapshot gate: %v", calls)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled retry returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock retry")
	}
	release()
	for seq := uint64(3); seq <= 4; seq++ {
		if err := processProgressReport(t, m, seq, 545<<20); err != nil {
			t.Fatal(err)
		}
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{7552 << 20}) {
		t.Fatalf("post-snapshot recovery did not use fresh evidence: %v", calls)
	}
}

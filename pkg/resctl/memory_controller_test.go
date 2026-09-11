package resctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

type reservationCall struct {
	current uint64
	delta   uint64
	reason  string
}

type fakeReservationAdapter struct {
	mu         sync.Mutex
	current    uint64
	calls      []reservationCall
	failures   int
	zeroGrants int
	grantMax   uint64
	cooldown   time.Duration
	onCall     func(reservationCall)
}

func (f *fakeReservationAdapter) Enabled() bool { return true }

func (f *fakeReservationAdapter) ReservationMemory() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

func (f *fakeReservationAdapter) RequestBudget(current, delta uint64, _, reason string) (uint64, uint64, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := reservationCall{current: current, delta: delta, reason: reason}
	f.calls = append(f.calls, call)
	if f.onCall != nil {
		f.onCall(call)
	}
	if f.failures > 0 {
		f.failures--
		return 0, f.current, 0, errors.New("injected reservation failure")
	}
	if f.zeroGrants > 0 {
		f.zeroGrants--
		return 0, f.current, f.cooldown, nil
	}
	if current > f.current {
		return 0, f.current, 0, errors.New("state mismatch")
	}
	granted := delta
	if f.grantMax > 0 && granted > f.grantMax {
		granted = f.grantMax
	}
	next := current + granted
	f.current = next
	return granted, next, f.cooldown, nil
}

func (f *fakeReservationAdapter) seen() []reservationCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reservationCall(nil), f.calls...)
}

func memoryTestConfig(cgroupPath string) *config.SandboxConfig {
	cfg := &config.SandboxConfig{Resources: config.ResourcesConfig{
		Capacity:    config.CapacityConfig{CPU: 1, Memory: "1GiB"},
		Allocatable: config.AllocatableConfig{CPU: 1, Memory: "256MiB"},
		Overhead:    &config.OverheadConfig{Memory: "64MiB"},
		Control:     config.ControlConfig{CgroupPath: cgroupPath},
	}}
	cfg.ApplyDefaults()
	return cfg
}

func newMemoryCgroup(t *testing.T, high, current string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.high"), []byte(high), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newMemoryControllerForTest(t *testing.T, fakeCH *fakeCHMemory, cgroup string, initial uint64, reservation *fakeReservationAdapter) *MemoryController {
	t.Helper()
	balloon := NewBalloonController(fakeCH.sock, 1<<30, time.Second, nil)
	fakeCH.mu.Lock()
	acceptedTarget := fakeCH.acceptedTarget
	fakeCH.mu.Unlock()
	if err := balloon.SeedColdTarget(acceptedTarget); err != nil {
		t.Fatal(err)
	}
	memoryCtl, err := NewMemoryController(MemoryControllerOptions{
		Config: memoryTestConfig(cgroup), CgroupPath: cgroup, Balloon: balloon,
		InitialBudget: initial, Interval: time.Millisecond, Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	memoryCtl.reservation = reservation
	return memoryCtl
}

func TestMemoryControllerDoesNotLowerHighWithoutHostCharge(t *testing.T) {
	for _, current := range []string{"", "not-a-number"} {
		t.Run(current, func(t *testing.T) {
			cgroup := newMemoryCgroup(t, "536870912", current)
			if current == "" {
				if err := os.Remove(filepath.Join(cgroup, "memory.current")); err != nil {
					t.Fatal(err)
				}
			}
			cfg := memoryTestConfig(cgroup)
			memoryCtl, err := NewMemoryController(MemoryControllerOptions{
				Config: cfg, CgroupPath: cgroup, InitialBudget: 1 << 30,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := memoryCtl.applyMemoryHigh(context.Background(), 256<<20, 128<<20, false); err == nil {
				t.Fatal("memory.high was allowed without a trustworthy memory.current")
			}
			high, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
			if err != nil {
				t.Fatal(err)
			}
			if string(high) != "536870912" {
				t.Fatalf("memory.high changed on charge-read failure: %q", high)
			}
		})
	}
}

func TestMemoryControllerSamplesHostChargeAfterLifecycleLock(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	cfg := memoryTestConfig(cgroup)
	memoryCtl, err := NewMemoryController(MemoryControllerOptions{
		Config: cfg, CgroupPath: cgroup, InitialBudget: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate snapshot/shutdown holding the lifecycle lock while the VMM
	// charge rises. applyMemoryHigh must wait first and sample the later charge,
	// otherwise it could write a stale, immediately throttling value.
	lifecycleLock, err := lockMemoryHigh(context.Background(), cgroup)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		done <- memoryCtl.applyMemoryHigh(context.Background(), 512<<20, 256<<20, false)
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("memory.high update crossed lifecycle lock: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	const laterCharge = uint64(900 << 20)
	if err := os.WriteFile(filepath.Join(cgroup, "memory.current"), []byte(strconv.FormatUint(laterCharge, 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := lifecycleLock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	highRaw, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	high, err := strconv.ParseUint(string(highRaw), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if high < laterCharge {
		t.Fatalf("memory.high=%d below charge sampled after lifecycle lock=%d", high, laterCharge)
	}
}

func TestMemoryControllerNoBalloonSnapshotBlocksMemoryHigh(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	cfg := memoryTestConfig(cgroup)
	cfg.Resources.Allocatable.Memory = "1GiB"
	memoryCtl, err := NewMemoryController(MemoryControllerOptions{
		Config: cfg, CgroupPath: cgroup, InitialBudget: capacity,
	})
	if err != nil {
		t.Fatal(err)
	}

	release, err := memoryCtl.BeginSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		memoryCtl.controlMu.Lock()
		defer memoryCtl.controlMu.Unlock()
		done <- memoryCtl.processReportLocked(context.Background(), proto.MemReport{
			Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: capacity,
		})
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("memory.high update crossed no-balloon snapshot barrier: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("memory.high update did not resume after snapshot barrier release")
	}
}

func TestMemoryControllerPressureBeforeFirstReportUsesInitialState(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	cfg := memoryTestConfig(cgroup)
	cfg.Resources.Allocatable.Memory = "1GiB"
	memoryCtl, err := NewMemoryController(MemoryControllerOptions{
		Config: cfg, CgroupPath: cgroup, InitialBudget: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	memoryCtl.controlMu.Lock()
	err = memoryCtl.processPressureLocked(context.Background(), pressureGrow{
		urgency: "normal", reason: "psi_before_first_report",
	})
	memoryCtl.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if memoryCtl.txn != nil {
		t.Fatalf("full-Budget no-balloon controller created a grow transaction: %+v", memoryCtl.txn)
	}
	state := memoryCtl.State()
	if state.TargetBudget != 1<<30 || state.CurrentBudget != 1<<30 || state.ObservedBudget != 1<<30 {
		t.Fatalf("initial no-balloon state = %+v, want full Capacity on both sides", state)
	}
}

func TestMemoryControllerRejectsSubCapacityInitialBudgetWithoutBalloon(t *testing.T) {
	cgroup := newMemoryCgroup(t, "max", "1")
	_, err := NewMemoryController(MemoryControllerOptions{
		Config: memoryTestConfig(cgroup), CgroupPath: cgroup, InitialBudget: 512 << 20,
	})
	if err == nil {
		t.Fatal("sub-Capacity InitialBudget without balloon was accepted")
	}
}

func TestMemoryControllerGrowOrdersReservationHighResizeAndDoesNotWaitActual(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, strconv.FormatUint(400<<20, 10), "1")
	fakeCH := newFakeCHMemory(t, capacity)
	var orderMu sync.Mutex
	var order []string
	reservation := &fakeReservationAdapter{current: 512 << 20}
	reservation.onCall = func(reservationCall) {
		orderMu.Lock()
		order = append(order, "reserve")
		orderMu.Unlock()
	}
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 512 << 20
		f.onResize = func(uint64) {
			high, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
			if err != nil {
				t.Errorf("read high at resize: %v", err)
			}
			value, _ := strconv.ParseUint(string(high), 10, 64)
			if value <= 400<<20 {
				t.Errorf("resize observed memory.high=%d before grow", value)
			}
			orderMu.Lock()
			order = append(order, "resize")
			orderMu.Unlock()
		}
	})
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 512<<20, reservation)
	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 0,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if !reflect.DeepEqual(gotOrder, []string{"reserve", "resize"}) {
		t.Fatalf("operation order = %v", gotOrder)
	}
	if reservation.ReservationMemory() != 768<<20 {
		t.Fatalf("reservation = %d, want %d", reservation.ReservationMemory(), uint64(768<<20))
	}
	state := m.balloon.State()
	if state.AcceptedTarget != 256<<20 || state.CurrentBudget != 512<<20 {
		t.Fatalf("grow state = %+v", state)
	}
	if state.TargetReached(capacity) {
		t.Fatal("grow fabricated/waited for actual convergence")
	}
}

func TestMemoryControllerInitialReportCanGrowWithoutTargetEquality(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 640 << 20 // emergency deflate before the first report
	})
	reservation := &fakeReservationAdapter{current: 512 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 512<<20, reservation)
	m.initialObservationPending = true

	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 256 << 20,
	})
	pending := m.initialObservationPending
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("valid CH observation did not complete the initial report barrier")
	}
	if calls := reservation.seen(); !reflect.DeepEqual(calls, []reservationCall{{
		current: 512 << 20, delta: 128 << 20, reason: "guest_report",
	}}) {
		t.Fatalf("reservation calls = %+v, want one safety grow", calls)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{384 << 20}) {
		t.Fatalf("balloon calls = %v, want safety target", calls)
	}
}

func TestMemoryControllerPressureGrowAllowedBeforeInitialReport(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 768 << 20
	})
	reservation := &fakeReservationAdapter{current: 512 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 512<<20, reservation)
	m.initialObservationPending = true

	m.controlMu.Lock()
	err := m.processPressureLocked(context.Background(), pressureGrow{urgency: resource.UrgencyHigh, reason: "oom"})
	pending := m.initialObservationPending
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("pressure grow incorrectly completed the initial report barrier")
	}
	if calls := reservation.seen(); !reflect.DeepEqual(calls, []reservationCall{{
		current: 512 << 20, delta: 64 << 20, reason: "oom",
	}}) {
		t.Fatalf("reservation calls = %+v, want one pressure grow", calls)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{448 << 20}) {
		t.Fatalf("balloon calls = %v, want one-step pressure target", calls)
	}
}

func TestMemoryControllerAccumulatesPartialGrantBeforeDeflate(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "1", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 512 << 20
	})
	reservation := &fakeReservationAdapter{current: 512 << 20, grantMax: 50 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 512<<20, reservation)

	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 0,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := reservation.ReservationMemory(); got != 562<<20 {
		t.Fatalf("partial reservation = %d, want %d", got, uint64(562<<20))
	}
	if calls := fakeCH.calls(); len(calls) != 0 {
		t.Fatalf("partial grant caused unreserved deflate: %v", calls)
	}

	// The retained objective progresses from the controller ticker; it does not
	// depend on another guest observation happening to repeat the request.
	m.controlMu.Lock()
	err = m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := reservation.ReservationMemory(); got != 612<<20 {
		t.Fatalf("accumulated reservation = %d, want %d", got, uint64(612<<20))
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{448 << 20}) {
		t.Fatalf("deflate calls = %v, want one represented 64MiB step", calls)
	}
	if got := BudgetFromTarget(capacity, m.balloon.State().AcceptedTarget); got != 576<<20 || got > reservation.ReservationMemory() {
		t.Fatalf("applied Budget = %d, reservation = %d", got, reservation.ReservationMemory())
	}
}

func TestMemoryControllerReservationFailureRetainsGrowObjective(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "1", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 512 << 20
	})
	reservation := &fakeReservationAdapter{current: 512 << 20, failures: 1}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 512<<20, reservation)

	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 0,
	})
	m.controlMu.Unlock()
	if err == nil {
		t.Fatal("injected reservation failure was hidden")
	}
	if m.txn == nil || m.txn.kind != memoryTransactionGrow || m.txn.growObjective() != 768<<20 {
		t.Fatalf("grow objective was forgotten after reservation failure: %+v", m.txn)
	}
	if calls := fakeCH.calls(); len(calls) != 0 {
		t.Fatalf("resize ran without reservation: %v", calls)
	}

	m.controlMu.Lock()
	err = m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := reservation.ReservationMemory(); got != 768<<20 {
		t.Fatalf("retried reservation = %d, want %d", got, uint64(768<<20))
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{256 << 20}) {
		t.Fatalf("forward retry resize calls = %v", calls)
	}
}

func TestMemoryControllerZeroGrantRetainsGrowObjectiveAndHighEscalationBypassesCooldown(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "1", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 512 << 20
	})
	reservation := &fakeReservationAdapter{
		current: 512 << 20, zeroGrants: 1, cooldown: time.Hour,
	}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 512<<20, reservation)

	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 0,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if m.txn == nil || m.txn.growObjective() != 768<<20 || m.txn.reservationRetryAt.IsZero() {
		t.Fatalf("zero grant did not retain a cooldown-bound objective: %+v", m.txn)
	}
	m.controlMu.Lock()
	err = m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := reservation.seen(); len(calls) != 1 {
		t.Fatalf("cooldown was ignored: %+v", calls)
	}

	m.controlMu.Lock()
	err = m.processPressureLocked(context.Background(), pressureGrow{
		urgency: resource.UrgencyHigh, reason: "oom_event",
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := reservation.ReservationMemory(); got != 832<<20 {
		t.Fatalf("high-urgency reservation = %d, want %d", got, uint64(832<<20))
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{192 << 20}) {
		t.Fatalf("high-urgency resize calls = %v", calls)
	}
}

func TestMemoryControllerGrowHighFailureRetainsReservationAndRetriesForward(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := t.TempDir()
	if err := os.WriteFile(filepath.Join(cgroup, "memory.current"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 512 << 20
	})
	reservation := &fakeReservationAdapter{current: 512 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 512<<20, reservation)
	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{Epoch: 1, Seq: 1, MemTotalBytes: capacity})
	m.controlMu.Unlock()
	if err == nil {
		t.Fatal("missing memory.high did not fail grow")
	}
	if reservation.ReservationMemory() != 768<<20 {
		t.Fatalf("reservation rolled back to %d", reservation.ReservationMemory())
	}
	if calls := fakeCH.calls(); len(calls) != 0 {
		t.Fatalf("resize ran before high succeeded: %v", calls)
	}
	if err := os.WriteFile(filepath.Join(cgroup, "memory.high"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.controlMu.Lock()
	err = m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{256 << 20}) {
		t.Fatalf("forward retry calls = %v", calls)
	}
	if len(reservation.seen()) != 1 {
		t.Fatalf("reservation was rolled back/re-requested: %+v", reservation.seen())
	}
}

func TestMemoryControllerGuestReportDoesNotReducePendingGrowTarget(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "1", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 512 << 20
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 768<<20, reservation)
	m.txn = &memoryTransaction{
		kind: memoryTransactionGrow, targetBudget: 768 << 20, target: 256 << 20,
		demand: 600 << 20,
	}

	m.controlMu.Lock()
	err := m.startGrowLocked(context.Background(), 640<<20, 384<<20, "normal", "guest_report")
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if m.txn != nil {
		t.Fatalf("pending grow did not complete: %+v", m.txn)
	}
	if got := BudgetFromTarget(capacity, m.balloon.State().AcceptedTarget); got != 768<<20 {
		t.Fatalf("smaller report reduced pending grow Budget to %d, want %d", got, uint64(768<<20))
	}
	if calls := reservation.seen(); len(calls) != 0 {
		t.Fatalf("existing reservation was re-requested: %+v", calls)
	}
}

func TestMemoryControllerResizeFailureNeverRollsBackHighOrTarget(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "1", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 512 << 20
		f.resizeFailures = 1
	})
	reservation := &fakeReservationAdapter{current: 512 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 512<<20, reservation)
	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{Epoch: 1, Seq: 1, MemTotalBytes: capacity})
	m.controlMu.Unlock()
	if err == nil {
		t.Fatal("resize rejection was hidden")
	}
	if m.balloon.State().DesiredTarget != 256<<20 {
		t.Fatalf("desired target rolled back: %+v", m.balloon.State())
	}
	highAfterFailure, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	if string(highAfterFailure) == "1" {
		t.Fatal("memory.high was rolled back")
	}
	m.controlMu.Lock()
	err = m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{256 << 20, 256 << 20}) {
		t.Fatalf("resize calls = %v, want forward target only", calls)
	}
}

func TestMemoryControllerShrinkSettlesObservedProgressAfterHigh(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "900000000", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	var commitHigh uint64
	reservation := &fakeReservationAdapter{current: 768 << 20}
	reservation.onCall = func(call reservationCall) {
		if call.delta == 0 {
			raw, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
			if err != nil {
				t.Errorf("read high at commit: %v", err)
				return
			}
			commitHigh, _ = strconv.ParseUint(string(raw), 10, 64)
		}
	}
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 256 << 20
		f.currentBudget = 768 << 20
	})
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 768<<20, reservation)
	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 512 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20}) {
		t.Fatalf("first shrink calls = %v", calls)
	}
	if reservation.ReservationMemory() != 768<<20 || commitHigh != 0 {
		t.Fatalf("released without observed reclamation: reservation=%d commitHigh=%d", reservation.ReservationMemory(), commitHigh)
	}
	fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget = 704 << 20 })
	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 2, MemTotalBytes: capacity, MemAvailableBytes: 256 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if reservation.ReservationMemory() != 704<<20 {
		t.Fatalf("reservation = %d, want %d", reservation.ReservationMemory(), uint64(704<<20))
	}
	if commitHigh == 0 || commitHigh >= 900000000 {
		t.Fatalf("reservation committed before lower high; observed high=%d", commitHigh)
	}
	if calls := reservation.seen(); len(calls) != 1 || calls[0].current != 704<<20 || calls[0].delta != 0 {
		t.Fatalf("shrink reservation calls = %+v", calls)
	}
	// A ticker retry cannot consume the old report for a second step.
	m.controlMu.Lock()
	err = m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); len(calls) != 1 {
		t.Fatalf("one report drove multiple shrink steps: %v", calls)
	}
	// A new report may drive exactly one additional step.
	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 3, MemTotalBytes: capacity, MemAvailableBytes: 448 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20, 384 << 20}) {
		t.Fatalf("second fresh-report step calls = %v", calls)
	}
}

func TestMemoryControllerShrinkCommitFailureKeepsConservativeReservation(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "900000000", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 256 << 20
		f.currentBudget = 768 << 20
		f.autoConverge = true
	})
	reservation := &fakeReservationAdapter{current: 768 << 20, failures: 1}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 768<<20, reservation)
	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 512 << 20,
	})
	m.controlMu.Unlock()
	if err == nil {
		t.Fatal("injected shrink commit failure was hidden")
	}
	if reservation.ReservationMemory() != 768<<20 {
		t.Fatalf("failed commit changed reservation to %d", reservation.ReservationMemory())
	}
	m.controlMu.Lock()
	err = m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if reservation.ReservationMemory() != 704<<20 {
		t.Fatalf("retry reservation = %d", reservation.ReservationMemory())
	}
}

func TestMemoryControllerPendingShrinkRecomputesHighForNewerDemand(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "900000000", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 256 << 20
		f.currentBudget = 768 << 20
		f.autoConverge = true
	})
	reservation := &fakeReservationAdapter{current: 768 << 20, failures: 1}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 768<<20, reservation)

	// The first report completes one balloon step and lowers high, but the
	// node-side shrink commit fails and remains pending.
	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 768 << 20,
	})
	m.controlMu.Unlock()
	if err == nil {
		t.Fatal("injected shrink commit failure was hidden")
	}
	beforeRaw, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := strconv.ParseUint(string(beforeRaw), 10, 64)
	if err != nil {
		t.Fatal(err)
	}

	// A newer report still supports the same shrunken Budget but carries a
	// larger DemandMemory. high must be recomputed before reservation release.
	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 2, MemTotalBytes: capacity, MemAvailableBytes: 448 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	afterRaw, err := os.ReadFile(filepath.Join(cgroup, "memory.high"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := strconv.ParseUint(string(afterRaw), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("memory.high did not advance for newer demand: before=%d after=%d", before, after)
	}
	if got := reservation.ReservationMemory(); got != 704<<20 {
		t.Fatalf("reservation=%d, want committed Budget=%d", got, uint64(704<<20))
	}
}

func TestMemoryControllerPendingShrinkWaitsForLatestAcceptedReport(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "900000000", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 256 << 20
		f.currentBudget = 768 << 20
		f.autoConverge = true
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 768<<20, reservation)

	// Seq 2 and 3 arrived while seq 1's shrink was still in flight. Even after
	// actual converges, seq 1 must not lower high or release reservation while
	// a newer accepted observation is waiting to be processed.
	m.reportMu.Lock()
	m.reportEpoch = 1
	m.reportSeq = 1
	m.reportMu.Unlock()
	fakeCH.configure(func(f *fakeCHMemory) {
		f.onResize = func(uint64) {
			m.reportMu.Lock()
			m.reportSeq = 3
			m.reportMu.Unlock()
		}
	})
	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 512 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20}) {
		t.Fatalf("first shrink calls = %v", calls)
	}
	if got := reservation.ReservationMemory(); got != 768<<20 {
		t.Fatalf("superseded shrink released reservation=%d", got)
	}

	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 2, MemTotalBytes: capacity, MemAvailableBytes: 448 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20}) {
		t.Fatalf("queued pre-convergence report drove another shrink: %v", calls)
	}
	if got := reservation.ReservationMemory(); got != 768<<20 {
		t.Fatalf("report behind accepted seq released reservation=%d", got)
	}

	m.reportMu.Lock()
	m.reportSeq = 4
	m.reportMu.Unlock()
	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 4, MemTotalBytes: capacity, MemAvailableBytes: 448 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20}) {
		t.Fatalf("one report both committed and drove another step: %v", calls)
	}
	if got := reservation.ReservationMemory(); got != 704<<20 {
		t.Fatalf("latest valid report did not commit converged shrink: reservation=%d", got)
	}

	// Committing the pending step consumes seq 4. A separate fresh report is
	// required to authorize the next 64 MiB shrink.
	m.reportMu.Lock()
	m.reportSeq = 5
	m.reportMu.Unlock()
	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 5, MemTotalBytes: capacity, MemAvailableBytes: 448 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20, 384 << 20}) {
		t.Fatalf("separate fresh report did not drive the next step: %v", calls)
	}
}

func TestMemoryControllerAvailableAboveCurrentSaturatesAndConsumesReport(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "900000000", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 256 << 20
		f.currentBudget = 768 << 20
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 768<<20, reservation)
	m.openReportBarrier()

	first := proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 512 << 20,
	}
	if !m.SubmitGuestReport(first) {
		t.Fatal("first report rejected")
	}
	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), <-m.reportEvents)
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget = 704 << 20 })

	saturating := proto.MemReport{
		Epoch: 1, Seq: 2, MemTotalBytes: capacity, MemAvailableBytes: 705 << 20,
	}
	if !m.SubmitGuestReport(saturating) {
		t.Fatal("newer report rejected at the open transport barrier")
	}
	m.controlMu.Lock()
	err = m.retryLocked(context.Background())
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := reservation.ReservationMemory(); got != 768<<20 {
		t.Fatalf("unprocessed newer report allowed shrink commit: reservation=%d", got)
	}
	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), <-m.reportEvents)
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := reservation.ReservationMemory(); got != 704<<20 {
		t.Fatalf("saturating report did not settle observed progress: reservation=%d", got)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20, 384 << 20}) {
		t.Fatalf("fresh saturating report did not authorize exactly one bounded target: %v", calls)
	}

	valid := proto.MemReport{
		Epoch: 1, Seq: 3, MemTotalBytes: capacity, MemAvailableBytes: 448 << 20,
	}
	if !m.SubmitGuestReport(valid) {
		t.Fatal("later valid report rejected")
	}
	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), <-m.reportEvents)
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := reservation.ReservationMemory(); got != 704<<20 {
		t.Fatalf("unexecuted target released reservation: reservation=%d", got)
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20, 384 << 20}) {
		t.Fatalf("unchanged actual accumulated another target: %v", calls)
	}
}

func TestMemoryControllerCaptureStateUsesSaturatingDemandFormula(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 320 << 20
		f.currentBudget = 704 << 20
	})
	reservation := &fakeReservationAdapter{current: 704 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 704<<20, reservation)
	m.openReportBarrier()
	if !m.SubmitGuestReport(proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 705 << 20,
	}) {
		t.Fatal("diagnostic report rejected")
	}

	state, err := m.CaptureState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.DemandMemory != 0 || state.RequestedBudget != 256<<20 {
		t.Fatalf("capture formula = demand %d requested %d, want 0/%d",
			state.DemandMemory, state.RequestedBudget, uint64(256<<20))
	}
}

func TestMemoryControllerSupersededReportCannotStartShrink(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "900000000", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 256 << 20
		f.currentBudget = 768 << 20
		f.autoConverge = true
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 768<<20, reservation)
	m.reportMu.Lock()
	m.reportEpoch = 1
	m.reportSeq = 2
	m.reportMu.Unlock()

	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 512 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); len(calls) != 0 {
		t.Fatalf("superseded report drove shrink: %v", calls)
	}
	if calls := reservation.seen(); len(calls) != 0 {
		t.Fatalf("superseded report released reservation: %+v", calls)
	}
}

func TestMemoryControllerPressureGrowOverridesPendingShrink(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "900000000", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 256 << 20
		f.currentBudget = 768 << 20
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 768<<20, reservation)
	m.controlMu.Lock()
	if err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 512 << 20,
	}); err != nil {
		m.controlMu.Unlock()
		t.Fatal(err)
	}
	if err := m.processPressureLocked(context.Background(), pressureGrow{urgency: "high", reason: "oom"}); err != nil {
		m.controlMu.Unlock()
		t.Fatal(err)
	}
	m.controlMu.Unlock()
	if reservation.ReservationMemory() != 768<<20 {
		t.Fatalf("pressure reservation = %d, want retained %d", reservation.ReservationMemory(), uint64(768<<20))
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20, 256 << 20}) {
		t.Fatalf("shrink then safety grow calls = %v", calls)
	}
}

func TestMemoryControllerFreshSafetyGrowPreemptsShrinkCommit(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "900000000", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 256 << 20
		f.currentBudget = 768 << 20
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 768<<20, reservation)

	// The first low-demand report starts one inflate step but actual remains at
	// the old Budget, so reservation must not be returned early.
	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 512 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget = 704 << 20 })

	// At convergence a fresh pressure observation needs 960 MiB. It must reuse
	// the still-conservative 768 MiB reservation and request only the grow tail;
	// committing the 704 MiB shrink first would expose a release/re-admit race.
	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 2, MemTotalBytes: capacity, MemAvailableBytes: 0,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := reservation.seen(); !reflect.DeepEqual(calls, []reservationCall{{
		current: 768 << 20, delta: 192 << 20, reason: "guest_report",
	}}) {
		t.Fatalf("reservation calls = %+v; shrink was released before safety grow", calls)
	}
	if got := reservation.ReservationMemory(); got != 960<<20 {
		t.Fatalf("reservation = %d, want %d", got, uint64(960<<20))
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{320 << 20, 64 << 20}) {
		t.Fatalf("balloon calls = %v, want shrink target then safety grow target", calls)
	}
}

func TestMemoryControllerPressureDoesNotReducePendingGrowTarget(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "1", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 512 << 20
	})
	reservation := &fakeReservationAdapter{current: 768 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 768<<20, reservation)
	m.txn = &memoryTransaction{
		kind: memoryTransactionGrow, targetBudget: 768 << 20, target: 256 << 20,
		demand: 512 << 20,
	}

	m.controlMu.Lock()
	err := m.processPressureLocked(context.Background(), pressureGrow{urgency: "high", reason: "oom"})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if m.txn != nil {
		t.Fatalf("pending grow did not complete: %+v", m.txn)
	}
	if got := BudgetFromTarget(capacity, m.balloon.State().AcceptedTarget); got != 832<<20 {
		t.Fatalf("pressure reduced pending grow objective: Budget=%d, want %d", got, uint64(832<<20))
	}
	if got := reservation.ReservationMemory(); got != 832<<20 {
		t.Fatalf("reservation=%d, want %d", got, uint64(832<<20))
	}
}

func TestMemoryControllerEmergencyDeflateStopsShrinkWithoutRetarget(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "900000000", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 640 << 20 // guest autonomously deflated 128 MiB
	})
	reservation := &fakeReservationAdapter{current: 512 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 512<<20, reservation)
	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 384 << 20,
	})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); len(calls) != 0 {
		t.Fatalf("emergency deflate triggered retarget: %v", calls)
	}
	if calls := reservation.seen(); len(calls) != 0 {
		t.Fatalf("emergency deflate changed node reservation: %+v", calls)
	}
}

func TestMemoryControllerColdSeparatesInitialObservationAndReservationCoverage(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 512 << 20
		f.currentBudget = 768 << 20
	})
	reservation := &fakeReservationAdapter{current: 512 << 20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, 512<<20, reservation)
	m.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartCold(ctx)
	defer m.Stop()

	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 512 << 20,
	})
	pending := m.initialObservationPending
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("valid cold observation was incorrectly blocked on equality")
	}
	if high, err := os.ReadFile(filepath.Join(cgroup, "memory.high")); err != nil || string(high) != "max" {
		t.Fatalf("unreserved cold actual changed high: %q, %v", high, err)
	}
	if calls := fakeCH.calls(); len(calls) != 0 {
		t.Fatalf("unreserved cold observation resized balloon: %v", calls)
	}
	if calls := reservation.seen(); len(calls) != 0 {
		t.Fatalf("unreserved cold observation changed reservation: %+v", calls)
	}

	fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget = 512 << 20 })
	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 1, Seq: 2, MemTotalBytes: capacity, MemAvailableBytes: 0,
	})
	pending = m.initialObservationPending
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("valid cold report did not open policy")
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{256 << 20}) {
		t.Fatalf("post-observation grow calls = %v, want one target", calls)
	}
	if got := reservation.ReservationMemory(); got != 768<<20 {
		t.Fatalf("post-observation reservation = %d, want %d", got, uint64(768<<20))
	}
}

func TestMemoryControllerReportBarrierRetriesClosedAndACKsIgnoredReports(t *testing.T) {
	m := &MemoryController{
		logf: func(string, ...any) {}, reportEvents: make(chan proto.MemReport, 16),
	}
	if m.SubmitGuestReport(proto.MemReport{Epoch: 1, Seq: 1}) {
		t.Fatal("pre-barrier report accepted")
	}
	m.openReportBarrier()
	if !m.SubmitGuestReport(proto.MemReport{Epoch: 4, Seq: 1}) {
		t.Fatal("first report rejected")
	}
	if got := len(m.reportEvents); got != 1 {
		t.Fatalf("queued reports = %d, want 1", got)
	}
	for _, report := range []proto.MemReport{
		{Epoch: 4, Seq: 1},
		{Epoch: 4, Seq: 0},
		{Epoch: 3, Seq: 2},
		{Epoch: 5, Seq: 2},
	} {
		if !m.SubmitGuestReport(report) {
			t.Fatalf("open barrier did not ACK ignored report: %+v", report)
		}
	}
	if got := len(m.reportEvents); got != 1 {
		t.Fatalf("ignored reports entered policy queue: %d", got)
	}
	if !m.SubmitGuestReport(proto.MemReport{Epoch: 4, Seq: 2}) {
		t.Fatal("monotonic report rejected")
	}
	if got := len(m.reportEvents); got != 2 {
		t.Fatalf("queued reports = %d, want 2", got)
	}
}

func TestMemoryControllerFullReportQueueFailsClosedAndRecovers(t *testing.T) {
	m := &MemoryController{
		logf: func(string, ...any) {}, reportEvents: make(chan proto.MemReport, 1),
	}
	m.openReportBarrier()
	first := proto.MemReport{Epoch: 7, Seq: 1, MemAvailableBytes: 1}
	dropped := proto.MemReport{Epoch: 7, Seq: 2, MemAvailableBytes: 2}
	next := proto.MemReport{Epoch: 7, Seq: 3, MemAvailableBytes: 3}
	if !m.SubmitGuestReport(first) || !m.SubmitGuestReport(dropped) {
		t.Fatal("open observation barrier did not ACK reports")
	}
	if got := <-m.reportEvents; got != first {
		t.Fatalf("queued report = %+v, want first report %+v", got, first)
	}
	m.reportMu.Lock()
	seq, last := m.reportSeq, m.lastReport
	m.reportMu.Unlock()
	if seq != dropped.Seq || last != dropped {
		t.Fatalf("full queue did not retain conservative sequence fence: seq=%d last=%+v", seq, last)
	}
	if !m.SubmitGuestReport(next) {
		t.Fatal("later periodic report did not recover progress")
	}
	if got := <-m.reportEvents; got != next {
		t.Fatalf("recovery report = %+v, want %+v", got, next)
	}
}

func TestMemoryControllerRestoreNormalizesAllSnapshotRelationsBeforeOpeningReports(t *testing.T) {
	const capacity = uint64(1 << 30)
	for _, tc := range []struct {
		name            string
		snapshotTarget  uint64
		snapshotCurrent uint64
		wantResize      []uint64
	}{
		{name: "target_equals_current", snapshotTarget: 300 << 20, snapshotCurrent: 300 << 20},
		{name: "current_below_target", snapshotTarget: 400 << 20, snapshotCurrent: 300 << 20, wantResize: []uint64{300 << 20}},
		{name: "target_below_current", snapshotTarget: 300 << 20, snapshotCurrent: 400 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cgroup := newMemoryCgroup(t, "max", "1")
			fakeCH := newFakeCHMemory(t, capacity)
			fakeCH.configure(func(f *fakeCHMemory) {
				f.acceptedTarget = tc.snapshotTarget
				f.currentBudget = capacity - tc.snapshotCurrent
			})
			safeTarget := min(tc.snapshotTarget, tc.snapshotCurrent)
			initialBudget := capacity - safeTarget
			reservation := &fakeReservationAdapter{current: initialBudget}
			m := newMemoryControllerForTest(t, fakeCH, cgroup, initialBudget, reservation)
			if err := m.balloon.SeedRestoredState(tc.snapshotTarget, tc.snapshotCurrent); err != nil {
				t.Fatal(err)
			}
			if m.SubmitGuestReport(proto.MemReport{Epoch: 2, Seq: 1}) {
				t.Fatal("pre-restore-ACK report accepted")
			}
			ctx, cancel := context.WithCancel(context.Background())
			if err := m.StartRestore(ctx, safeTarget); err != nil {
				cancel()
				t.Fatal(err)
			}
			// Stop the policy worker before probing the report barrier so this test
			// isolates normalization from the later steady Budget transaction.
			m.Stop()
			cancel()
			if calls := fakeCH.calls(); !reflect.DeepEqual(calls, tc.wantResize) {
				t.Fatalf("normalization calls = %v, want %v", calls, tc.wantResize)
			}
			if !m.SubmitGuestReport(proto.MemReport{Epoch: 2, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 256 << 20}) {
				t.Fatal("post-normalization restore report rejected")
			}
			if calls := reservation.seen(); len(calls) != 0 {
				t.Fatalf("normalization touched node reservation: %+v", calls)
			}
		})
	}
}

func TestMemoryControllerRestoreNormalizationSerializesConcurrentReport(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	resizeStarted := make(chan struct{})
	allowResize := make(chan struct{})
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 400 << 20
		f.currentBudget = capacity - 300<<20
		f.onResize = func(uint64) {
			close(resizeStarted)
			<-allowResize
		}
	})
	reservation := &fakeReservationAdapter{current: capacity - 300<<20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, capacity-300<<20, reservation)
	if err := m.balloon.SeedRestoredState(400<<20, 300<<20); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDone := make(chan error, 1)
	go func() { startDone <- m.StartRestore(ctx, 300<<20) }()
	<-resizeStarted

	report := proto.MemReport{
		Epoch: 2, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 256 << 20,
	}
	if m.SubmitGuestReport(report) {
		t.Fatal("report crossed in-flight restore normalization")
	}
	close(allowResize)
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if !m.SubmitGuestReport(report) {
		t.Fatal("same report was not accepted after normalization opened the barrier")
	}
	if calls := reservation.seen(); len(calls) != 0 {
		t.Fatalf("normalization/report race touched node reservation: %+v", calls)
	}
}

func TestMemoryControllerRestoreNormalizationBlocksPressurePolicy(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 400 << 20
		f.currentBudget = capacity - 300<<20
	})
	reservation := &fakeReservationAdapter{current: capacity - 300<<20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, capacity-300<<20, reservation)
	if err := m.balloon.SeedRestoredState(400<<20, 300<<20); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.StartRestore(ctx, 300<<20); err == nil {
		t.Fatal("canceled normalization unexpectedly completed")
	}
	defer m.Stop()

	m.controlMu.Lock()
	err := m.processPressureLocked(context.Background(), pressureGrow{urgency: "high", reason: "oom"})
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if calls := fakeCH.calls(); len(calls) != 0 {
		t.Fatalf("pressure crossed restore normalization barrier: %v", calls)
	}
	if calls := reservation.seen(); len(calls) != 0 {
		t.Fatalf("pressure reserved during restore normalization: %+v", calls)
	}
}

func TestMemoryControllerRestoreUsesObservedBudgetAfterNormalization(t *testing.T) {
	const capacity = uint64(1 << 30)
	cgroup := newMemoryCgroup(t, "max", "1")
	fakeCH := newFakeCHMemory(t, capacity)
	fakeCH.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 400 << 20
		f.currentBudget = capacity - 400<<20
	})
	reservation := &fakeReservationAdapter{current: capacity - 300<<20}
	m := newMemoryControllerForTest(t, fakeCH, cgroup, capacity-300<<20, reservation)
	if err := m.balloon.SeedRestoredState(400<<20, 300<<20); err != nil {
		t.Fatal(err)
	}
	m.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartRestore(ctx, 300<<20); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	m.controlMu.Lock()
	err := m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 2, Seq: 1, MemTotalBytes: capacity, MemAvailableBytes: 256 << 20,
	})
	pending := m.initialObservationPending
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("post-normalization observation still waited for target equality")
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{300 << 20, 320 << 20}) {
		t.Fatalf("post-normalization bounded target calls: %v", calls)
	}
	if calls := reservation.seen(); !reflect.DeepEqual(calls, []reservationCall{{
		current: 704 << 20, reason: "shrink_commit",
	}}) {
		t.Fatalf("restore settlement did not cover target and actual: %+v", calls)
	}

	fakeCH.configure(func(f *fakeCHMemory) { f.currentBudget = capacity - 300<<20 })
	m.controlMu.Lock()
	err = m.processReportLocked(context.Background(), proto.MemReport{
		Epoch: 2, Seq: 2, MemTotalBytes: capacity, MemAvailableBytes: 0,
	})
	pending = m.initialObservationPending
	m.controlMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("valid restore report did not open steady policy")
	}
	if calls := fakeCH.calls(); !reflect.DeepEqual(calls, []uint64{300 << 20, 320 << 20, 0}) {
		t.Fatalf("post-observation restore grow calls = %v", calls)
	}
	if got := reservation.ReservationMemory(); got != capacity {
		t.Fatalf("post-observation restore reservation = %d, want %d", got, capacity)
	}
}

func TestMemoryControllerStopBeforeStartPreventsLateWorker(t *testing.T) {
	cfg := memoryTestConfig("")
	cfg.Resources.Allocatable.Memory = "1GiB"
	m, err := NewMemoryController(MemoryControllerOptions{
		Config: cfg, InitialBudget: 1 << 30, Interval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Stop()
	m.StartCold(context.Background())

	m.lifecycleMu.Lock()
	started, stopped := m.started, m.stopped
	m.lifecycleMu.Unlock()
	if started || !stopped {
		t.Fatalf("lifecycle after stop-before-start: started=%v stopped=%v", started, stopped)
	}
	if m.SubmitGuestReport(proto.MemReport{Epoch: 1, Seq: 1}) {
		t.Fatal("stopped controller opened its report barrier on a late StartCold")
	}
	if state := m.State(); state.Active {
		t.Fatal("stopped controller became active on a late StartCold")
	}
}

func TestMemoryControllerConcurrentStartStopAlwaysJoinsWorker(t *testing.T) {
	for i := 0; i < 100; i++ {
		cfg := memoryTestConfig("")
		cfg.Resources.Allocatable.Memory = "1GiB"
		m, err := NewMemoryController(MemoryControllerOptions{
			Config: cfg, InitialBudget: 1 << 30, Interval: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.StartCold(context.Background())
		}()
		go func() {
			defer wg.Done()
			m.Stop()
		}()
		wg.Wait()

		m.lifecycleMu.Lock()
		started, stopped := m.started, m.stopped
		m.lifecycleMu.Unlock()
		if !stopped {
			t.Fatalf("iteration %d: controller was not stopped", i)
		}
		if started {
			select {
			case <-m.done:
			default:
				t.Fatalf("iteration %d: Stop returned before the worker exited", i)
			}
		}
	}
}

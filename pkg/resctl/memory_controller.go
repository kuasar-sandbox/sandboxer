package resctl

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
	"golang.org/x/sys/unix"
)

const (
	defaultMemoryReconcileInterval = 250 * time.Millisecond
	memoryHighLockRetryInterval    = 10 * time.Millisecond
)

type memoryTransactionKind uint8

const (
	memoryTransactionGrow memoryTransactionKind = iota + 1
	memoryTransactionShrink
)

type memoryTransaction struct {
	kind memoryTransactionKind
	// objectiveBudget is the monotonic final grow objective. targetBudget is
	// the currently prepared/applied representable sub-target. Keeping the two
	// separate lets partial node grants accumulate without either rounding up
	// unreserved guest memory or forgetting the original request.
	objectiveBudget    uint64
	targetBudget       uint64
	target             uint64
	demand             uint64
	urgency            string
	reason             string
	reportSeq          uint64
	targetChange       bool
	reservationRetryAt time.Time

	resizeAccepted bool
	highApplied    bool
	// Shrink keeps its target intent separate from the Budget already observed
	// and applied to high. The decision sample is rechecked before an inflate.
	shrinkObservation BalloonState
	highBudget        uint64
}

type pressureGrow struct {
	urgency string
	reason  string
}

type reservationAdapter interface {
	Enabled() bool
	ReservationMemory() uint64
	RequestBudget(currentAlloc, requestedDelta uint64, urgency, reason string) (uint64, uint64, time.Duration, error)
}

// MemoryControllerOptions assembles the sandbox-local Budget loop. InitialBudget
// is the exact cold admission Budget or restore BudgetAtSnapshot already
// reserved before CH starts.
type MemoryControllerOptions struct {
	Config        *config.SandboxConfig
	CgroupPath    string
	Balloon       *BalloonController
	Reservation   *ControllerHooks
	InitialBudget uint64
	Interval      time.Duration
	Logf          func(string, ...any)
}

// MemoryControllerState is diagnostic sandbox-local state. No field is sent
// through the node reservation protocol.
type MemoryControllerState struct {
	Active               bool
	RestoreNormalization bool
	Capacity             uint64
	Headroom             uint64
	Reservation          uint64
	DemandMemory         uint64
	RequestedBudget      uint64
	TargetBudget         uint64
	CurrentBudget        uint64
	ObservedBudget       uint64
	BalloonTarget        uint64
	BalloonCurrent       uint64
	ReportEpoch          uint64
	ReportSeq            uint64
	GuestMemAvailable    uint64
	GuestMemFree         uint64
	GuestCached          uint64
	GuestAnonPages       uint64
	GuestSReclaimable    uint64
	HostMemoryHigh       uint64
}

// MemoryController owns guest observation, CH balloon, memory.high, and their
// ordering. Dynamic mode adds ControllerHooks only as a reservation adapter;
// the local formula and executor are otherwise identical to static mode.
type MemoryController struct {
	cfg         *config.SandboxConfig
	cgroupPath  string
	balloon     *BalloonController
	reservation reservationAdapter
	capacity    uint64
	headroom    uint64
	overhead    uint64
	ratio       uint64
	interval    time.Duration
	logf        func(string, ...any)

	controlMu sync.Mutex
	txn       *memoryTransaction
	state     MemoryControllerState

	normalizationPending bool
	normalizationTarget  uint64
	// initialObservationPending requires a fresh post-lifecycle report and a
	// trustworthy CH observation, not target/current equality. Reservation
	// coverage and each candidate's actual-relative bound are checked separately.
	initialObservationPending bool
	staticReservation         uint64
	lastDemand                uint64
	lastDemandKnown           bool
	shrinkReportFence         uint64

	reportMu       sync.Mutex
	reportsOpen    bool
	reportEpoch    uint64
	reportSeq      uint64
	lastReport     proto.MemReport
	reportEvents   chan proto.MemReport
	pressureEvents chan pressureGrow
	// localMutationGate provides the same snapshot/high serialization when the
	// full-Capacity configuration legitimately has no balloon device. With a
	// balloon, its gate remains the single gate covering high and resize.
	localMutationGate chan struct{}

	lifecycleMu sync.Mutex
	started     bool
	stopped     bool
	cancel      context.CancelFunc
	done        chan struct{}
}

func NewMemoryController(opts MemoryControllerOptions) (*MemoryController, error) {
	if opts.Config == nil {
		return nil, errors.New("memory controller requires config")
	}
	capacity, err := opts.Config.CapacityMemoryBytes()
	if err != nil {
		return nil, err
	}
	headroom, err := opts.Config.AllocatableMemoryBytes()
	if err != nil {
		return nil, err
	}
	overhead, err := opts.Config.OverheadMemoryBytes()
	if err != nil {
		return nil, err
	}
	ratio, err := opts.Config.WatermarkHighRatio()
	if err != nil {
		return nil, err
	}
	if opts.InitialBudget == 0 || opts.InitialBudget > capacity {
		return nil, fmt.Errorf("initial Budget %d is outside (0, %d]", opts.InitialBudget, capacity)
	}
	if opts.Balloon == nil && opts.InitialBudget != capacity {
		return nil, fmt.Errorf("initial Budget %d below Capacity %d requires a balloon device", opts.InitialBudget, capacity)
	}
	if opts.Balloon != nil && opts.Balloon.Capacity != capacity {
		return nil, fmt.Errorf("balloon Capacity %d differs from config Capacity %d", opts.Balloon.Capacity, capacity)
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultMemoryReconcileInterval
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	m := &MemoryController{
		cfg: opts.Config, cgroupPath: opts.CgroupPath, balloon: opts.Balloon,
		reservation: opts.Reservation, capacity: capacity, headroom: headroom,
		overhead: overhead, ratio: ratio, interval: opts.Interval, logf: opts.Logf,
		staticReservation: opts.InitialBudget,
		reportEvents:      make(chan proto.MemReport, 16),
		pressureEvents:    make(chan pressureGrow, 16),
		localMutationGate: make(chan struct{}, 1),
		done:              make(chan struct{}),
	}
	m.localMutationGate <- struct{}{}
	m.state = MemoryControllerState{
		Capacity: capacity, Headroom: headroom, Reservation: opts.InitialBudget,
		TargetBudget: opts.InitialBudget,
	}
	if opts.Balloon == nil {
		// No device means both balloon sides are exactly zero and the guest
		// Budget is the full memory domain, even before the first report.
		m.state.TargetBudget = capacity
		m.state.CurrentBudget = capacity
		m.state.ObservedBudget = capacity
	} else {
		balloonState := opts.Balloon.State()
		m.state.BalloonTarget = balloonState.AcceptedTarget
		m.state.BalloonCurrent = balloonState.BalloonCurrent
		if balloonState.AcceptedTargetKnown {
			m.state.TargetBudget = BudgetFromTarget(capacity, balloonState.AcceptedTarget)
		}
		if balloonState.BalloonCurrentKnown {
			m.state.CurrentBudget = balloonState.CurrentBudget
		}
		if observed, ok := balloonState.ObservedBudget(capacity); ok {
			m.state.ObservedBudget = observed
		}
	}
	if opts.Reservation != nil && opts.Reservation.Enabled() {
		if got := opts.Reservation.ReservationMemory(); got != opts.InitialBudget {
			return nil, fmt.Errorf("initial node reservation=%d, local initial Budget=%d", got, opts.InitialBudget)
		}
	}
	return m, nil
}

// StartCold opens the launch-ACK observation barrier and starts the local
// worker. No report sampled before this call can influence policy.
func (m *MemoryController) StartCold(ctx context.Context) {
	workerCtx, start := m.claimStart(ctx)
	if !start {
		return
	}
	m.controlMu.Lock()
	m.state.Active = true
	m.initialObservationPending = true
	m.controlMu.Unlock()
	m.openReportBarrier()
	go m.loop(workerCtx)
}

// StartRestore starts after restore ACK and MUX establishment. SafeTarget
// normalization is attempted before reports are accepted. A failure is
// returned for diagnostics while the worker retains and retries the same
// target; no steady policy runs until confirmation succeeds.
func (m *MemoryController) StartRestore(ctx context.Context, safeTarget uint64) error {
	if m.balloon == nil {
		if safeTarget != 0 {
			return fmt.Errorf("restore SafeTarget=%d without balloon device", safeTarget)
		}
	} else if err := ValidateBalloonSize(m.capacity, safeTarget); err != nil {
		return err
	}
	workerCtx, start := m.claimStart(ctx)
	if !start {
		return nil
	}
	m.controlMu.Lock()
	m.state.Active = true
	m.state.RestoreNormalization = true
	m.normalizationPending = true
	m.normalizationTarget = safeTarget
	m.initialObservationPending = true
	err := m.advanceNormalizationLocked(workerCtx)
	m.controlMu.Unlock()
	go m.loop(workerCtx)
	return err
}

func (m *MemoryController) claimStart(ctx context.Context) (context.Context, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.started || m.stopped {
		return nil, false
	}
	workerCtx, cancel := context.WithCancel(ctx)
	m.started = true
	m.cancel = cancel
	return workerCtx, true
}

func (m *MemoryController) Stop() {
	if m == nil {
		return
	}
	m.lifecycleMu.Lock()
	m.stopped = true
	started, cancel := m.started, m.cancel
	m.lifecycleMu.Unlock()
	if !started {
		return
	}
	if cancel != nil {
		cancel()
	}
	<-m.done
}

// SubmitGuestReport performs only barrier/epoch/seq validation and queues the
// observation. Its result says whether the launch server may ACK the report:
// false is reserved for a closed observation barrier, so the guest retries the
// same epoch/seq. Once the barrier is open, duplicate, stale, and invalid
// reports are ignored for policy but return true so a lost ACK cannot wedge the
// guest stream. The method never performs CH or cgroup I/O.
func (m *MemoryController) SubmitGuestReport(report proto.MemReport) bool {
	if m == nil {
		return true
	}
	m.reportMu.Lock()
	defer m.reportMu.Unlock()
	if !m.reportsOpen {
		m.logf("memory: held report behind launch/restore observation barrier epoch=%d seq=%d", report.Epoch, report.Seq)
		return false
	}
	if report.Epoch == 0 || report.Seq == 0 {
		m.logf("memory: ignored invalid report epoch=%d seq=%d", report.Epoch, report.Seq)
		return true
	}
	if m.reportEpoch == 0 {
		m.reportEpoch = report.Epoch
	} else if report.Epoch != m.reportEpoch {
		m.logf("memory: rejected report epoch=%d seq=%d after epoch=%d seq=%d", report.Epoch, report.Seq, m.reportEpoch, m.reportSeq)
		return true
	}
	if report.Seq <= m.reportSeq {
		m.logf("memory: rejected duplicate/stale report epoch=%d seq=%d after seq=%d", report.Epoch, report.Seq, m.reportSeq)
		return true
	}
	m.reportSeq = report.Seq
	m.lastReport = report
	select {
	case m.reportEvents <- report:
	default:
		// A full queue may delay either policy direction until the next periodic
		// observation or pressure event. Keep the sequence consumed: queued
		// older reports then fail closed for shrink, and this observation cannot
		// be replayed later against newer CH state.
		m.logf("memory: report queue full; dropped epoch=%d seq=%d", report.Epoch, report.Seq)
	}
	return true
}

func (m *MemoryController) openReportBarrier() {
	m.reportMu.Lock()
	m.reportEpoch = 0
	m.reportSeq = 0
	m.lastReport = proto.MemReport{}
	m.reportsOpen = true
	m.reportMu.Unlock()
}

// RequestPressureGrow coalesces PSI/OOM into one MemoryStep reservation/grow
// request. It never writes a target directly from the sensor goroutine.
func (m *MemoryController) RequestPressureGrow(urgency, reason string) {
	if m == nil {
		return
	}
	select {
	case m.pressureEvents <- pressureGrow{urgency: urgency, reason: reason}:
	default:
		m.logf("memory: pressure queue full; coalescing urgency=%s reason=%s", urgency, reason)
	}
}

func (m *MemoryController) loop(ctx context.Context) {
	defer close(m.done)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case report := <-m.reportEvents:
			m.controlMu.Lock()
			if err := m.processReportLocked(ctx, report); err != nil {
				m.logf("memory: report epoch=%d seq=%d: %v", report.Epoch, report.Seq, err)
			}
			m.controlMu.Unlock()
		case pressure := <-m.pressureEvents:
			m.controlMu.Lock()
			if err := m.processPressureLocked(ctx, pressure); err != nil {
				m.logf("memory: pressure grow urgency=%s reason=%s: %v", pressure.urgency, pressure.reason, err)
			}
			m.controlMu.Unlock()
		case <-ticker.C:
			m.controlMu.Lock()
			if err := m.retryLocked(ctx); err != nil {
				m.logf("memory: reconcile: %v", err)
			}
			m.controlMu.Unlock()
		}
	}
}

func (m *MemoryController) retryLocked(ctx context.Context) error {
	if m.normalizationPending {
		return m.advanceNormalizationLocked(ctx)
	}
	if m.txn != nil {
		return m.advanceTransactionLocked(ctx)
	}
	return nil
}

func (m *MemoryController) advanceNormalizationLocked(ctx context.Context) error {
	if !m.normalizationPending {
		return nil
	}
	if m.balloon != nil {
		if err := m.balloon.SetDesiredTarget(m.normalizationTarget); err != nil {
			return err
		}
		release, err := m.balloon.acquireMutation(ctx)
		if err != nil {
			return err
		}
		state, applyErr := m.balloon.applyDesiredHeld(ctx)
		release()
		m.recordBalloonStateLocked(state)
		if applyErr != nil {
			return fmt.Errorf("restore SafeTarget normalization: %w", applyErr)
		}
	}
	m.normalizationPending = false
	m.state.RestoreNormalization = false
	m.openReportBarrier()
	m.logf("memory: restore SafeTarget normalization accepted target=%d", m.normalizationTarget)
	return nil
}

func (m *MemoryController) processReportLocked(ctx context.Context, report proto.MemReport) error {
	if m.normalizationPending {
		return nil
	}
	state, err := m.observeBalloon(ctx)
	if err != nil {
		return err
	}
	m.recordBalloonStateLocked(state)
	m.state.ReportEpoch = report.Epoch
	m.state.ReportSeq = report.Seq
	m.state.GuestMemAvailable = report.MemAvailableBytes
	m.state.GuestMemFree = report.MemFreeBytes
	m.state.GuestCached = report.CachedBytes
	m.state.GuestAnonPages = report.AnonPagesBytes
	m.state.GuestSReclaimable = report.SReclaimableBytes
	if _, known := state.ObservedBudget(m.capacity); !known {
		return errors.New("memory: incomplete CH target/current observation")
	}
	if m.initialObservationPending {
		m.initialObservationPending = false
		m.logf("memory: initial CH observation accepted epoch=%d seq=%d", report.Epoch, report.Seq)
	}
	budget, err := CalculateMemoryBudget(m.capacity, m.headroom, state.AcceptedTarget,
		state.CurrentBudget, report.MemAvailableBytes)
	if err != nil && !errors.Is(err, ErrMemoryOverflow) {
		return err
	}
	if errors.Is(err, ErrMemoryOverflow) {
		// saturating_add already selected full Capacity. Continue with that
		// conservative result so an arithmetic edge cannot suppress a grow.
		m.logf("memory: demand + headroom saturated; requesting full Capacity")
	}
	m.lastDemand, m.lastDemandKnown = budget.DemandMemory, true
	m.state.DemandMemory = budget.DemandMemory
	m.state.RequestedBudget = budget.RequestedBudget
	m.state.TargetBudget = budget.TargetBudget
	m.state.CurrentBudget = budget.CurrentBudget
	m.state.ObservedBudget = budget.ObservedBudget
	m.state.Reservation = m.reservationNow()

	// A newer valid observation may replace a pending shrink with a safety
	// grow, or validate completion of that same shrink. Until it is processed,
	// advanceShrinkLocked keeps the old high and reservation by comparing the
	// transaction sequence with the last sequence accepted by the report
	// barrier.
	if m.txn != nil && m.txn.kind == memoryTransactionShrink {
		txn := m.txn
		if budget.RequestedBudget > txn.targetBudget {
			// A previous partial settlement may already have returned reservation.
			// Grow always uses reservationNow(), never the pre-shrink baseline.
			m.txn = nil
			return m.startGrowLocked(ctx, budget.RequestedBudget, budget.DemandMemory,
				resource.UrgencyNormal, "guest_report")
		}
		if budget.ObservedBudget > m.reservationNow() {
			// This fresh observation also invalidates an unissued candidate.
			// Do not refresh its decision and inflate against uncovered actual.
			if m.balloon != nil {
				if err := m.balloon.SetDesiredTarget(state.AcceptedTarget); err != nil {
					return err
				}
			}
			m.logf("memory: observed Budget=%d exceeds reservation=%d; pending shrink discarded",
				budget.ObservedBudget, m.reservationNow())
			m.finishShrinkLocked(txn)
			return nil
		}
		// A larger demand produces a no-lower memory.high. Keep the most
		// conservative demand observed by reports that still support this
		// target, including observations sampled while actual converged.
		if budget.DemandMemory > txn.demand {
			txn.demand = budget.DemandMemory
			// The previous high may already have been written while a node
			// shrink commit was retrying. Recompute it from the newer demand
			// before releasing reservation; this is a forward safety update,
			// not a target/Budget rollback.
			if txn.highApplied {
				txn.highApplied = false
			}
		}
		txn.reportSeq = report.Seq
		if !txn.resizeAccepted {
			// Only a new guest report can refresh an unexecuted decision. A
			// ticker retry retains its old sample and cannot erase a reversal.
			txn.shrinkObservation = state
		}
		if err := m.advanceTransactionLocked(ctx); err != nil {
			m.logf("memory: pending shrink transaction: %v", err)
		}
		return nil
	}
	if m.txn != nil && m.txn.kind == memoryTransactionGrow {
		txn := m.txn
		if budget.RequestedBudget > txn.growObjective() {
			txn.objectiveBudget = budget.RequestedBudget
		}
		txn.upgradeGrowCause(resource.UrgencyNormal, "guest_report")
		if budget.DemandMemory > txn.demand {
			txn.demand = budget.DemandMemory
			// If high was written but the prepared resize was not yet
			// confirmed, recompute the safety value from the newer, larger
			// demand before retrying that deflate.
			if txn.targetChange && txn.highApplied {
				txn.highApplied = false
			}
		}
		return m.advanceTransactionLocked(ctx)
	}

	// Grow is safety-prioritized and does not require target/current equality.
	// It may replace an in-flight shrink. A pending grow was handled above, so
	// a smaller report can never reverse its retained objective.
	if budget.RequestedBudget > budget.TargetBudget {
		return m.startGrowLocked(ctx, budget.RequestedBudget, budget.DemandMemory, resource.UrgencyNormal, "guest_report")
	}
	// A report that was monotonic when accepted may sit behind newer reports
	// while CH/cgroup I/O is in progress. It remains useful for a conservative
	// grow above, but must not authorize shrink, memory.high reduction, or node
	// release once a newer observation is already pending.
	m.reportMu.Lock()
	latestEpoch, latestSeq := m.reportEpoch, m.reportSeq
	m.reportMu.Unlock()
	if latestEpoch == report.Epoch && latestSeq > report.Seq {
		m.logf("memory: report epoch=%d seq=%d superseded by seq=%d; shrink and high reduction skipped",
			report.Epoch, report.Seq, latestSeq)
		return nil
	}
	if budget.ObservedBudget > m.reservationNow() {
		// Cold/emergency actual is not a node grant. Keep the existing high;
		// normal demand/pressure grow remains the only way to obtain more.
		m.logf("memory: observed Budget=%d exceeds reservation=%d; settlement and new inflate deferred",
			budget.ObservedBudget, m.reservationNow())
		return nil
	}

	nextTarget, shrink := NextShrinkTarget(m.capacity, state.AcceptedTarget,
		state.CurrentBudget, budget.RequestedBudget)
	if shrink && report.Seq > m.shrinkReportFence {
		m.txn = &memoryTransaction{
			kind: memoryTransactionShrink, target: nextTarget,
			targetBudget: BudgetFromTarget(m.capacity, nextTarget), demand: budget.DemandMemory,
			reportSeq: report.Seq, targetChange: true, shrinkObservation: state,
		}
		return m.advanceTransactionLocked(ctx)
	}

	// No new target is needed/representable. A confirmed partial observation
	// can still settle high and reservation; it need not reach the old target.
	targetBudget := budget.TargetBudget
	m.txn = &memoryTransaction{
		kind: memoryTransactionShrink, target: state.AcceptedTarget,
		targetBudget: targetBudget, demand: budget.DemandMemory, resizeAccepted: true,
		reportSeq: report.Seq,
	}
	return m.advanceTransactionLocked(ctx)
}

func (m *MemoryController) processPressureLocked(ctx context.Context, pressure pressureGrow) error {
	// Restore normalization is the sole allowed target operation between
	// restore ACK/MUX establishment and confirmation of SafeTarget. Sensor
	// inputs are transient and will be sampled again after that barrier. Once
	// normalization (or cold launch ACK) completes, pressure grow is safe even
	// before target/current reach equality because it reserves before
	// raising high and deflating.
	if m.normalizationPending {
		return nil
	}
	currentBudget := m.state.TargetBudget
	if m.balloon != nil {
		state := m.balloon.State()
		if state.AcceptedTargetKnown {
			currentBudget = BudgetFromTarget(m.capacity, state.AcceptedTarget)
		}
	}
	// A pressure signal is monotonic grow intent. If a larger grow is already
	// retained for forward retry, extend that target rather than replacing it
	// with one step above CH's still-old accepted target.
	if m.txn != nil && m.txn.kind == memoryTransactionGrow && m.txn.growObjective() > currentBudget {
		currentBudget = m.txn.growObjective()
	}
	want, overflow := bits.Add64(currentBudget, resource.MemoryStep, 0)
	if overflow != 0 || want > m.capacity {
		want = m.capacity
	}
	if want <= currentBudget {
		return nil
	}
	demand := want
	if m.lastDemandKnown {
		demand = m.lastDemand
	}
	return m.startGrowLocked(ctx, want, demand, pressure.urgency, pressure.reason)
}

func (m *MemoryController) startGrowLocked(ctx context.Context, requestedBudget, demand uint64, urgency, reason string) error {
	if requestedBudget > m.capacity {
		requestedBudget = m.capacity
	}
	// Reservation/high/resize failures retain a monotonic forward objective.
	// A later observation may extend that objective, but it cannot turn the
	// retained grow into an implicit rollback. Once the grow is accepted the
	// transaction clears, and a subsequent fresh report may shrink it within
	// the normal target- and actual-relative step bounds.
	if m.txn != nil && m.txn.kind == memoryTransactionGrow {
		if objective := m.txn.growObjective(); objective > requestedBudget {
			requestedBudget = objective
		}
		if m.txn.demand > demand {
			demand = m.txn.demand
		}
		m.txn.objectiveBudget = requestedBudget
		m.txn.demand = demand
		m.txn.upgradeGrowCause(urgency, reason)
		return m.advanceTransactionLocked(ctx)
	}
	currentTargetBudget := m.state.TargetBudget
	if m.balloon != nil {
		state := m.balloon.State()
		if state.AcceptedTargetKnown {
			currentTargetBudget = BudgetFromTarget(m.capacity, state.AcceptedTarget)
		}
	}
	if requestedBudget <= currentTargetBudget {
		return nil
	}
	m.txn = &memoryTransaction{
		kind: memoryTransactionGrow, objectiveBudget: requestedBudget,
		demand: demand, urgency: urgency, reason: reason,
	}
	return m.advanceTransactionLocked(ctx)
}

func (m *MemoryController) advanceTransactionLocked(ctx context.Context) error {
	if m.txn == nil {
		return nil
	}
	if m.txn.kind == memoryTransactionGrow {
		return m.advanceGrowLocked(ctx)
	}
	return m.advanceShrinkLocked(ctx)
}

func (m *MemoryController) advanceGrowLocked(ctx context.Context) error {
	txn := m.txn
	objective := txn.growObjective()
	if objective == 0 || objective > m.capacity {
		return fmt.Errorf("grow objective %d is outside (0, %d]", objective, m.capacity)
	}

	// Complete an already prepared high -> resize step before asking for more
	// reservation. In particular, an ambiguous resize keeps targetChange set so
	// vm.info confirmation is retried rather than treating the optimistic local
	// accepted-target cache as transaction completion.
	if txn.targetChange {
		return m.applyPreparedGrowLocked(ctx, txn, objective)
	}

	currentTargetBudget := m.state.TargetBudget
	if m.balloon != nil {
		state := m.balloon.State()
		if state.AcceptedTargetKnown {
			currentTargetBudget = BudgetFromTarget(m.capacity, state.AcceptedTarget)
		}
	}
	if currentTargetBudget >= objective {
		m.txn = nil
		return nil
	}

	reservation := m.reservationNow()
	if objective > reservation {
		if !txn.reservationRetryAt.IsZero() && time.Now().Before(txn.reservationRetryAt) {
			return nil
		}
		delta, _ := saturatingSub(objective, reservation)
		urgency := txn.urgency
		if urgency == "" {
			urgency = resource.UrgencyNormal
		}
		granted, newReservation, cooldown, err := m.requestReservation(reservation, delta, urgency, txn.reason)
		if err != nil {
			return err
		}
		if cooldown > 0 {
			txn.reservationRetryAt = time.Now().Add(cooldown)
		} else {
			txn.reservationRetryAt = time.Time{}
		}
		reservation = newReservation
		if granted == 0 {
			return nil
		}
	}
	if reservation > objective {
		reservation = objective
	}
	// Existing grants may be partial and need not be step aligned. Never round
	// them up into unreserved guest Budget; retain the objective and retry until
	// another representable sub-target is available.
	executableBudget := alignedBudgetAtMostReservation(m.capacity, reservation)
	if executableBudget <= currentTargetBudget {
		return nil
	}
	txn.targetBudget = executableBudget
	txn.target = TargetForBudget(m.capacity, executableBudget)
	txn.targetChange = true
	txn.highApplied = false
	return m.applyPreparedGrowLocked(ctx, txn, objective)
}

func (m *MemoryController) applyPreparedGrowLocked(ctx context.Context, txn *memoryTransaction, objective uint64) error {
	release, err := m.acquireMutation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if !txn.highApplied {
		if err := m.applyMemoryHigh(ctx, txn.targetBudget, txn.demand, true); err != nil {
			return err
		}
		txn.highApplied = true
	}
	if m.balloon != nil {
		if err := m.balloon.SetDesiredTarget(txn.target); err != nil {
			return err
		}
		state, err := m.balloon.applyDesiredHeld(ctx)
		m.recordBalloonStateLocked(state)
		if err != nil {
			return err
		}
	} else if txn.target != 0 {
		return fmt.Errorf("grow target=%d without balloon device", txn.target)
	}
	m.logf("memory: grow accepted Budget=%d target=%d reservation=%d", txn.targetBudget, txn.target, m.reservationNow())
	txn.targetChange = false
	if txn.targetBudget >= objective {
		m.txn = nil
	}
	return nil
}

func (t *memoryTransaction) growObjective() uint64 {
	if t == nil {
		return 0
	}
	if t.objectiveBudget != 0 {
		return t.objectiveBudget
	}
	return t.targetBudget
}

func (t *memoryTransaction) upgradeGrowCause(urgency, reason string) {
	if t == nil {
		return
	}
	if t.urgency == "" || urgency == resource.UrgencyHigh ||
		(t.urgency == resource.UrgencyLow && urgency == resource.UrgencyNormal) {
		if t.urgency != urgency {
			// An OOM/high escalation must not remain parked behind a cooldown
			// returned for an earlier low/normal request.
			t.reservationRetryAt = time.Time{}
		}
		t.urgency = urgency
		if reason != "" {
			t.reason = reason
		}
	}
}

func (m *MemoryController) advanceShrinkLocked(ctx context.Context) error {
	txn := m.txn
	if m.shrinkReportSuperseded(txn.reportSeq) {
		return nil
	}
	release, err := m.acquireMutation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if m.shrinkReportSuperseded(txn.reportSeq) {
		return nil
	}

	var state BalloonState
	if !txn.resizeAccepted {
		if m.balloon != nil {
			if err := m.balloon.SetDesiredTarget(txn.target); err != nil {
				return err
			}
			state, err = m.balloon.applyShrinkDesiredHeld(ctx, txn.shrinkObservation)
		} else {
			state = m.syntheticBalloonState()
			if txn.target != 0 {
				err = fmt.Errorf("shrink target=%d without balloon device", txn.target)
			}
		}
		m.recordBalloonStateLocked(state)
		if errors.Is(err, errShrinkObservationChanged) {
			// CH confirmed that this candidate was not issued. Discard only
			// local intent; a fresh report must choose another bounded target.
			if resetErr := m.balloon.SetDesiredTarget(state.AcceptedTarget); resetErr != nil {
				return resetErr
			}
			m.logf("memory: discarded unexecuted shrink target=%d: %v", txn.target, err)
			m.finishShrinkLocked(txn)
			return nil
		}
		if err != nil {
			return err
		}
		txn.resizeAccepted = true
	} else {
		state, err = m.observeBalloon(ctx)
		if err != nil {
			return err
		}
		m.recordBalloonStateLocked(state)
		if txn.targetChange && (!state.AcceptedTargetKnown || state.AcceptedTarget != txn.target) {
			txn.resizeAccepted = false
			return fmt.Errorf("accepted balloon target=%d, retry desired=%d", state.AcceptedTarget, txn.target)
		}
	}
	budget, known := state.ObservedBudget(m.capacity)
	if !known || budget == 0 {
		return errors.New("memory: invalid shrink settlement observation")
	}
	reservation := m.reservationNow()
	if budget > reservation {
		// Autonomous deflate is not an authorization to raise high or to claim
		// a larger node reservation. A fresh demand/pressure event can grow.
		m.logf("memory: observed Budget=%d exceeds reservation=%d; settlement deferred", budget, reservation)
		m.finishShrinkLocked(txn)
		return nil
	}
	if m.shrinkReportSuperseded(txn.reportSeq) {
		return nil
	}
	if !txn.highApplied || txn.highBudget != budget {
		if err := m.applyMemoryHigh(ctx, budget, txn.demand, false); err != nil {
			return err
		}
		txn.highApplied, txn.highBudget = true, budget
	}
	if reservation > budget {
		// The high lifecycle lock may have delayed us. Recheck CH before
		// releasing: an increase or an unexpected target invalidates this
		// baseline. A decrease only makes this partial commit conservative.
		confirmed, err := m.observeBalloon(ctx)
		if err != nil {
			return err
		}
		m.recordBalloonStateLocked(confirmed)
		current, known := confirmed.ObservedBudget(m.capacity)
		if !known || confirmed.AcceptedTarget != state.AcceptedTarget || current > budget {
			txn.highApplied = false
			return fmt.Errorf("memory: shrink settlement observation changed; retaining reservation=%d", m.reservationNow())
		}
		if m.shrinkReportSuperseded(txn.reportSeq) {
			return nil
		}
		// CH and memory.high mutations are complete. Keep controlMu as the
		// transaction serializer, but do not make a node-only reservation RPC
		// part of the snapshot mutation barrier.
		release()
		_, newReservation, _, err := m.requestReservation(budget, 0, resource.UrgencyLow, "shrink_commit")
		if err != nil {
			return err
		}
		if newReservation != budget {
			return fmt.Errorf("shrink commit returned reservation=%d, want %d", newReservation, budget)
		}
		m.logf("memory: shrink settled Budget=%d target=%d current=%d", budget, state.AcceptedTarget, state.BalloonCurrent)
	}
	// This round's host operations are done. Remaining guest progress is
	// observed by later reports, not an unfinishable equality transaction.
	m.finishShrinkLocked(txn)
	return nil
}

func (m *MemoryController) shrinkReportSuperseded(seq uint64) bool {
	m.reportMu.Lock()
	defer m.reportMu.Unlock()
	return m.reportSeq > seq
}

func (m *MemoryController) finishShrinkLocked(txn *memoryTransaction) {
	if txn.targetChange {
		// Reports queued during this target operation cannot authorize another
		// inflate. Settlement completion is independent of target attainment.
		m.reportMu.Lock()
		floor := m.reportSeq
		m.reportMu.Unlock()
		if floor < txn.reportSeq {
			floor = txn.reportSeq
		}
		if floor > m.shrinkReportFence {
			m.shrinkReportFence = floor
		}
	}
	m.txn = nil
}

func (m *MemoryController) requestReservation(current, delta uint64, urgency, reason string) (uint64, uint64, time.Duration, error) {
	if m.reservation != nil && m.reservation.Enabled() {
		granted, next, cooldown, err := m.reservation.RequestBudget(current, delta, urgency, reason)
		if err == nil {
			m.state.Reservation = next
		}
		return granted, next, cooldown, err
	}
	next, carry := bits.Add64(current, delta, 0)
	if carry != 0 || next > m.capacity {
		return 0, m.staticReservation, 0, ErrMemoryOverflow
	}
	if current > m.staticReservation {
		return 0, m.staticReservation, 0, fmt.Errorf("current baseline %d exceeds static reservation %d", current, m.staticReservation)
	}
	// Existing excess reservation is reusable exactly like the node-side
	// absolute-baseline transaction.
	granted := delta
	m.staticReservation = next
	m.state.Reservation = next
	return granted, next, 0, nil
}

func (m *MemoryController) reservationNow() uint64 {
	if m.reservation != nil && m.reservation.Enabled() {
		return m.reservation.ReservationMemory()
	}
	return m.staticReservation
}

func (m *MemoryController) observeBalloon(ctx context.Context) (BalloonState, error) {
	if m.balloon == nil {
		return m.syntheticBalloonState(), nil
	}
	return m.balloon.Observe(ctx)
}

func (m *MemoryController) syntheticBalloonState() BalloonState {
	return BalloonState{
		DesiredTarget: 0, AcceptedTarget: 0, AcceptedTargetKnown: true,
		CurrentBudget: m.capacity, BalloonCurrent: 0, BalloonCurrentKnown: true,
	}
}

func (m *MemoryController) recordBalloonStateLocked(state BalloonState) {
	m.state.BalloonTarget = state.AcceptedTarget
	m.state.BalloonCurrent = state.BalloonCurrent
	if state.AcceptedTargetKnown {
		m.state.TargetBudget = BudgetFromTarget(m.capacity, state.AcceptedTarget)
	}
	if state.BalloonCurrentKnown {
		m.state.CurrentBudget = state.CurrentBudget
	}
	if observed, ok := state.ObservedBudget(m.capacity); ok {
		m.state.ObservedBudget = observed
	}
	m.state.Reservation = m.reservationNow()
}

func (m *MemoryController) acquireMutation(ctx context.Context) (func(), error) {
	if m.balloon != nil {
		return m.balloon.acquireMutation(ctx)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.localMutationGate:
		var once sync.Once
		return func() { once.Do(func() { m.localMutationGate <- struct{}{} }) }, nil
	}
}

// BeginSnapshot shares the mutation gate with local grow/shrink. The caller
// must acquire this before the existing memory.high lifecycle flock, preserving
// one lock order and preventing high/resize changes from crossing quiesce.
func (m *MemoryController) BeginSnapshot(ctx context.Context) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	return m.acquireMutation(ctx)
}

// CaptureState obtains the exact CH target/current pair for freeze diagnostics.
// Snapshot callers invoke it while holding BeginSnapshot's mutation barrier.
func (m *MemoryController) CaptureState(ctx context.Context) (MemoryControllerState, error) {
	if m == nil {
		return MemoryControllerState{}, nil
	}
	state, err := m.observeBalloon(ctx)
	if err != nil {
		return MemoryControllerState{}, err
	}
	m.reportMu.Lock()
	report := m.lastReport
	m.reportMu.Unlock()
	result := MemoryControllerState{
		Active: true, Capacity: m.capacity, Headroom: m.headroom,
		Reservation: m.reservationNow(), BalloonTarget: state.AcceptedTarget,
		BalloonCurrent: state.BalloonCurrent, CurrentBudget: state.CurrentBudget,
		ReportEpoch: report.Epoch, ReportSeq: report.Seq,
		GuestMemAvailable: report.MemAvailableBytes, GuestMemFree: report.MemFreeBytes,
		GuestCached: report.CachedBytes, GuestAnonPages: report.AnonPagesBytes,
		GuestSReclaimable: report.SReclaimableBytes,
	}
	result.TargetBudget = BudgetFromTarget(m.capacity, state.AcceptedTarget)
	if observed, ok := state.ObservedBudget(m.capacity); ok {
		result.ObservedBudget = observed
	}
	if report.Epoch != 0 {
		budget, calcErr := CalculateMemoryBudget(m.capacity, m.headroom, state.AcceptedTarget,
			state.CurrentBudget, report.MemAvailableBytes)
		if calcErr == nil || errors.Is(calcErr, ErrMemoryOverflow) {
			result.DemandMemory = budget.DemandMemory
			result.RequestedBudget = budget.RequestedBudget
		}
	}
	return result, nil
}

func (m *MemoryController) applyMemoryHigh(ctx context.Context, budget, demand uint64, grow bool) error {
	if m.cgroupPath == "" {
		return nil
	}
	lock, err := lockMemoryHigh(ctx, m.cgroupPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Read the safety floor only after acquiring the lifecycle flock. A
	// snapshot or ordered shutdown may hold that lock while the VMM charge is
	// still changing; a pre-lock sample could become stale before this write
	// and immediately self-throttle the resumed/settled VMM.
	hostCurrent, err := readHostMemoryCurrent(m.cgroupPath)
	if err != nil {
		return err
	}
	result, err := CalculateMemoryHigh(m.capacity, m.overhead, budget, demand, m.ratio, hostCurrent)
	if err != nil {
		return err
	}
	path := filepath.Join(m.cgroupPath, "memory.high")
	currentRaw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read memory.high: %w", err)
	}
	trimmed := strings.TrimSpace(string(currentRaw))
	if grow {
		if trimmed == "max" {
			m.state.HostMemoryHigh = result.MemoryMax
			return nil
		}
		current, parseErr := strconv.ParseUint(trimmed, 10, 64)
		if parseErr != nil {
			return fmt.Errorf("parse memory.high %q: %w", trimmed, parseErr)
		}
		if current >= result.HostMemoryHigh {
			m.state.HostMemoryHigh = current
			return nil
		}
	}
	want := strconv.FormatUint(result.HostMemoryHigh, 10)
	if trimmed != want {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			return fmt.Errorf("write memory.high: %w", err)
		}
	}
	m.state.HostMemoryHigh = result.HostMemoryHigh
	return nil
}

// readHostMemoryCurrent is strict because its result is a safety lower bound
// for a memory.high write. Controller heartbeat/settled diagnostics may use the
// best-effort readHostMemoryChargeBestEffort helper, but an unavailable or
// malformed charge must defer enforcement instead of being interpreted as zero.
func readHostMemoryCurrent(cgroupPath string) (uint64, error) {
	path := filepath.Join(cgroupPath, "memory.current")
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read memory.current: %w", err)
	}
	value := strings.TrimSpace(string(raw))
	current, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse memory.current %q: %w", value, err)
	}
	return current, nil
}

func lockMemoryHigh(ctx context.Context, cgroupPath string) (*os.File, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lock, err := os.Open(cgroupPath)
	if err != nil {
		return nil, fmt.Errorf("open memory.high lifecycle lock: %w", err)
	}
	fail := func(err error) (*os.File, error) {
		_ = lock.Close()
		return nil, err
	}
	retry := time.NewTicker(memoryHighLockRetryInterval)
	defer retry.Stop()
	for {
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return lock, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) {
			return fail(fmt.Errorf("lock memory.high lifecycle: %w", err))
		}
		select {
		case <-ctx.Done():
			return fail(fmt.Errorf("lock memory.high lifecycle: %w", ctx.Err()))
		case <-retry.C:
		}
	}
}

func (m *MemoryController) State() MemoryControllerState {
	if m == nil {
		return MemoryControllerState{}
	}
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	state := m.state
	state.Reservation = m.reservationNow()
	return state
}

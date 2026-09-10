package resctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandboxer/internal/chmemory"
)

var errShrinkObservationChanged = errors.New("balloon shrink decision changed")

// BalloonState keeps the local target intent, Cloud Hypervisor's accepted
// target, and the guest driver's observed current balloon size separate.
// A successful vm.resize advances only AcceptedTarget. CurrentBudget and
// BalloonCurrent advance only from vm.info.memory_actual_size.
type BalloonState struct {
	DesiredTarget uint64

	AcceptedTarget      uint64
	AcceptedTargetKnown bool

	CurrentBudget       uint64
	BalloonCurrent      uint64
	BalloonCurrentKnown bool
}

// TargetReached describes one exact sample, not convergence or permission to
// reclaim/settle. Policy uses trusted observations and operation-specific bounds.
func (s BalloonState) TargetReached(capacity uint64) bool {
	return s.AcceptedTargetKnown && s.BalloonCurrentKnown &&
		BudgetFromTarget(capacity, s.AcceptedTarget) == s.CurrentBudget
}

// ObservedBudget is the safe local upper bound across accepted target and
// current balloon. The boolean is false until both sides have been observed.
func (s BalloonState) ObservedBudget(capacity uint64) (uint64, bool) {
	if !s.AcceptedTargetKnown || !s.BalloonCurrentKnown {
		return 0, false
	}
	targetBudget := BudgetFromTarget(capacity, s.AcceptedTarget)
	if s.CurrentBudget > targetBudget {
		return s.CurrentBudget, true
	}
	return targetBudget, true
}

type balloonObservation struct {
	AcceptedTarget uint64
	CurrentBudget  uint64
	BalloonCurrent uint64
}

// BalloonController is the sandbox-local, single writer for Cloud
// Hypervisor's balloon target. It contains no guest-demand policy and no node
// reservation logic; those belong to MemoryController.
type BalloonController struct {
	Capacity uint64
	Logf     func(string, ...any)

	client *http.Client

	stateMu sync.Mutex
	state   BalloonState

	// apiMu serializes vm.info and vm.resize exchanges. mutationGate extends
	// serialization across the local high -> resize transaction and is also
	// held by the snapshot lifecycle barrier.
	apiMu        sync.Mutex
	mutationGate chan struct{}
}

// NewBalloonController constructs a controller for one CH API socket.
// capacity must be the exact byte total of CH's memory zones. A zero timeout
// leaves request lifetime entirely to ctx.
func NewBalloonController(chSock string, capacity uint64, timeout time.Duration, logf func(string, ...any)) *BalloonController {
	dialer := net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", chSock)
		},
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b := &BalloonController{
		Capacity:     capacity,
		Logf:         logf,
		client:       &http.Client{Transport: transport, Timeout: timeout},
		mutationGate: make(chan struct{}, 1),
	}
	b.mutationGate <- struct{}{}
	return b
}

// SeedColdTarget records the exact target encoded on CH's command line. It
// does not infer BalloonCurrent; the first post-launch vm.info supplies that
// observation.
func (b *BalloonController) SeedColdTarget(target uint64) error {
	if err := ValidateBalloonSize(b.Capacity, target); err != nil {
		return err
	}
	b.stateMu.Lock()
	b.state = BalloonState{
		DesiredTarget: target, AcceptedTarget: target,
		AcceptedTargetKnown: true,
	}
	b.stateMu.Unlock()
	return nil
}

// SeedRestoredState records the two balloon values captured in CH snapshot
// state. It performs no resize; restore normalization is an explicit operation
// after restore ACK and MUX establishment.
func (b *BalloonController) SeedRestoredState(snapshotTarget, snapshotCurrent uint64) error {
	if err := ValidateBalloonSize(b.Capacity, snapshotTarget); err != nil {
		return fmt.Errorf("snapshot balloon target: %w", err)
	}
	if err := ValidateBalloonSize(b.Capacity, snapshotCurrent); err != nil {
		return fmt.Errorf("snapshot balloon current: %w", err)
	}
	currentBudget := BudgetFromTarget(b.Capacity, snapshotCurrent)
	b.stateMu.Lock()
	b.state = BalloonState{
		DesiredTarget:  snapshotTarget,
		AcceptedTarget: snapshotTarget, AcceptedTargetKnown: true,
		CurrentBudget: currentBudget, BalloonCurrent: snapshotCurrent,
		BalloonCurrentKnown: true,
	}
	b.stateMu.Unlock()
	return nil
}

// State returns one consistent local snapshot.
func (b *BalloonController) State() BalloonState {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.state
}

// SetDesiredTarget updates local intent without issuing an HTTP request. A
// failed apply never rolls this value back; later reconciliation continues
// toward it.
func (b *BalloonController) SetDesiredTarget(target uint64) error {
	if err := ValidateBalloonSize(b.Capacity, target); err != nil {
		return err
	}
	b.stateMu.Lock()
	b.state.DesiredTarget = target
	b.stateMu.Unlock()
	return nil
}

// Observe refreshes accepted target and current balloon from one vm.info
// response. Capacity is asserted against CH's exact memory total.
func (b *BalloonController) Observe(ctx context.Context) (BalloonState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.apiMu.Lock()
	defer b.apiMu.Unlock()
	observation, err := b.readMemoryObservation(ctx)
	if err != nil {
		return b.State(), err
	}
	return b.commitObservation(observation), nil
}

// ApplyTarget records target as desired and reconciles CH toward it. It
// returns after vm.info confirms the accepted target; it deliberately does not
// wait for memory_actual_size to converge. Failures retain target as desired
// and never issue a compensating resize to an older value.
func (b *BalloonController) ApplyTarget(ctx context.Context, target uint64) (BalloonState, error) {
	if err := b.SetDesiredTarget(target); err != nil {
		return b.State(), err
	}
	release, err := b.acquireMutation(ctx)
	if err != nil {
		return b.State(), err
	}
	defer release()
	return b.applyDesiredHeld(ctx)
}

// applyDesiredHeld requires mutationGate to be held. MemoryController uses it
// to keep memory.high -> resize ordering inside the same snapshot barrier.
func (b *BalloonController) applyDesiredHeld(ctx context.Context) (BalloonState, error) {
	return b.applyDesiredHeldMode(ctx, nil)
}

// applyShrinkDesiredHeld rechecks the report's decision sample immediately
// before an inflate, under the existing mutation/API locks. A reversal of
// actual or a changed accepted target invalidates that sample; equality is
// neither required nor treated as evidence that another step is safe.
func (b *BalloonController) applyShrinkDesiredHeld(ctx context.Context, observed BalloonState) (BalloonState, error) {
	return b.applyDesiredHeldMode(ctx, &observed)
}

func (b *BalloonController) applyDesiredHeldMode(ctx context.Context, shrinkObservation *BalloonState) (BalloonState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.apiMu.Lock()
	defer b.apiMu.Unlock()

	desired := b.State().DesiredTarget
	if err := ValidateBalloonSize(b.Capacity, desired); err != nil {
		return b.State(), err
	}

	// First observe: an earlier response may have been lost even though CH
	// accepted the desired target. Confirmation completes that transaction
	// without another resize.
	pre, preErr := b.readMemoryObservation(ctx)
	if preErr == nil {
		state := b.commitObservation(pre)
		if state.AcceptedTarget == desired {
			return state, nil
		}
		if shrinkObservation != nil {
			observed := *shrinkObservation
			if !observed.AcceptedTargetKnown || !observed.BalloonCurrentKnown ||
				state.AcceptedTarget != observed.AcceptedTarget || state.CurrentBudget > observed.CurrentBudget {
				return state, fmt.Errorf("%w: decision observation changed (target/current=%d/%d)",
					errShrinkObservationChanged, state.AcceptedTarget, state.BalloonCurrent)
			}
			if desired <= state.AcceptedTarget || desired > shrinkTargetLimit(b.Capacity, state.AcceptedTarget, state.CurrentBudget) {
				return state, fmt.Errorf("%w: target=%d exceeds the accepted/actual step bound", errShrinkObservationChanged, desired)
			}
		}
	} else if shrinkObservation != nil {
		return b.State(), fmt.Errorf("balloon shrink deferred: pre-resize vm.info: %w", preErr)
	}

	resizeErr := b.callResize(ctx, desired)
	if resizeErr == nil {
		// HTTP success means CH accepted target, but not that the guest balloon
		// current converged. Record only the accepted side before confirmation.
		b.setAcceptedTarget(desired)
	}

	post, postErr := b.readMemoryObservation(ctx)
	if postErr == nil {
		state := b.commitObservation(post)
		if state.AcceptedTarget == desired {
			if resizeErr != nil {
				b.Logf("balloon: confirmed target=%d after lost/ambiguous resize response", desired)
			} else {
				b.Logf("balloon: CH accepted target=%d", desired)
			}
			return state, nil
		}
		mismatch := fmt.Errorf("vm.info balloon target=%d, want desired=%d", state.AcceptedTarget, desired)
		if resizeErr != nil {
			return state, errors.Join(resizeErr, mismatch)
		}
		return state, mismatch
	}

	state := b.State()
	if resizeErr != nil {
		if preErr != nil {
			return state, errors.Join(resizeErr, fmt.Errorf("pre-resize vm.info: %w", preErr), fmt.Errorf("post-resize vm.info: %w", postErr))
		}
		return state, errors.Join(resizeErr, fmt.Errorf("post-resize vm.info: %w", postErr))
	}
	return state, fmt.Errorf("vm.resize accepted target=%d but vm.info confirmation failed: %w", desired, postErr)
}

// BeginSnapshot blocks new local balloon mutations and waits for an in-flight
// high/resize critical section to finish. The returned release is idempotent;
// destroy-snapshot callers may retain it until CH exits.
func (b *BalloonController) BeginSnapshot(ctx context.Context) (func(), error) {
	return b.acquireMutation(ctx)
}

func (b *BalloonController) acquireMutation(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.mutationGate:
		var once sync.Once
		return func() { once.Do(func() { b.mutationGate <- struct{}{} }) }, nil
	}
}

func (b *BalloonController) setAcceptedTarget(target uint64) {
	b.stateMu.Lock()
	b.state.AcceptedTarget = target
	b.state.AcceptedTargetKnown = true
	b.stateMu.Unlock()
}

func (b *BalloonController) commitObservation(observation balloonObservation) BalloonState {
	b.stateMu.Lock()
	b.state.AcceptedTarget = observation.AcceptedTarget
	b.state.AcceptedTargetKnown = true
	b.state.CurrentBudget = observation.CurrentBudget
	b.state.BalloonCurrent = observation.BalloonCurrent
	b.state.BalloonCurrentKnown = true
	state := b.state
	b.stateMu.Unlock()
	return state
}

func (b *BalloonController) callResize(ctx context.Context, sizeBytes uint64) error {
	body, err := json.Marshal(struct {
		DesiredBalloon uint64 `json:"desired_balloon"`
	}{DesiredBalloon: sizeBytes})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://ch/api/v1/vm.resize", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vm.resize: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (b *BalloonController) readMemoryObservation(ctx context.Context) (balloonObservation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://ch/api/v1/vm.info", nil)
	if err != nil {
		return balloonObservation{}, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return balloonObservation{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return balloonObservation{}, fmt.Errorf("vm.info: HTTP %d", resp.StatusCode)
	}
	var info struct {
		MemoryActualSize *uint64 `json:"memory_actual_size"`
		Config           struct {
			Memory  *chmemory.Config `json:"memory"`
			Balloon *struct {
				Size *uint64 `json:"size"`
			} `json:"balloon"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return balloonObservation{}, fmt.Errorf("vm.info: decode: %w", err)
	}
	if info.Config.Memory == nil {
		return balloonObservation{}, errors.New("vm.info: config.memory missing")
	}
	if info.Config.Balloon == nil || info.Config.Balloon.Size == nil {
		return balloonObservation{}, errors.New("vm.info: config.balloon.size missing")
	}
	if info.MemoryActualSize == nil {
		return balloonObservation{}, errors.New("vm.info: memory_actual_size missing")
	}
	capacity, err := info.Config.Memory.TotalSize()
	if err != nil {
		return balloonObservation{}, fmt.Errorf("vm.info: config.memory: %w", err)
	}
	if capacity != b.Capacity {
		return balloonObservation{}, fmt.Errorf("vm.info: memory capacity=%d, resolved Capacity=%d", capacity, b.Capacity)
	}
	target := *info.Config.Balloon.Size
	if err := ValidateBalloonSize(capacity, target); err != nil {
		return balloonObservation{}, fmt.Errorf("vm.info target: %w", err)
	}
	currentBudget := *info.MemoryActualSize
	if currentBudget > capacity {
		return balloonObservation{}, fmt.Errorf("vm.info: memory_actual_size=%d exceeds Capacity=%d", currentBudget, capacity)
	}
	balloonCurrent, _ := saturatingSub(capacity, currentBudget)
	if err := ValidateBalloonSize(capacity, balloonCurrent); err != nil {
		return balloonObservation{}, fmt.Errorf("vm.info current: %w", err)
	}
	return balloonObservation{
		AcceptedTarget: target,
		CurrentBudget:  currentBudget,
		BalloonCurrent: balloonCurrent,
	}, nil
}

package resctl

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

// ControllerHookOptions identifies the existing sandbox/node reservation
// session. CgroupPath is used only for host memory.current diagnostics; all
// cgroup and balloon control remains sandbox-local outside ControllerHooks.
type ControllerHookOptions struct {
	SocketPath string
	CgroupPath string
	SandboxID  string
	Context    context.Context
	Logf       func(string, ...any)
}

// ControllerHooks is a reservation adapter. It deliberately knows nothing
// about guest reports, CH balloon state, memory.high, or lifecycle phases other
// than the existing Settled bit carried by StateSync.
type ControllerHooks struct {
	opts ControllerHookOptions
	cfg  *config.SandboxConfig

	// sessionMu covers one complete reservation RPC and connection
	// replacement. It prevents an old response from racing StateSync.
	sessionMu sync.Mutex
	mu        sync.Mutex
	client    *resource.Client
	lease     *resource.LeaseHandle

	controllerCgroupPath     string
	controllerSocketIdentity string

	admitted         bool
	settled          bool
	connected        bool
	reservationBytes uint64
	previousToken    string
	released         bool

	lifetimeCtx    context.Context
	cancelLifetime context.CancelFunc
	reconnectWake  chan struct{}
	reconnectWG    sync.WaitGroup

	cancelBg context.CancelFunc
	bgWG     sync.WaitGroup
}

func NewControllerHooks(opts ControllerHookOptions, cfg *config.SandboxConfig) (*ControllerHooks, error) {
	if cfg == nil {
		return nil, errors.New("controller hooks require config")
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	lifetimeCtx, cancel := context.WithCancel(opts.Context)
	h := &ControllerHooks{
		opts: opts, cfg: cfg, lifetimeCtx: lifetimeCtx, cancelLifetime: cancel,
		reconnectWake: make(chan struct{}, 1),
	}
	if opts.SocketPath == "" {
		return h, nil
	}
	if opts.SandboxID == "" {
		cancel()
		return nil, errors.New("dynamic resource mode requires sandbox id")
	}
	controllerSocketIdentity, err := resource.CanonicalSocketPath(opts.SocketPath)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("resolve controller socket: %w", err)
	}
	controllerSocketPath, err := filepath.Abs(opts.SocketPath)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("resolve controller dial path: %w", err)
	}
	h.opts.SocketPath = controllerSocketPath
	h.controllerSocketIdentity = controllerSocketIdentity
	h.controllerCgroupPath = filepath.Clean(cfg.Resources.Control.CgroupPath)

	capacity, err := cfg.CapacityMemoryBytes()
	if err != nil {
		cancel()
		return nil, err
	}
	headroom, err := cfg.AllocatableMemoryBytes()
	if err != nil {
		cancel()
		return nil, err
	}
	startupHeadroom, err := cfg.StartupBytes()
	if err != nil {
		cancel()
		return nil, err
	}
	lease, err := resource.CreateLease(resource.Lease{
		Version: resource.LeaseVersion, SandboxID: opts.SandboxID, PID: os.Getpid(),
		ControllerSocket: controllerSocketIdentity, CgroupPath: h.controllerCgroupPath,
		CapacityMemory: capacity, CapacityCPUMilli: uint64(cfg.Resources.Capacity.CPU) * 1000,
		FloorMemory: headroom, FloorCPUMilli: cpuMilliCeil(cfg.Resources.Allocatable.CPU),
		// The immutable lease mirrors configured startup headroom. Only the
		// existing Admit field carries its aligned InitialBudget representation.
		StartupMemory: startupHeadroom, ClientFeatures: []string{resource.FeatureStateSyncV1},
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("controller lease: %w", err)
	}
	h.lease = lease
	h.client = &resource.Client{SocketPath: controllerSocketPath}
	h.reconnectWG.Add(1)
	go h.reconnectLoop()
	return h, nil
}

func cpuMilliCeil(cpu float64) uint64 {
	if cpu <= 0 {
		return 0
	}
	return uint64(math.Ceil(cpu * 1000))
}

func (h *ControllerHooks) SetLocalCgroupPath(path string) {
	if h == nil {
		return
	}
	h.sessionMu.Lock()
	h.opts.CgroupPath = path
	h.sessionMu.Unlock()
}

func (h *ControllerHooks) Enabled() bool {
	return h != nil && h.opts.SocketPath != ""
}

func (h *ControllerHooks) Connected() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connected
}

// ReservationMemory returns the safe absolute reservation baseline used by
// StateSync. A grow response advances it before local high/resize work. A
// shrink request may lower it without a response because the local controller
// sends that request only after a confirmed safe observed Budget and high
// reduction; if the node did not commit the request, StateSync completes the
// release.
func (h *ControllerHooks) ReservationMemory() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reservationBytes
}

type localControllerState struct {
	admitted, settled, connected, released bool
	reservation                            uint64
	token                                  string
}

func (h *ControllerHooks) localState() localControllerState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return localControllerState{
		admitted: h.admitted, settled: h.settled, connected: h.connected,
		released: h.released, reservation: h.reservationBytes,
		token: h.previousToken,
	}
}

// Admit obtains the exact initial reservation. budgetAtSnapshot is zero
// for cold start and BudgetAtSnapshot for restore. Static mode returns the same
// locally resolved value without contacting a node.
func (h *ControllerHooks) Admit(sid string, budgetAtSnapshot uint64) (uint64, error) {
	capacity, err := h.cfg.CapacityMemoryBytes()
	if err != nil {
		return 0, err
	}
	headroom, err := h.cfg.AllocatableMemoryBytes()
	if err != nil {
		return 0, err
	}
	startupHeadroom, err := h.cfg.StartupBytes()
	if err != nil {
		return 0, err
	}
	startupBudget := AlignedBudget(capacity, startupHeadroom)
	initialBudget := startupBudget
	if budgetAtSnapshot != 0 {
		if budgetAtSnapshot > capacity {
			return 0, fmt.Errorf("BudgetAtSnapshot %d exceeds Capacity %d", budgetAtSnapshot, capacity)
		}
		initialBudget = budgetAtSnapshot
	}
	if !h.Enabled() {
		return initialBudget, nil
	}
	if sid != h.opts.SandboxID {
		return 0, fmt.Errorf("admit sandbox id %q does not match lease %q", sid, h.opts.SandboxID)
	}

	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	backoff := 100 * time.Millisecond
	for {
		select {
		case <-h.opts.Context.Done():
			return 0, h.opts.Context.Err()
		case <-h.lifetimeCtx.Done():
			return 0, errors.New("controller hooks released")
		default:
		}
		if !h.client.Connected() {
			if err := h.client.ConnectContext(h.lifetimeCtx); err != nil {
				if err := h.waitRetry(backoff); err != nil {
					return 0, err
				}
				backoff = nextBackoff(backoff)
				continue
			}
		}
		res, err := h.client.AdmitContext(h.lifetimeCtx, resource.AdmitParams{
			SandboxID: sid, CapacityMemoryBytes: capacity,
			CapacityCPU:      h.cfg.Resources.Capacity.CPU,
			FloorMemoryBytes: headroom, FloorCPU: h.cfg.Resources.Allocatable.CPU,
			StartupBudgetMemory: startupBudget, AllocatableAtSnapshot: budgetAtSnapshot,
			CgroupPath: h.controllerCgroupPath, ClientFeatures: []string{resource.FeatureStateSyncV1},
		})
		if resource.IsTransportError(err) {
			h.setConnected(false)
			if err := h.waitRetry(backoff); err != nil {
				return 0, err
			}
			backoff = nextBackoff(backoff)
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("admit: %w", err)
		}
		if res.Status == resource.StatusRejected {
			if res.Reason != "" {
				return 0, fmt.Errorf("admit rejected (%s): %s", res.Reason, res.Msg)
			}
			return 0, fmt.Errorf("admit rejected: %s", res.Msg)
		}
		if res.Status != resource.StatusAdmitted {
			return 0, fmt.Errorf("admit returned unexpected status %q", res.Status)
		}
		if res.GrantedInitialAlloc != initialBudget {
			return 0, fmt.Errorf("admit granted partial/incorrect initial reservation %d, require exactly %d", res.GrantedInitialAlloc, initialBudget)
		}
		h.mu.Lock()
		h.admitted, h.connected = true, true
		h.reservationBytes = initialBudget
		h.previousToken = res.Token
		h.mu.Unlock()
		if res.QueuedForMs > 0 {
			h.opts.Logf("controller admit: token=%s initial_reservation=%d (queued %dms, pos %d at entry)",
				shortToken(res.Token), initialBudget, res.QueuedForMs, res.QueuePosAtIn)
		} else {
			h.opts.Logf("controller admit: token=%s initial_reservation=%d", shortToken(res.Token), initialBudget)
		}
		return initialBudget, nil
	}
}

func shortToken(token string) string {
	if len(token) > 8 {
		return token[:8]
	}
	return token
}

// Settled publishes only the lifecycle fact. It never derives or changes a
// reservation from host memory.current.
func (h *ControllerHooks) Settled() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	h.settled = true
	h.mu.Unlock()
	if !h.Enabled() {
		return nil
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	state := h.localState()
	if !state.connected {
		h.notifyReconnect()
		return nil
	}
	hostCharge := readHostMemoryChargeBestEffort(h.opts.CgroupPath)
	if err := h.client.SettledContext(h.lifetimeCtx, hostCharge, 0); err != nil {
		h.markDisconnectedLocked(err)
		if resource.IsTransportError(err) {
			return nil
		}
		return fmt.Errorf("controller.Settled: %w", err)
	}
	return nil
}

// RequestBudget is the existing reservation transaction. currentAlloc is the
// sandbox's safe absolute baseline; requestedDelta may be zero to commit a
// shrink. The returned NewAllocatable is reservation state, never a balloon or
// cgroup command.
func (h *ControllerHooks) RequestBudget(currentAlloc, requestedDelta uint64, urgency, reason string) (uint64, uint64, time.Duration, error) {
	if !h.Enabled() {
		result, overflow := bits.Add64(currentAlloc, requestedDelta, 0)
		if overflow != 0 {
			return 0, currentAlloc, 0, ErrMemoryOverflow
		}
		return requestedDelta, result, 0, nil
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	state := h.localState()
	if !state.connected {
		h.notifyReconnect()
		return 0, state.reservation, 0, &resource.TransportError{Err: errors.New("controller session disconnected")}
	}
	granted, newReservation, cooldownMs, err := h.client.RequestBudgetContext(
		h.lifetimeCtx, currentAlloc, requestedDelta, urgency, reason)
	if err != nil {
		baseline := state.reservation
		// A shrink request is sent only after the accepted target/current pair
		// establishes a safe observed Budget and memory.high was lowered. If
		// its response is lost, CurrentAlloc
		// is therefore the only reusable local baseline that is safe whether
		// the node committed the request or not. A lost grow response keeps the
		// old smaller baseline because no local grow has been applied yet.
		if requestedDelta == 0 && currentAlloc < state.reservation {
			h.mu.Lock()
			h.reservationBytes = currentAlloc
			h.mu.Unlock()
			baseline = currentAlloc
		}
		h.markDisconnectedLocked(err)
		return 0, baseline, 0, err
	}
	capacity, err := h.cfg.CapacityMemoryBytes()
	if err != nil {
		return 0, state.reservation, 0, err
	}
	want, overflow := bits.Add64(currentAlloc, granted, 0)
	if overflow != 0 || granted > requestedDelta || newReservation != want || newReservation > capacity {
		protocolErr := fmt.Errorf("invalid budget response: current=%d requested=%d granted=%d new=%d capacity=%d",
			currentAlloc, requestedDelta, granted, newReservation, capacity)
		baseline := state.reservation
		if requestedDelta == 0 && currentAlloc < state.reservation {
			h.mu.Lock()
			h.reservationBytes = currentAlloc
			h.mu.Unlock()
			baseline = currentAlloc
		}
		h.markDisconnectedLocked(protocolErr)
		return 0, baseline, time.Duration(cooldownMs) * time.Millisecond, protocolErr
	}
	h.mu.Lock()
	h.reservationBytes = newReservation
	h.mu.Unlock()
	return granted, newReservation, time.Duration(cooldownMs) * time.Millisecond, nil
}

func (h *ControllerHooks) StartHeartbeat(ctx context.Context, period time.Duration) {
	if !h.Enabled() {
		return
	}
	bgCtx, cancel := context.WithCancel(ctx)
	h.addBackground(cancel)
	h.bgWG.Add(1)
	go func() {
		defer h.bgWG.Done()
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-ticker.C:
				h.heartbeatOnce()
			}
		}
	}()
}

func (h *ControllerHooks) heartbeatOnce() {
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	state := h.localState()
	if state.released || !state.admitted {
		return
	}
	if !state.connected {
		h.notifyReconnect()
		return
	}
	hostCharge := readHostMemoryChargeBestEffort(h.opts.CgroupPath)
	res, err := h.client.HeartbeatContext(h.lifetimeCtx, hostCharge, 0, 0, 0)
	if err != nil {
		h.markDisconnectedLocked(err)
		return
	}
	if res == nil || res.NewAllocatable != state.reservation {
		got := uint64(0)
		if res != nil {
			got = res.NewAllocatable
		}
		h.markDisconnectedLocked(fmt.Errorf("heartbeat reservation mismatch: node=%d local=%d", got, state.reservation))
	}
}

func (h *ControllerHooks) addBackground(cancel context.CancelFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancelBg == nil {
		h.cancelBg = cancel
		return
	}
	old := h.cancelBg
	h.cancelBg = func() { old(); cancel() }
}

func (h *ControllerHooks) markDisconnectedLocked(err error) {
	h.setConnected(false)
	_ = h.client.Close()
	h.opts.Logf("controller disconnected: %v; retaining reservation baseline and reconnecting", err)
	h.notifyReconnect()
}

func (h *ControllerHooks) setConnected(connected bool) {
	h.mu.Lock()
	h.connected = connected
	h.mu.Unlock()
}

func (h *ControllerHooks) notifyReconnect() {
	select {
	case h.reconnectWake <- struct{}{}:
	default:
	}
}

func (h *ControllerHooks) reconnectLoop() {
	defer h.reconnectWG.Done()
	for {
		select {
		case <-h.lifetimeCtx.Done():
			return
		case <-h.reconnectWake:
		}
		backoff := 100 * time.Millisecond
		for {
			state := h.localState()
			if state.released {
				return
			}
			if !state.admitted || state.connected {
				break
			}
			h.sessionMu.Lock()
			state = h.localState()
			if state.connected || state.released || !state.admitted {
				h.sessionMu.Unlock()
				break
			}
			err := h.client.ConnectContext(h.lifetimeCtx)
			if err == nil {
				hostCharge := readHostMemoryChargeBestEffort(h.opts.CgroupPath)
				result, syncErr := h.client.StateSyncContext(h.lifetimeCtx, resource.StateSyncParams{
					SandboxID:                h.opts.SandboxID,
					AppliedAllocatableMemory: state.reservation,
					Settled:                  state.settled, CurrentRSS: hostCharge, PreviousToken: state.token,
				})
				if syncErr == nil && result.NewAllocatable != state.reservation {
					syncErr = fmt.Errorf("state_sync reservation mismatch: node=%d local=%d", result.NewAllocatable, state.reservation)
				}
				if syncErr == nil {
					h.mu.Lock()
					h.connected, h.previousToken = true, result.Token
					h.mu.Unlock()
					h.sessionMu.Unlock()
					h.opts.Logf("controller state sync restored token=%s reservation=%d settled=%v",
						shortToken(result.Token), state.reservation, state.settled)
					break
				}
				err = syncErr
			}
			_ = h.client.Close()
			h.setConnected(false)
			h.sessionMu.Unlock()
			if err != nil {
				h.opts.Logf("controller reconnect: %v", err)
			}
			if err := h.waitRetry(backoff); err != nil {
				return
			}
			backoff = nextBackoff(backoff)
		}
	}
}

func nextBackoff(current time.Duration) time.Duration {
	current *= 2
	if current > 5*time.Second {
		return 5 * time.Second
	}
	return current
}

func (h *ControllerHooks) waitRetry(base time.Duration) error {
	jitter := time.Duration(rand.Int64N(int64(base/2 + 1)))
	timer := time.NewTimer(base + jitter)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-h.opts.Context.Done():
		return h.opts.Context.Err()
	case <-h.lifetimeCtx.Done():
		return errors.New("controller hooks released")
	}
}

func (h *ControllerHooks) Release(reason string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return
	}
	h.released = true
	cancelBg := h.cancelBg
	h.mu.Unlock()
	if cancelBg != nil {
		cancelBg()
	}
	h.cancelLifetime()
	h.bgWG.Wait()
	h.reconnectWG.Wait()
	if h.Enabled() {
		h.sessionMu.Lock()
		if h.client.Connected() {
			if err := h.client.Release(reason); err != nil && !resource.IsTransportError(err) {
				h.opts.Logf("controller.Release: %v", err)
			}
		}
		_ = h.client.Close()
		h.sessionMu.Unlock()
	}
	if h.lease != nil {
		if err := h.lease.Close(); err != nil {
			h.opts.Logf("controller lease cleanup: %v", err)
		}
	}
}

func readHostMemoryChargeBestEffort(cgroupPath string) uint64 {
	if cgroupPath == "" {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.current"))
	if err != nil {
		return 0
	}
	charge, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return charge
}

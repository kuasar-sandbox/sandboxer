package resctl

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

type ControllerHookOptions struct {
	SocketPath string
	CgroupPath string
	SandboxID  string
	Context    context.Context
	Logf       func(string, ...any)
	Balloon    *BalloonController
}

// ControllerHooks owns one recoverable controller session. sessionMu covers a
// complete RPC response application (network -> cgroup/balloon -> local state)
// and connection replacement, so a stale response cannot race StateSync.
type ControllerHooks struct {
	opts ControllerHookOptions
	cfg  *config.SandboxConfig

	sessionMu sync.Mutex
	mu        sync.Mutex
	client    *resource.Client
	lease     *resource.LeaseHandle

	admitted          bool
	settled           bool
	connected         bool
	allocatableNowMem uint64
	desiredAllocMem   uint64
	previousToken     string
	released          bool

	lifetimeCtx    context.Context
	cancelLifetime context.CancelFunc
	reconnectWake  chan struct{}
	reconnectWG    sync.WaitGroup

	cancelBg context.CancelFunc
	bgWG     sync.WaitGroup
}

func NewControllerHooks(opts ControllerHookOptions, cfg *config.SandboxConfig) (*ControllerHooks, error) {
	if cfg == nil {
		return nil, fmt.Errorf("controller hooks require config")
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	lifetimeCtx, cancel := context.WithCancel(context.Background())
	h := &ControllerHooks{
		opts: opts, cfg: cfg, lifetimeCtx: lifetimeCtx, cancelLifetime: cancel,
		reconnectWake: make(chan struct{}, 1),
	}
	if opts.SocketPath == "" {
		return h, nil
	}
	if opts.SandboxID == "" {
		cancel()
		return nil, fmt.Errorf("dynamic resource mode requires sandbox id")
	}
	capMem, err := cfg.CapacityMemoryBytes()
	if err != nil {
		cancel()
		return nil, err
	}
	floorMem, err := cfg.AllocatableMemoryBytes()
	if err != nil {
		cancel()
		return nil, err
	}
	startupMem, err := cfg.StartupBytes()
	if err != nil {
		cancel()
		return nil, err
	}
	lease, err := resource.CreateLease(resource.Lease{
		Version: resource.LeaseVersion, SandboxID: opts.SandboxID, PID: os.Getpid(),
		ControllerSocket: opts.SocketPath, CgroupPath: cfg.Resources.Control.CgroupPath,
		CapacityMemory: capMem, CapacityCPUMilli: uint64(cfg.Resources.Capacity.CPU) * 1000,
		FloorMemory: floorMem, FloorCPUMilli: uint64(cfg.Resources.Allocatable.CPU * 1000),
		StartupMemory: startupMem, ClientFeatures: []string{resource.FeatureStateSyncV1},
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("controller lease: %w", err)
	}
	h.lease = lease
	h.client = &resource.Client{SocketPath: opts.SocketPath}
	h.reconnectWG.Add(1)
	go h.reconnectLoop()
	return h, nil
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

func (h *ControllerHooks) SetBalloon(b *BalloonController) {
	if h == nil {
		return
	}
	h.sessionMu.Lock()
	h.opts.Balloon = b
	h.sessionMu.Unlock()
}

func (h *ControllerHooks) AllocatableNowMem() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.allocatableNowMem
}

type localControllerState struct {
	admitted, settled, connected, released bool
	applied, desired                       uint64
	token                                  string
}

func (h *ControllerHooks) localState() localControllerState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return localControllerState{
		admitted: h.admitted, settled: h.settled, connected: h.connected,
		released: h.released, applied: h.allocatableNowMem,
		desired: h.desiredAllocMem, token: h.previousToken,
	}
}

func (h *ControllerHooks) Admit(sid string, allocatableAtSnapshot uint64) (uint64, error) {
	if !h.Enabled() {
		return h.cfg.StartupBytes()
	}
	if sid != h.opts.SandboxID {
		return 0, fmt.Errorf("admit sandbox id %q does not match lease %q", sid, h.opts.SandboxID)
	}
	capMem, err := h.cfg.CapacityMemoryBytes()
	if err != nil {
		return 0, err
	}
	floorMem, err := h.cfg.AllocatableMemoryBytes()
	if err != nil {
		return 0, err
	}
	startupMem, err := h.cfg.StartupBytes()
	if err != nil {
		return 0, err
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
			if err := h.client.Connect(); err != nil {
				if err := h.waitRetry(backoff); err != nil {
					return 0, err
				}
				backoff = nextBackoff(backoff)
				continue
			}
		}
		res, err := h.client.Admit(resource.AdmitParams{
			SandboxID: sid, CapacityMemoryBytes: capMem,
			CapacityCPU:      h.cfg.Resources.Capacity.CPU,
			FloorMemoryBytes: floorMem, FloorCPU: h.cfg.Resources.Allocatable.CPU,
			StartupBudgetMemory: startupMem, AllocatableAtSnapshot: allocatableAtSnapshot,
			CgroupPath:     h.cfg.Resources.Control.CgroupPath,
			ClientFeatures: []string{resource.FeatureStateSyncV1},
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
		h.mu.Lock()
		h.admitted, h.connected = true, true
		h.desiredAllocMem = res.GrantedInitialAlloc
		// Cold start has no pre-existing applied state, so the CH boot
		// balloon is constructed from this grant. Restore seeds the actual
		// snapshot value before Admit and must not be overwritten by intent.
		if h.allocatableNowMem == 0 {
			h.allocatableNowMem = res.GrantedInitialAlloc
		}
		h.previousToken = res.Token
		h.mu.Unlock()
		if res.QueuedForMs > 0 {
			h.opts.Logf("controller admit: token=%s initial_alloc=%d (queued %dms, pos %d at entry)",
				shortToken(res.Token), res.GrantedInitialAlloc, res.QueuedForMs, res.QueuePosAtIn)
		} else {
			h.opts.Logf("controller admit: token=%s initial_alloc=%d", shortToken(res.Token), res.GrantedInitialAlloc)
		}
		return res.GrantedInitialAlloc, nil
	}
}

func shortToken(token string) string {
	if len(token) > 8 {
		return token[:8]
	}
	return token
}

func (h *ControllerHooks) Settled() error {
	if h == nil {
		return nil
	}
	floor, err := h.cfg.AllocatableMemoryBytes()
	if err != nil {
		return err
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	state := h.localState()
	// The guest crossed the settle barrier independently of controller or
	// local enforcement health. Commit that fact first so a reconnect can
	// replay it even when memory.high/balloon or the Settled RPC fails.
	h.mu.Lock()
	h.settled = true
	h.mu.Unlock()
	target := state.desired
	if target == 0 {
		target = state.applied
	}
	if target == 0 {
		target = floor
	}
	applyErr := h.applyAllocatableLocked(h.applyContext(), target)
	notifyErr := h.notifySettledLocked("controller.Settled")
	if applyErr != nil {
		return errors.Join(fmt.Errorf("settled apply allocatable: %w", applyErr), notifyErr)
	}
	return notifyErr
}

func (h *ControllerHooks) SettledRestore(allocAtSnap, desiredAlloc uint64) error {
	if h == nil {
		return nil
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	state := h.localState()
	// restore_ack is the local settle barrier. Record it before any fallible
	// post-resume resource correction or controller notification.
	h.mu.Lock()
	h.settled = true
	h.mu.Unlock()
	if desiredAlloc == 0 {
		desiredAlloc = state.desired
	}
	if desiredAlloc == 0 {
		desiredAlloc = allocAtSnap
	}
	applyErr := h.applyAllocatableLocked(h.applyContext(), desiredAlloc)
	if applyErr == nil && desiredAlloc != allocAtSnap {
		h.opts.Logf("settled-restore: applied correction alloc %d → %d", allocAtSnap, desiredAlloc)
	}
	notifyErr := h.notifySettledLocked("controller.SettledRestore")
	if applyErr != nil {
		return errors.Join(fmt.Errorf("settled-restore apply allocatable: %w", applyErr), notifyErr)
	}
	return notifyErr
}

// notifySettledLocked publishes the irreversible guest settle barrier even if
// local enforcement just failed. The controller therefore releases startup
// accounting; the locally retained applied value remains the StateSync truth.
// Caller holds sessionMu.
func (h *ControllerHooks) notifySettledLocked(operation string) error {
	h.mu.Lock()
	connected := h.connected
	h.mu.Unlock()
	if !h.Enabled() {
		return nil
	}
	if !connected {
		h.notifyReconnect()
		return nil
	}
	rss := readMemoryCurrent(h.opts.CgroupPath)
	if err := h.client.Settled(rss, 0); err != nil {
		if resource.IsTransportError(err) {
			h.markDisconnectedLocked(err)
			return nil
		}
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

// SetRestoreAppliedAllocatable records the allocation encoded in the restored
// balloon device before Admit. It is an observed value, not controller intent.
func (h *ControllerHooks) SetRestoreAppliedAllocatable(allocBytes uint64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.allocatableNowMem = allocBytes
	h.mu.Unlock()
}

func (h *ControllerHooks) OnAllocatableChanged(allocBytes uint64) error {
	if h == nil {
		return nil
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	return h.applyAllocatableLocked(h.applyContext(), allocBytes)
}

func (h *ControllerHooks) applyContext() context.Context {
	if h != nil && h.lifetimeCtx != nil {
		return h.lifetimeCtx
	}
	return context.Background()
}

func (h *ControllerHooks) applyAllocatableLocked(ctx context.Context, allocBytes uint64) error {
	if err := h.setMemoryHigh(allocBytes); err != nil {
		return err
	}
	if h.opts.Balloon != nil {
		var err error
		if h.Enabled() {
			err = h.opts.Balloon.ApplyAllocatable(ctx, allocBytes)
		} else {
			err = h.opts.Balloon.ApplyAllocatableEventually(ctx, allocBytes)
		}
		if err != nil {
			return err
		}
	}
	h.mu.Lock()
	h.allocatableNowMem = allocBytes
	h.desiredAllocMem = allocBytes
	h.mu.Unlock()
	return nil
}

func (h *ControllerHooks) setMemoryHigh(allocBytes uint64) error {
	if h == nil || h.opts.CgroupPath == "" {
		return nil
	}
	ratio := 0.875
	if h.cfg.Resources.WatermarkHigh != nil && h.cfg.Resources.WatermarkHigh.Memory != "" {
		alloc, _ := h.cfg.AllocatableMemoryBytes()
		cur, parseErr := h.cfg.WatermarkHighBytes()
		if parseErr == nil && alloc > 0 {
			ratio = float64(cur) / float64(alloc)
		}
	}
	newHigh := uint64(float64(allocBytes) * ratio)
	path := filepath.Join(h.opts.CgroupPath, "memory.high")
	if err := os.WriteFile(path, []byte(strconv.FormatUint(newHigh, 10)), 0o644); err != nil {
		return fmt.Errorf("write memory.high: %w", err)
	}
	return nil
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
				h.heartbeatOnce(bgCtx)
			}
		}
	}()
}

func (h *ControllerHooks) heartbeatOnce(ctx context.Context) {
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
	rss := readMemoryCurrent(h.opts.CgroupPath)
	res, err := h.client.Heartbeat(rss, 0, 0, 0)
	if err != nil {
		if resource.IsTransportError(err) {
			h.markDisconnectedLocked(err)
		} else {
			h.opts.Logf("heartbeat: %v", err)
		}
		return
	}
	if res != nil && res.NewAllocatable > 0 && res.NewAllocatable != state.applied {
		h.opts.Logf("heartbeat: controller adjusted alloc %d → %d, applying", state.applied, res.NewAllocatable)
		if err := h.applyAllocatableLocked(ctx, res.NewAllocatable); err != nil {
			h.opts.Logf("apply allocatable: %v", err)
		}
	}
}

// RequestBudget serializes the grant response with local application. It is the
// only positive-budget entry point used by the pressure sensor.
func (h *ControllerHooks) RequestBudget(step uint64, urgency, reason string) (uint64, uint64, time.Duration, error) {
	if !h.Enabled() {
		return 0, h.AllocatableNowMem(), 0, nil
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	state := h.localState()
	if !state.connected {
		h.notifyReconnect()
		return 0, state.applied, 0, &resource.TransportError{Err: errors.New("controller session disconnected")}
	}
	granted, newAlloc, cooldownMs, err := h.client.RequestBudget(state.applied, step, urgency, reason)
	if err != nil {
		if resource.IsTransportError(err) {
			h.markDisconnectedLocked(err)
		}
		return 0, state.applied, 0, err
	}
	if granted > 0 {
		if err := h.applyAllocatableLocked(h.applyContext(), newAlloc); err != nil {
			return 0, state.applied, time.Duration(cooldownMs) * time.Millisecond, err
		}
	}
	return granted, newAlloc, time.Duration(cooldownMs) * time.Millisecond, nil
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
	h.opts.Logf("controller disconnected: %v; retaining applied allocatable and reconnecting", err)
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
			err := h.client.Connect()
			if err == nil {
				rss := readMemoryCurrent(h.opts.CgroupPath)
				result, syncErr := h.client.StateSync(resource.StateSyncParams{
					SandboxID: h.opts.SandboxID, AppliedAllocatableMemory: state.applied,
					Settled: state.settled, CurrentRSS: rss, PreviousToken: state.token,
				})
				if syncErr == nil {
					h.mu.Lock()
					h.connected, h.previousToken = true, result.Token
					h.mu.Unlock()
					h.sessionMu.Unlock()
					h.opts.Logf("controller state sync restored token=%s allocatable=%d settled=%v",
						shortToken(result.Token), state.applied, state.settled)
					break
				}
				if resource.IsStateSyncUnsupported(syncErr) && state.token != "" {
					legacyAlloc, reattachErr := h.client.ReattachState(state.token)
					if reattachErr == nil {
						if legacyAlloc > 0 && legacyAlloc != state.applied {
							reattachErr = h.applyAllocatableLocked(h.applyContext(), legacyAlloc)
						}
						if reattachErr == nil {
							h.setConnected(true)
							h.sessionMu.Unlock()
							h.opts.Logf("controller reattached through legacy protocol token=%s", shortToken(state.token))
							break
						}
					}
					err = reattachErr
				} else {
					err = syncErr
				}
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
	h.bgWG.Wait()
	h.cancelLifetime()
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

func readMemoryCurrent(cgroupPath string) uint64 {
	if cgroupPath == "" {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.current"))
	if err != nil {
		return 0
	}
	var n uint64
	for _, b := range data {
		if b < '0' || b > '9' {
			break
		}
		n = n*10 + uint64(b-'0')
	}
	return n
}

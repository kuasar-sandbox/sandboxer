package resctl

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
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
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
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
	// Retained is a local inventory hint. The controller resolves the entire
	// SID account independently of this runtime's selected restore graph.
	Retained *resource.CaptureRetention
	// CheckpointAccount queries the node inventory even if the local capture
	// set is empty, since an account header can outlive its last R.
	CheckpointAccount bool
}

// ControllerHooks is a reservation adapter. It deliberately knows nothing
// about guest reports, CH balloon state, memory.high, or lifecycle phases other
// than the existing Settled bit carried by StateSync.
type ControllerHooks struct {
	opts ControllerHookOptions
	cfg  *config.SandboxConfig

	// sessionMu covers one complete reservation RPC and connection
	// replacement. It prevents an old response from racing StateSync.
	sessionMu   sync.Mutex
	syncStartMu sync.Mutex // orders StateSync writes with all lifetime cancellation
	mu          sync.Mutex
	client      *resource.Client
	lease       *resource.LeaseHandle

	controllerCgroupPath     string
	controllerSocketIdentity string

	admitted             bool
	settled              bool
	connected            bool
	reservationBytes     uint64
	cacheMaximum         uint64
	cacheReservation     uint64
	cacheSupported       bool
	cacheCoordinated     bool
	cacheReclaim         resource.CacheReclaimState
	cacheReclaimAck      uint64
	cacheReclaimTarget   uint64
	cacheReclaimSeq      uint64
	cacheWake            chan struct{}
	previousToken        string
	released             bool
	releaseDone          chan struct{}
	ordinaryReleased     bool
	abortRetries         map[resource.CaptureRetention]bool
	retainedSupported    bool
	deferredVMMSupported bool
	retention            *resource.CaptureRetention
	retentionPrepared    bool
	retentionHolder      bool
	leaseCandidate       string
	handoffID            string

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
	// Parent cancellation must use the same gate as explicit Release. Direct
	// propagation would cancel lifetimeCtx in the check-to-write interval even
	// while the StateSync writer owns syncStartMu.
	lifetimeCtx, cancel := context.WithCancel(context.WithoutCancel(opts.Context))
	h := &ControllerHooks{
		opts: opts, cfg: cfg, lifetimeCtx: lifetimeCtx, cancelLifetime: cancel,
		reconnectWake: make(chan struct{}, 1), cacheWake: make(chan struct{}, 1),
	}
	cancelLocal := cancel
	stopParent := context.AfterFunc(opts.Context, func() {
		h.syncStartMu.Lock()
		cancelLocal()
		h.syncStartMu.Unlock()
	})
	// Constructor failures and Release also unregister the parent callback.
	// stopParent does not wait for a callback already waiting on syncStartMu.
	cancel = func() { stopParent(); cancelLocal() }
	h.cancelLifetime = cancel
	if opts.Context.Err() != nil {
		cancel() // no worker exists yet; preserve already-canceled construction
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
	cacheMax, _, err := cfg.Resources.DiffCOW.Bytes()
	if err != nil {
		cancel()
		return nil, err
	}
	h.cacheMaximum = cacheMax
	cacheFeatures := []string{resource.FeatureStateSyncV1, resource.FeatureRetainedMemoryV2, resource.FeatureDeferredVMMExit}
	if !diffstore.ElasticCacheSupported() {
		// Keep the fixed cache if payload pages cannot be charged and returned individually.
		h.cacheMaximum = 0
	} else {
		var run [16]byte
		if _, err := cryptorand.Read(run[:]); err != nil {
			cancel()
			return nil, fmt.Errorf("cache reclaim run identity: %w", err)
		}
		h.cacheReclaim.RunID = hex.EncodeToString(run[:])
		cacheFeatures = append(cacheFeatures, resource.FeatureCacheBudgetV1, resource.CacheMaximumFeature(cacheMax),
			resource.FeatureCacheReclaimV1, resource.CacheReclaimRunFeature(h.cacheReclaim.RunID))
	}
	leaseSpec := resource.Lease{
		Version: resource.LeaseVersion, SandboxID: opts.SandboxID, PID: os.Getpid(),
		ControllerSocket: controllerSocketIdentity, CgroupPath: h.controllerCgroupPath,
		CapacityMemory: capacity, CapacityCPUMilli: uint64(cfg.Resources.Capacity.CPU) * 1000,
		FloorMemory: headroom, FloorCPUMilli: cpuMilliCeil(cfg.Resources.Allocatable.CPU),
		// The immutable lease mirrors configured startup headroom. Only the
		// existing Admit field carries its aligned InitialBudget representation.
		StartupMemory: startupHeadroom, ClientFeatures: cacheFeatures,
	}
	var lease *resource.LeaseHandle
	if opts.Retained != nil || opts.CheckpointAccount {
		if opts.Retained != nil {
			if err := opts.Retained.Validate(); err != nil {
				cancel()
				return nil, err
			}
		}
		var operation [16]byte
		if _, err := cryptorand.Read(operation[:]); err != nil {
			cancel()
			return nil, err
		}
		h.handoffID = hex.EncodeToString(operation[:])
		lease, err = resource.CreateLeaseCandidate(leaseSpec)
		if err == nil {
			h.leaseCandidate = lease.Path()
		}
	} else {
		lease, err = resource.CreateLease(leaseSpec)
	}
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
	cacheReservation                       uint64
	cacheMaximum                           uint64
	cacheSupported                         bool
	cacheCoordinated                       bool
	cacheReclaim                           *resource.CacheReclaimState
	cacheTarget                            uint64
	token                                  string
}

func (h *ControllerHooks) localState() localControllerState {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := localControllerState{
		admitted: h.admitted, settled: h.settled, connected: h.connected,
		released: h.released, reservation: h.reservationBytes,
		cacheReservation: h.cacheReservation, cacheMaximum: h.cacheMaximum, cacheSupported: h.cacheSupported, cacheTarget: h.cacheReclaimTarget,
		token: h.previousToken,
	}
	out.cacheCoordinated = h.cacheCoordinated
	if h.cacheCoordinated {
		out.cacheReclaim = resource.CloneCacheReclaimState(&h.cacheReclaim)
	}
	return out
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
	var account *resource.RetainedAccount
	accountKnown := false
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
		cacheFeatures := []string{resource.FeatureStateSyncV1, resource.FeatureRetainedMemoryV2, resource.FeatureDeferredVMMExit}
		if h.cacheMaximum > 0 {
			cacheFeatures = append(cacheFeatures, resource.FeatureCacheBudgetV1, resource.CacheMaximumFeature(h.cacheMaximum))
			if h.cacheReclaim.RunID != "" {
				cacheFeatures = append(cacheFeatures, resource.FeatureCacheReclaimV1, resource.CacheReclaimRunFeature(h.cacheReclaim.RunID))
			}
		}
		var reclaim *resource.CacheReclaimState
		if h.cacheReclaim.RunID != "" {
			reclaim = resource.CloneCacheReclaimState(&h.cacheReclaim)
			reclaim.Current = min(h.cacheMaximum, cacheBudgetUnit)
		}
		params := resource.AdmitParams{
			SandboxID: sid, CapacityMemoryBytes: capacity,
			CapacityCPU:      h.cfg.Resources.Capacity.CPU,
			FloorMemoryBytes: headroom, FloorCPU: h.cfg.Resources.Allocatable.CPU,
			StartupBudgetMemory: startupBudget, AllocatableAtSnapshot: budgetAtSnapshot,
			CgroupPath: h.controllerCgroupPath, ClientFeatures: cacheFeatures, CacheMaximum: h.cacheMaximum, CacheInitial: min(h.cacheMaximum, cacheBudgetUnit),
			CacheReclaim: reclaim,
		}
		var res *resource.AdmitResult
		if h.leaseCandidate != "" {
			// Before any owner-side lookup following an ACK loss, register the
			// already-held inode if the controller published it.
			if _, err := h.lease.ConfirmPublished(); err != nil {
				return 0, err
			}
			// A transport failure may follow acceptance. Keep the exact
			// reference until the node definitively rejects it as stale.
			var lookupErr error
			if !accountKnown {
				account, lookupErr = h.client.RetainedAccountContext(h.lifetimeCtx, sid)
				accountKnown = lookupErr == nil
			}
			if lookupErr != nil {
				err = lookupErr
			} else if account == nil && h.opts.Retained == nil {
				err = h.publishOrdinaryLease()
				if err == nil {
					res, err = h.client.AdmitContext(h.lifetimeCtx, params)
				}
			} else {
				res, err = h.client.AdoptRetainedAccountContext(h.lifetimeCtx, params, account, h.opts.Retained, h.leaseCandidate, h.handoffID)
			}
		} else {
			res, err = h.client.AdmitContext(h.lifetimeCtx, params)
		}
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
			if h.leaseCandidate != "" && res.Reason == resource.ReasonRetentionAccountChanged && resource.HasFeature(res.ClientFeatures, resource.FeatureRetainedMemoryV2) {
				accountKnown = false
				continue
			}
			if h.opts.Retained != nil && res.Reason == resource.ReasonRetentionSettledMissing &&
				resource.HasFeature(res.ClientFeatures, resource.FeatureRetainedMemoryV2) {
				// The node positively verified RAMReleased for this exact
				// capture and has no prior ledger (e.g. static -> dynamic).
				// Close the unpublished candidate before ordinary publication.
				if err := h.publishOrdinaryLease(); err != nil {
					return 0, err
				}

				continue
			}
			if res.Reason != "" {
				return 0, fmt.Errorf("admit rejected (%s): %s", res.Reason, res.Msg)
			}
			return 0, fmt.Errorf("admit rejected: %s", res.Msg)
		}
		if res.Status != resource.StatusAdmitted {
			return 0, fmt.Errorf("admit returned unexpected status %q", res.Status)
		}
		if h.leaseCandidate != "" {
			published, err := h.lease.ConfirmPublished()
			if err != nil || !published {
				return 0, fmt.Errorf("admitted handoff lease was not published: %v", err)
			}
		}
		if res.GrantedInitialAlloc != initialBudget {
			return 0, fmt.Errorf("admit granted partial/incorrect initial reservation %d, require exactly %d", res.GrantedInitialAlloc, initialBudget)
		}
		if res.CacheSupported {
			expectedCache := min(h.cacheMaximum, cacheBudgetUnit)
			if res.GrantedInitialCache != expectedCache {
				return 0, fmt.Errorf("admit cache initial grant %d differs from %d", res.GrantedInitialCache, expectedCache)
			}
		}
		if res.CacheCoordinated && (!res.CacheSupported || reclaim == nil) {
			return 0, fmt.Errorf("admit confirmed unrequested cache coordination")
		}
		h.mu.Lock()
		h.admitted, h.connected = true, true
		h.retainedSupported = resource.HasFeature(res.ClientFeatures, resource.FeatureRetainedMemoryV2)
		h.deferredVMMSupported = resource.HasFeature(res.ClientFeatures, resource.FeatureDeferredVMMExit)
		h.reservationBytes = initialBudget
		h.cacheSupported = res.CacheSupported
		h.cacheCoordinated = res.CacheCoordinated
		if res.CacheSupported {
			h.cacheReservation = res.GrantedInitialCache
			h.cacheReclaim.Current = res.GrantedInitialCache
		}
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
	if res != nil && state.cacheSupported {
		pendingReturn := state.cacheCoordinated && state.cacheReclaim.Done != nil &&
			res.CacheReservation >= state.cacheReservation && res.CacheReservation <= state.cacheMaximum
		if res.CacheReservation != state.cacheReservation && !pendingReturn {
			h.markDisconnectedLocked(fmt.Errorf("heartbeat cache reservation mismatch node=%d local=%d", res.CacheReservation, state.cacheReservation))
			return
		}
		if state.cacheCoordinated {
			if err := h.acceptCacheReclaimResponse(res.CacheReclaimTask, res.CacheReclaimAck); err != nil {
				h.markDisconnectedLocked(err)
				return
			}
		} else {
			h.setCacheTarget(res.CacheReclaimTarget, res.CacheReclaimSeq)
		}
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
			err := h.restoreSessionLocked(h.lifetimeCtx)
			if err == nil {
				h.sessionMu.Unlock()
				break
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

// restoreSessionLocked restores the active B/cache contract on a fresh stream.
// sessionMu must be held; retired holders must use their capture replay instead.
func (h *ControllerHooks) restoreSessionLocked(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state := h.localState()
	if state.released || !state.admitted {
		return fmt.Errorf("controller has no active session to restore")
	}
	if err := h.client.ConnectContext(ctx); err != nil {
		return err
	}
	hostCharge := readHostMemoryChargeBestEffort(h.opts.CgroupPath)
	h.syncStartMu.Lock()
	// Order the StateSync write with lifetime cancellation, including a parent
	// whose cancellation callback has not yet acquired this gate.
	if h.opts.Context.Err() != nil {
		h.cancelLifetime()
	}
	if err := h.lifetimeCtx.Err(); err != nil {
		h.syncStartMu.Unlock()
		return err
	}
	var startOnce sync.Once
	unlockStart := func() { startOnce.Do(h.syncStartMu.Unlock) }
	// Once written, drain the bounded RPC: its ACK may carry the only new token.
	// Release waits for sessionMu after joining the background reconnect worker.
	result, syncErr := h.client.StateSyncContextOnWrite(context.WithoutCancel(h.lifetimeCtx), resource.StateSyncParams{
		SandboxID:                h.opts.SandboxID,
		AppliedAllocatableMemory: state.reservation,
		AppliedCacheReservation:  state.cacheReservation, AppliedCacheMaximum: state.cacheMaximum,
		Settled: state.settled, CurrentRSS: hostCharge, PreviousToken: state.token,
		CacheReclaim: state.cacheReclaim,
	}, unlockStart, ctx)
	unlockStart()
	if syncErr == nil && state.cacheSupported && result.CacheReservation != state.cacheReservation {
		syncErr = fmt.Errorf("cache state_sync mismatch node=%d local=%d", result.CacheReservation, state.cacheReservation)
	}
	if syncErr == nil && result.NewAllocatable != state.reservation {
		syncErr = fmt.Errorf("state_sync reservation mismatch: node=%d local=%d", result.NewAllocatable, state.reservation)
	}
	if syncErr == nil && state.cacheCoordinated {
		if !resource.HasFeature(result.ClientFeatures, resource.FeatureCacheReclaimV1) ||
			!resource.HasFeature(result.ClientFeatures, resource.FeatureCacheBudgetV1) {
			syncErr = fmt.Errorf("state_sync did not confirm cache coordination")
		} else {
			syncErr = h.acceptCacheReclaimResponse(result.CacheReclaimTask, result.CacheReclaimAck)
		}
	}
	if syncErr != nil {
		return syncErr
	}
	h.mu.Lock()
	h.connected, h.previousToken = true, result.Token
	h.retainedSupported = resource.HasFeature(result.ClientFeatures, resource.FeatureRetainedMemoryV2)
	h.deferredVMMSupported = resource.HasFeature(result.ClientFeatures, resource.FeatureDeferredVMMExit)
	if !state.cacheCoordinated {
		h.cacheReclaimTarget = result.CacheReclaimTarget
		h.cacheReclaimSeq = result.CacheReclaimSeq
	}
	select {
	case h.cacheWake <- struct{}{}:
	default:
	}
	h.mu.Unlock()
	h.opts.Logf("controller state sync restored token=%s reservation=%d settled=%v",
		shortToken(result.Token), state.reservation, state.settled)
	return nil
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
	h.syncStartMu.Lock()
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		h.syncStartMu.Unlock()
		return
	}
	h.released = true
	h.releaseDone = make(chan struct{})
	releaseDone := h.releaseDone
	cancelBg := h.cancelBg
	h.mu.Unlock()
	if cancelBg != nil {
		cancelBg()
	}
	h.cancelLifetime()
	h.syncStartMu.Unlock()
	h.bgWG.Wait()
	h.reconnectWG.Wait()
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	defer close(releaseDone)
	h.mu.Lock()
	preserveRetained := h.retentionPrepared || h.retentionHolder
	h.mu.Unlock()
	if !preserveRetained {
		h.releaseOrdinaryLocked(reason)
		return
	}
	if h.Enabled() {
		_ = h.client.Close()
	}
}

// releaseOrdinaryLocked runs after normal controller work has stopped and the
// prepared retention has either committed or explicitly aborted. sessionMu
// serializes it with retention replay and protects the original client/token.
func (h *ControllerHooks) releaseOrdinaryLocked(reason string) {
	h.mu.Lock()
	if h.ordinaryReleased {
		h.mu.Unlock()
		return
	}
	h.ordinaryReleased = true
	h.mu.Unlock()
	if h.Enabled() {
		// Release reconnects on its bounded cleanup connection even after the
		// active lifetime and its control transport have already ended.
		if err := h.client.Release(reason); err != nil {
			h.opts.Logf("controller.Release: %v; retaining node recovery", err)
		}
		_ = h.client.Close()
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

// Used only after the v2 controller positively reports no account (and, for
// local capture hints, verifies the full inventory is physically released).
func (h *ControllerHooks) publishOrdinaryLease() error {
	spec, err := resource.ReadLease(h.lease.Path())
	if err != nil {
		return err
	}
	if err := h.lease.Close(); err != nil {
		return err
	}
	h.lease, err = resource.CreateLease(spec)
	if err != nil {
		return err
	}
	h.opts.Retained, h.leaseCandidate, h.handoffID = nil, "", ""
	return nil
}

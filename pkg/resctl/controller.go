package resctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

// ControllerHookOptions configures the dynamic-mode controller integration.
//
// SocketPath: UDS path of the controller endpoint. Empty disables the
// node-ctl RPC integration; hooks still run in a degraded form (writes
// cgroup memory.high, updates in-memory allocatable_now), but no
// controller RPCs are issued and no Heartbeat/Sensor goroutines start.
//
// CgroupPath: directory used for memory.high writes and memory.current /
// memory.events.local reads. Empty = no-cgroup mode.
//
// Balloon: the per-sandbox BalloonController owning /api/v1/vm.resize.
// Hooks never speaks CH HTTP directly — every balloon change goes
// through Balloon.SetAllocatable, so the BalloonController stays the
// sole writer of /vm.resize. May be nil when allocatable.memory ==
// capacity.memory (no balloon device on this VM).
type ControllerHookOptions struct {
	SocketPath       string
	CgroupPath       string
	ReservationToken string
	Logf             func(string, ...any)
	Balloon          *BalloonController
}

// ControllerHooks bundles the per-sandbox state for resource control:
// the node-ctl client (dynamic mode only), the current allocatable_now,
// and the background goroutines for Heartbeat and pressure-driven
// RequestBudget.
//
// A nil receiver — or a receiver whose SocketPath was empty at
// construction — is a safe no-op for every method that requires an
// active client; the cgroup-side writes (memory.high) and the balloon
// hand-off still happen in static mode. This lets lifecycle.go always
// call into hooks without branching on mode.
type ControllerHooks struct {
	opts ControllerHookOptions
	cfg  *config.SandboxConfig

	mu                sync.Mutex
	client            *resource.Client
	allocatableNowMem uint64
	released          bool
	cancelBg          context.CancelFunc
	bgWG              sync.WaitGroup
}

// NewControllerHooks dials the controller and returns hooks ready for
// Admit. Returns hooks with no client (static-mode no-op for RPC calls)
// when SocketPath is empty.
func NewControllerHooks(opts ControllerHookOptions, cfg *config.SandboxConfig) (*ControllerHooks, error) {
	h := &ControllerHooks{opts: opts, cfg: cfg}
	if h.opts.Logf == nil {
		h.opts.Logf = func(string, ...any) {}
	}
	if opts.SocketPath == "" {
		if opts.ReservationToken != "" {
			return nil, errors.New("resource controller is required for a prepared reservation")
		}
		return h, nil
	}
	h.client = &resource.Client{SocketPath: opts.SocketPath}
	if err := h.client.Connect(); err != nil {
		return nil, err
	}
	if opts.ReservationToken != "" {
		if err := h.client.OwnPreparedReservation(opts.ReservationToken); err != nil {
			_ = h.client.Close()
			return nil, err
		}
	}
	return h, nil
}

// Enabled reports whether dynamic-mode controller integration is in
// effect (i.e. a controller client is connected).
func (h *ControllerHooks) Enabled() bool {
	return h != nil && h.client != nil
}

// SetBalloon late-injects the BalloonController owning /vm.resize.
// Used by the restore path, where snapCap (and hence the BalloonController
// constructor argument) is only known after the snapshot bundle has
// been parsed — well after NewControllerHooks. lifecycle (cold-start)
// passes Balloon via ControllerHookOptions at construction and does not
// need this. Caller must ensure no concurrent Heartbeat/Sensor goroutine
// has been started yet (they read opts.Balloon).
func (h *ControllerHooks) SetBalloon(b *BalloonController) {
	if h == nil {
		return
	}
	h.opts.Balloon = b
}

// AllocatableNowMem returns the current allocatable_now in bytes. Set
// by SetAllocatableNow / OnAllocatableChanged / Settled. Returns 0 when
// the receiver is nil or no Settled-equivalent has run yet.
func (h *ControllerHooks) AllocatableNowMem() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.allocatableNowMem
}

// Admit performs the controller handshake. Caller passes capacity /
// floor / startup values from sandbox.yaml; allocatableAtSnapshot is
// non-zero only on the restore path.
//
// On success, the returned grantedInitialAlloc is what to use for
// initial cgroup memory.high and balloon target. The controller
// computes max(yaml.startup, yaml.allocatable, allocatable_at_snapshot)
// so the grant may exceed yaml.startup, but never falls below it.
func (h *ControllerHooks) Admit(sid string, allocatableAtSnapshot uint64) (uint64, error) {
	if !h.Enabled() {
		// Mode A/B: no admission, return startup (or floor if startup
		// not configured) so the cold-start cgroup setup works the same.
		burst, err := h.cfg.StartupBytes()
		if err != nil {
			return 0, err
		}
		return burst, nil
	}
	if h.opts.ReservationToken != "" {
		granted, err := h.client.Reattach(h.opts.ReservationToken)
		if err != nil {
			return 0, fmt.Errorf("reattach preassigned reservation: %w", err)
		}
		h.mu.Lock()
		h.allocatableNowMem = granted
		h.mu.Unlock()
		h.opts.Logf("controller reattach: initial_alloc=%d", granted)
		return granted, nil
	}
	cap, err := h.cfg.CapacityMemoryBytes()
	if err != nil {
		return 0, err
	}
	floor, err := h.cfg.AllocatableMemoryBytes()
	if err != nil {
		return 0, err
	}
	burst, err := h.cfg.StartupBytes()
	if err != nil {
		return 0, err
	}
	floorCPU := h.cfg.Resources.Allocatable.CPU
	res, err := h.client.Admit(resource.AdmitParams{
		SandboxID:             sid,
		CapacityMemoryBytes:   cap,
		CapacityCPU:           h.cfg.Resources.Capacity.CPU,
		FloorMemoryBytes:      floor,
		FloorCPU:              floorCPU,
		StartupBudgetMemory:   burst,
		AllocatableAtSnapshot: allocatableAtSnapshot,
		CgroupPath:            h.cfg.Resources.Control.CgroupPath,
	})
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
		// StatusQueued was deprecated when the controller moved to a
		// server-side hold-connection queue (the server blocks the conn
		// rather than returning Queued). Any other status is a server
		// protocol violation.
		return 0, fmt.Errorf("admit returned unexpected status %q", res.Status)
	}
	h.mu.Lock()
	h.allocatableNowMem = res.GrantedInitialAlloc
	h.mu.Unlock()
	if res.QueuedForMs > 0 {
		h.opts.Logf("controller admit: token=%s initial_alloc=%d (queued %dms, pos %d at entry)",
			res.Token[:8], res.GrantedInitialAlloc, res.QueuedForMs, res.QueuePosAtIn)
	} else {
		h.opts.Logf("controller admit: token=%s initial_alloc=%d",
			res.Token[:8], res.GrantedInitialAlloc)
	}
	return res.GrantedInitialAlloc, nil
}

// Settled marks the cold-start launch hello arrival.
//
// Writes cgroup memory.high using the locally visible allocatable_now. In
// static mode this is the floor. In dynamic mode it is the controller-granted
// startup budget until the next Heartbeat returns the controller's authoritative
// post-settled allocatable. This keeps the startup grant effective long enough
// for the user program to exec while still releasing the controller-side
// startup_pool as soon as launch_ack arrives.
//
// Does NOT touch the balloon directly. Cold-start CH was launched with a
// balloon derived from the initial allocatable (static floor or dynamic startup
// grant), and the first Heartbeat / reclaim / grant path applies later
// controller decisions through OnAllocatableChanged.
//
// In dynamic mode, also notifies the controller RPC of the settled
// transition.
func (h *ControllerHooks) Settled() error {
	if h == nil {
		return nil
	}
	floor, err := h.cfg.AllocatableMemoryBytes()
	if err != nil {
		return err
	}
	current := h.AllocatableNowMem()
	if current == 0 {
		current = floor
	}
	if err := h.setMemoryHigh(current); err != nil {
		h.opts.Logf("settled: setMemoryHigh: %v", err)
	}
	if !h.Enabled() {
		h.mu.Lock()
		h.allocatableNowMem = floor
		h.mu.Unlock()
		return nil
	}
	rss := readMemoryCurrent(h.opts.CgroupPath)
	if err := h.client.Settled(rss, 0); err != nil {
		return fmt.Errorf("controller.Settled: %w", err)
	}
	return nil
}

// SettledRestore is the restore-path equivalent of Settled, fired on
// the guest's restore_ack. Writes cgroup memory.high using the current
// allocatable_now (set pre-resume by restore.go via SetAllocatableNow;
// fallback = yaml.floor when in-memory state is zero — usually a bug
// elsewhere, but we don't want to crash).
//
// allocAtSnap is the allocatable_at_snapshot derived from the bundle's
// state.json balloon section. The CH balloon was reloaded to that value
// when CH started with --restore, so a balloon /vm.resize is only issued
// when the local allocatable_now decision (yaml.max-with-snap in static,
// controller granted in dynamic) differs from it. When they agree —
// the common case — no balloon write happens, avoiding mmu_notifier
// traffic on top of the ongoing uffd-driven page replay.
//
// In dynamic mode, also notifies the controller RPC.
func (h *ControllerHooks) SettledRestore(allocAtSnap uint64) error {
	if h == nil {
		return nil
	}
	cur := h.AllocatableNowMem()
	if cur == 0 {
		if floor, err := h.cfg.AllocatableMemoryBytes(); err == nil {
			cur = floor
		}
	}
	if err := h.setMemoryHigh(cur); err != nil {
		h.opts.Logf("settled-restore: setMemoryHigh: %v", err)
	}
	// Balloon was already at `cap - allocAtSnap` from the snapshot
	// restore. Only correct it when the decision diverges.
	if cur != allocAtSnap && h.opts.Balloon != nil {
		h.opts.Balloon.SetAllocatable(cur)
		h.opts.Logf("settled-restore: balloon correction alloc %d → %d", allocAtSnap, cur)
	}
	if !h.Enabled() {
		return nil
	}
	rss := readMemoryCurrent(h.opts.CgroupPath)
	if err := h.client.Settled(rss, 0); err != nil {
		return fmt.Errorf("controller.SettledRestore: %w", err)
	}
	return nil
}

// SetAllocatableNow updates the in-memory allocatable_now state without
// any external side effect (no cgroup write, no balloon write).
//
// Used by restore.go before vm.resume to record the controller-granted
// (or static-mode-decided) allocatable, so that the subsequent
// SettledRestore can compute memory.high and decide whether a balloon
// correction is needed. Doing the in-memory update before CH is even up
// is safe; doing any external write here is not (CH socket doesn't
// exist yet pre-cmd.Start; even after, pre-resume balloon writes step
// on the snapshot-loaded balloon state, and pre-replay memory.high
// throttles the uffd page-fault burst).
func (h *ControllerHooks) SetAllocatableNow(allocBytes uint64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.allocatableNowMem = allocBytes
	h.mu.Unlock()
}

// OnAllocatableChanged is the runtime-adjustment entry point used by
// the dynamic-mode Heartbeat and Sensor loops when the controller's
// authoritative allocatable_now has changed (admin grant, admin
// reclaim, or sensor-triggered RequestBudget). Writes cgroup memory.high
// and the balloon target together, then updates allocatable_now state.
//
// memory.high and balloon move in lock-step per docs/sandbox.md §10.2:
// PSI threshold and host-side reclaim target must agree, or the guest
// hits PSI throttling at a watermark inconsistent with what the balloon
// is letting it use. No-op on the balloon side when Balloon is nil
// (allocatable.memory == capacity.memory — no balloon device).
func (h *ControllerHooks) OnAllocatableChanged(allocBytes uint64) error {
	if h == nil {
		return nil
	}
	if err := h.setMemoryHigh(allocBytes); err != nil {
		return err
	}
	if h.opts.Balloon != nil {
		h.opts.Balloon.SetAllocatable(allocBytes)
	}
	h.mu.Lock()
	h.allocatableNowMem = allocBytes
	h.mu.Unlock()
	return nil
}

// setMemoryHigh writes memory.high = allocBytes * watermark_ratio. The
// ratio is taken from the user-configured WatermarkHigh.memory if set,
// otherwise defaults to 0.875 (matching WatermarkHighBytes default).
// No-op when CgroupPath unset.
func (h *ControllerHooks) setMemoryHigh(allocBytes uint64) error {
	if h == nil || h.opts.CgroupPath == "" {
		return nil
	}
	ratio := 0.875
	if h.cfg.Resources.WatermarkHigh != nil && h.cfg.Resources.WatermarkHigh.Memory != "" {
		alloc, _ := h.cfg.AllocatableMemoryBytes()
		cur, perr := h.cfg.WatermarkHighBytes()
		if perr == nil && alloc > 0 {
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

// StartHeartbeat spawns the periodic Heartbeat goroutine. Stops on ctx
// cancel or Release. No-op in static mode (no controller to talk to).
//
// Each heartbeat returns the controller's authoritative allocatable_now.
// If it differs from the local value, the active reclaimer (or an
// admin reclaim/grant command) changed our budget; we apply it via
// OnAllocatableChanged so cgroup memory.high and CH balloon target
// move together.
func (h *ControllerHooks) StartHeartbeat(ctx context.Context, period time.Duration) {
	if !h.Enabled() {
		return
	}
	bgCtx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	if h.cancelBg == nil {
		h.cancelBg = cancel
	} else {
		old := h.cancelBg
		h.cancelBg = func() { old(); cancel() }
	}
	h.mu.Unlock()
	h.bgWG.Add(1)
	go func() {
		defer h.bgWG.Done()
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-t.C:
				rss := readMemoryCurrent(h.opts.CgroupPath)
				res, err := h.client.Heartbeat(rss, 0, 0, 0)
				if err != nil {
					h.opts.Logf("heartbeat: %v (will retry next tick)", err)
					continue
				}
				if res != nil && res.NewAllocatable > 0 {
					local := h.AllocatableNowMem()
					if res.NewAllocatable != local {
						h.opts.Logf("heartbeat: controller adjusted alloc %d → %d, applying",
							local, res.NewAllocatable)
						if err := h.OnAllocatableChanged(res.NewAllocatable); err != nil {
							h.opts.Logf("apply allocatable: %v", err)
						}
					}
				}
			}
		}
	}()
}

// Release sends a final Release message and tears down the connection.
// Idempotent.
func (h *ControllerHooks) Release(reason string) {
	if !h.Enabled() {
		return
	}
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return
	}
	h.released = true
	h.mu.Unlock()
	if h.cancelBg != nil {
		h.cancelBg()
	}
	h.bgWG.Wait()
	if err := h.client.Release(reason); err != nil {
		h.opts.Logf("controller.Release: %v", err)
	}
	_ = h.client.Close()
}

// readMemoryCurrent reads memory.current from a cgroup directory. Returns
// 0 when the file is absent or the cgroup_path is empty.
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

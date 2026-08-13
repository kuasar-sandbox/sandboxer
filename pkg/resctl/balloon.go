package resctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// BalloonController drives the cloud-hypervisor virtio-balloon size via
// /api/v1/vm.resize, replacing virtio-balloon free-page-reporting.
//
// Why not FPR: the guest's FPR work item produces ~30 K UFFD_REMOVE
// events per second; CH's release_memory_range path then issues
// madvise(MADV_DONTNEED) on its own mmap, which broadcasts mmu_notifier
// invalidations into the KVM EPT. The cumulative shootdown traffic
// starves the guest's vsock kthread (timer interrupts get lost mid-IPI),
// the OP_REQUEST → OP_RESPONSE handshake never completes, and every
// host→guest ping deadlines out. The bug is reproducible in ≤ 30 s.
//
// The replacement loop:
//
//	guest sandbox-init  ──── mem_report (vsock) ───►  BalloonController.Hint
//	                          MemAvailable                       │
//	                                                             ▼
//	                ◄──── PUT /api/v1/vm.resize  ──── reconcile (5 s ticker)
//
// Hint takes the latest /proc/meminfo snapshot and recomputes the
// target balloon size to keep the guest's free buffer near
// TargetFreeBuffer. SetTarget is also exposed for direct overrides
// from the resource controller (node-ctl grant/reclaim).
//
// Trade-off vs FPR: reclaim latency rises from ~2 s (fixed) to
// ~5–10 s (one or two reconcile ticks), but mmu_notifier traffic is
// host-bounded by MaxStep per tick. In practice the guest no longer
// deadlocks and the host still gets its physical memory back within
// a sandbox heartbeat.
type BalloonController struct {
	// Capacity is the VM's full RAM in bytes. Target is clamped to this.
	Capacity uint64

	// Interval between reconcile ticks. Default 5 s.
	Interval time.Duration

	// TargetFreeBuffer is the guest-visible free-memory cushion the
	// controller tries to maintain. Default max(64 MiB, Capacity/32).
	// Smaller → more aggressive reclaim; larger → more burst headroom.
	TargetFreeBuffer uint64

	// MaxStep caps the absolute change applied per Hint, in bytes.
	// Bounds the per-tick mmu_notifier burst from the inflate path.
	// Default 256 MiB.
	MaxStep uint64

	// Slack: ignore Hint-driven changes smaller than this. Default 32 MiB.
	Slack uint64

	// Logf is the structured logger. Required.
	Logf func(string, ...any)

	// chSock is the path to CH's HTTP API UDS.
	chSock string
	client *http.Client

	target      atomic.Uint64 // desired balloon size in bytes
	actual      atomic.Uint64 // last successfully applied size
	reconcileMu sync.Mutex

	// kick is a non-blocking signal channel: SetTarget (and therefore
	// SetAllocatable, which wraps SetTarget) pokes it on every write.
	// The reconcile loop responds immediately when the previous reconcile
	// was ≥ Interval ago, otherwise it drops the kick and lets the next
	// ticker tick apply the pending target. Cap = 1, drop-on-full.
	kick chan struct{}

	// lastReconcileAt is the wall time of the most recent successful or
	// no-op Reconcile, in unix nanos. Used by the loop to decide whether
	// a kick can fire immediately.
	lastReconcileAt atomic.Int64

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// NewBalloonController constructs a controller targeting the given
// CH api-socket. capacity must equal the VM's --memory-zone size in
// bytes. Defaults are filled in when not set by the caller.
func NewBalloonController(chSock string, capacity uint64, logf func(string, ...any)) *BalloonController {
	tr := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", chSock)
		},
	}
	return &BalloonController{
		Capacity: capacity,
		Logf:     logf,
		chSock:   chSock,
		client:   &http.Client{Transport: tr, Timeout: 3 * time.Second},
		kick:     make(chan struct{}, 1),
	}
}

func (b *BalloonController) defaults() {
	if b.Interval <= 0 {
		b.Interval = 5 * time.Second
	}
	if b.TargetFreeBuffer == 0 {
		b.TargetFreeBuffer = 64 << 20
		if min := b.Capacity / 32; b.TargetFreeBuffer < min {
			b.TargetFreeBuffer = min
		}
	}
	if b.MaxStep == 0 {
		b.MaxStep = 256 << 20
	}
	if b.Slack == 0 {
		b.Slack = 32 << 20
	}
	if b.Logf == nil {
		b.Logf = func(string, ...any) {}
	}
}

// SetTarget overrides the desired balloon size in bytes. Clamped to
// Capacity. Pokes the kick channel; the reconcile loop applies the new
// target immediately if the last reconcile was ≥ Interval ago, otherwise
// the pending change rides on the next ticker tick.
//
// Hint uses this for mem_report-driven feedback. External callers that
// think in terms of "guest-visible allocatable memory" should prefer
// SetAllocatable, which derives the balloon target from that.
func (b *BalloonController) SetTarget(sizeBytes uint64) {
	if sizeBytes > b.Capacity {
		sizeBytes = b.Capacity
	}
	b.target.Store(sizeBytes)
	select {
	case b.kick <- struct{}{}:
	default: // already pending; drop
	}
}

// SetAllocatable sets the balloon target from a guest-visible
// allocatable-memory budget: target = Capacity − allocBytes, clamped
// to [0, Capacity]. Used by ControllerHooks at every allocatable-now
// change (Settled / Heartbeat-grant / Sensor-grant / restore correction)
// and by lifecycle / restore at construction so the in-memory target
// matches the value baked into CH's --balloon arg or the snapshot.
func (b *BalloonController) SetAllocatable(allocBytes uint64) {
	b.SetTarget(b.targetForAllocatable(allocBytes))
}

// ApplyAllocatable synchronously commits an allocatable change to Cloud
// Hypervisor. ControllerHooks uses it inside the serialized resource session
// and advances appliedAllocatable only after this returns successfully.
func (b *BalloonController) ApplyAllocatable(ctx context.Context, allocBytes uint64) error {
	b.defaults()
	previous := b.target.Load()
	target := b.targetForAllocatable(allocBytes)
	b.target.Store(target)
	if err := b.Reconcile(ctx); err != nil {
		// Do not leave a failed controller budget queued for the background
		// reconcile loop. ControllerHooks deliberately keeps reporting the
		// previous applied allocation on error; a later untracked resize would
		// otherwise make StateSync undercount the live consumer. Preserve a
		// concurrent Hint/SetTarget update instead of overwriting it.
		b.target.CompareAndSwap(target, previous)
		return err
	}
	return nil
}

// SeedAppliedAllocatable initializes the desired and applied balloon target
// before Start when CH is launched with the same target on its command line.
// It deliberately does not queue a resize; later SetAllocatable calls retain
// their normal reconcile behavior.
func (b *BalloonController) SeedAppliedAllocatable(allocBytes uint64) {
	target := b.targetForAllocatable(allocBytes)
	b.target.Store(target)
	b.actual.Store(target)
}

func (b *BalloonController) targetForAllocatable(allocBytes uint64) uint64 {
	if b.Capacity > allocBytes {
		return b.Capacity - allocBytes
	}
	return 0
}

// Hint adjusts the balloon target based on a guest /proc/meminfo
// sample. The policy keeps the guest's free buffer near TargetFreeBuffer,
// stepped by at most MaxStep per call to bound mmu_notifier bursts.
//
// Math:
//
//	delta = memAvailable - TargetFreeBuffer
//	new_target = clamp(target + delta, 0, Capacity)
//	|delta| < Slack → no-op (anti-hunting)
//	|delta| > MaxStep → clamp to ±MaxStep
//
// memTotal > Capacity is the only impossible case we reject; the
// guest's MemTotal is normally a few % below Capacity (kernel +
// reserved zones).
//
// Stale-report guard: the controller may have just inflated the
// balloon by hundreds of MiB; the guest's MemAvailable lags by one
// or two ticks because the balloon driver hasn't actually evicted
// pages yet. If the report claims more free memory than the current
// balloon target leaves visible, it is stale — skipping it avoids
// driving the target to Capacity in a feedback runaway.
func (b *BalloonController) Hint(memAvailable, memTotal uint64) {
	b.defaults()
	if memTotal > b.Capacity {
		b.Logf("balloon: ignoring impossible mem_report total=%d MiB cap=%d MiB",
			memTotal>>20, b.Capacity>>20)
		return
	}

	// Visible memory after the current commitment = Capacity - max(target, actual).
	// `target` (a SetTarget already published, even if reconcile hasn't
	// applied it yet) is the authoritative "what guest is being asked to
	// give up". If memAvailable is wildly larger than that, the guest's
	// MemTotal hasn't yet reflected our pending inflate — skip the
	// sample to avoid driving target up further in a feedback runaway.
	const visibleSlack uint64 = 64 << 20
	committed := b.actual.Load()
	if pending := b.target.Load(); pending > committed {
		committed = pending
	}
	if committed > 0 && b.Capacity > committed {
		visible := b.Capacity - committed
		if memAvailable > visible+visibleSlack {
			b.Logf("balloon: stale mem_report avail=%d MiB visible=%d MiB (ignored)",
				memAvailable>>20, visible>>20)
			return
		}
	}

	delta := int64(memAvailable) - int64(b.TargetFreeBuffer)
	if abs64(delta) < int64(b.Slack) {
		return
	}
	if delta > int64(b.MaxStep) {
		delta = int64(b.MaxStep)
	} else if delta < -int64(b.MaxStep) {
		delta = -int64(b.MaxStep)
	}

	curr := b.target.Load()
	var next uint64
	if delta >= 0 {
		next = curr + uint64(delta)
	} else if uint64(-delta) > curr {
		next = 0
	} else {
		next = curr - uint64(-delta)
	}
	b.SetTarget(next)
}

// Start runs an immediate reconcile (so the post-Settled inflate
// happens as soon as the controller is engaged), then a ticker.
// Returns the initial reconcile error so the caller can fast-fail
// if CH's HTTP API is unreachable.
func (b *BalloonController) Start(ctx context.Context) error {
	b.defaults()
	var err error
	b.startOnce.Do(func() {
		if rerr := b.Reconcile(ctx); rerr != nil {
			err = fmt.Errorf("balloon: initial resize: %w", rerr)
			return
		}
		b.stopCh = make(chan struct{})
		b.doneCh = make(chan struct{})
		go b.loop(ctx)
	})
	return err
}

// Stop terminates the reconcile loop. Idempotent.
func (b *BalloonController) Stop() {
	b.stopOnce.Do(func() {
		if b.stopCh != nil {
			close(b.stopCh)
			<-b.doneCh
		}
	})
}

// CurrentTarget returns the desired balloon size in bytes.
func (b *BalloonController) CurrentTarget() uint64 { return b.target.Load() }

// CurrentActual returns the last successfully applied size in bytes.
func (b *BalloonController) CurrentActual() uint64 { return b.actual.Load() }

func (b *BalloonController) loop(ctx context.Context) {
	defer close(b.doneCh)
	t := time.NewTicker(b.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case <-t.C:
			if err := b.Reconcile(ctx); err != nil {
				b.Logf("balloon: reconcile: %v", err)
			}
		case <-b.kick:
			// Immediate-response path: SetTarget/SetAllocatable poked us.
			// Honour the Interval rate limit — if the last reconcile is
			// fresher than Interval, drop this kick and let the next
			// ticker tick (≤ Interval - elapsed away) catch the pending
			// target. Reconcile is idempotent (no-op when target==actual),
			// so a dropped kick can never leave the state divergent.
			last := time.Unix(0, b.lastReconcileAt.Load())
			if time.Since(last) < b.Interval {
				continue
			}
			if err := b.Reconcile(ctx); err != nil {
				b.Logf("balloon: reconcile (kick): %v", err)
			}
			// Re-anchor the ticker so the next periodic tick is one
			// full Interval away from this kick-driven reconcile, not
			// from the original Ticker start.
			t.Reset(b.Interval)
		}
	}
}

// Reconcile applies the current target to CH if it differs from the
// last applied value. Idempotent; safe to call concurrently with
// SetTarget/Hint.
func (b *BalloonController) Reconcile(ctx context.Context) error {
	b.reconcileMu.Lock()
	defer b.reconcileMu.Unlock()
	target := b.target.Load()
	// Record the attempt time regardless of whether a resize is actually
	// needed: the kick-rate-limit only cares "did we recently look", not
	// "did we recently change CH state".
	defer b.lastReconcileAt.Store(time.Now().UnixNano())
	if target == b.actual.Load() {
		return nil
	}
	if err := b.callResize(ctx, target); err != nil {
		return err
	}
	b.actual.Store(target)
	b.Logf("balloon: resized to %d MiB", target>>20)
	return nil
}

func (b *BalloonController) callResize(ctx context.Context, sizeBytes uint64) error {
	body, err := json.Marshal(struct {
		DesiredBalloon uint64 `json:"desired_balloon"`
	}{DesiredBalloon: sizeBytes})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://ch/api/v1/vm.resize", bytes.NewReader(body))
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

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

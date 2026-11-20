package resctl

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

const (
	cacheBudgetUnit           = uint64(4 << 20)
	cacheMaxBatch             = uint64(32 << 20)
	cachePhysicalBatch        = uint64(1 << 20)
	cachePhysicalReclaimBatch = uint64(256 << 10)
	defaultCacheIdleAfter     = 10 * time.Second
	cacheIdleTick             = time.Second
	cacheRetryWait            = 250 * time.Millisecond
)

// NewCacheWithBudget returns the existing fixed cache when the controller is
// absent or predates cache grants. On negotiated nodes it acquires the initial
// host memory grant before touching a single payload page, and returns a
// lifetime-bound stop function for the autonomous cache budget loop.
func NewCacheWithBudget(ctx context.Context, hooks *ControllerHooks, maximum, maxDirty, maxDirtyPerDisk uint64, idleAfter ...time.Duration) (*diffstore.Cache, func(), error) {
	cacheOptions := []diffstore.CacheOption{diffstore.WithPerDiskDirtyLimit(maxDirtyPerDisk)}
	if hooks == nil || !hooks.CacheSupported() {
		cache, err := diffstore.NewCache(maximum, maxDirty, cacheOptions...)
		return cache, func() {}, err
	}
	cleanIdleAfter := defaultCacheIdleAfter
	if len(idleAfter) > 0 {
		cleanIdleAfter = idleAfter[0]
	}
	if cleanIdleAfter <= 0 {
		return nil, nil, fmt.Errorf("cache_idle_after must be positive")
	}
	first := min(maximum, cacheBudgetUnit)
	if first == 0 {
		return nil, nil, fmt.Errorf("cache maximum is zero")
	}
	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()
	granted := hooks.CacheReservation()
	if granted > first {
		return nil, nil, fmt.Errorf("pregranted cache %d above initial %d", granted, first)
	}
	for granted < first {
		if err := startCtx.Err(); err != nil {
			return nil, nil, fmt.Errorf("initial cache budget: %w", err)
		}
		delta, next, _, cooldown, err := hooks.RequestCacheBudget(granted, first-granted)
		if err == nil {
			if next != granted+delta {
				return nil, nil, fmt.Errorf("invalid initial cache grant baseline")
			}
			granted = next
		}
		if granted >= first {
			break
		}
		wait := max(cacheRetryWait, cooldown)
		t := time.NewTimer(wait)
		select {
		case <-startCtx.Done():
			t.Stop()
			return nil, nil, fmt.Errorf("initial cache budget: %w", startCtx.Err())
		case <-t.C:
		}
	}
	cache, err := diffstore.NewElasticCache(maximum, maxDirty, first, cacheOptions...)
	if err != nil {
		// Construction has already closed its transient backing on failure;
		// therefore even after a lost release reply this lower baseline is safe.
		_, _, _, _, _ = hooks.RequestCacheBudget(0, 0)
		return nil, nil, err
	}
	life, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	controller := &elasticCacheController{
		cache: cache, hooks: hooks, ctx: life,
		wake: make(chan struct{}, 1), growth: first, idleAfter: cleanIdleAfter,
	}
	controller.setWatermark(first)
	go func() { defer close(done); controller.run() }()
	var once sync.Once
	stop := func() {
		once.Do(cancel)
		<-done
	}
	return cache, stop, nil
}

type elasticCacheController struct {
	cache              *diffstore.Cache
	hooks              *ControllerHooks
	ctx                context.Context
	wake               chan struct{}
	growth             uint64
	growAfter          time.Time
	lastDemand         uint64
	pendingGrowth      bool
	moreIdleReclaim    bool
	pendingAck         uint64
	idleAfter          time.Duration
	reclaimObserved    diffstore.BudgetSnapshot
	reclaimObservedSet bool
	reclaimObservation uint64
	reclaimIntentSeq   uint64
	reclaimProbeIntent uint64
	reclaimZero        bool
	reclaimProbeAfter  time.Time
}

// Start preparing the next grant while existing slots can still serve a burst.
// Require at least a quarter of the current capacity to be active, and allow
// at most one maximum grant of headroom before signaling growth.
func cacheGrowthMargin(capacity, lastGrant uint64) uint64 {
	return min(capacity-capacity/4, cacheMaxBatch, max(uint64(4096), lastGrant*2))
}

func (c *elasticCacheController) setWatermark(step uint64) {
	capacity := c.cache.BudgetSnapshot().Capacity
	margin := cacheGrowthMargin(capacity, step)
	c.cache.SetBudgetNotifier(c.wake, max(uint64(4096), capacity-margin))
}

func (c *elasticCacheController) run() {
	ticker := time.NewTicker(cacheIdleTick)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.wake:
			c.adjust(false)
		case <-c.hooks.CacheWake():
			c.adjust(false)
		case <-ticker.C:
			c.adjust(true)
		}
	}
}

// adjust is the only controller worker: idle-threshold and node-directed
// requests both use the same bounded, asynchronous physical reclaim path.
func (c *elasticCacheController) adjust(tick bool) {
	if c.ctx.Err() != nil {
		return
	}
	s := c.cache.BudgetSnapshot()
	if tick {
		_ = c.cache.AgeClean(c.idleAfter)
		s = c.cache.BudgetSnapshot()
	}
	minimum := min(s.Maximum, cacheBudgetUnit)
	if c.hooks.CacheCoordinated() {
		if c.adjustCoordinatedReclaim(s, tick) {
			return
		}
	} else {
		target, seq := c.hooks.CacheRequest()
		if target > 0 {
			var released uint64
			if (seq == 0 || seq != c.pendingAck) && s.Capacity > max(minimum, target) {
				released = c.reclaimBudget(s.Capacity - max(minimum, target))
			}
			// A lost acknowledgement may be retried without evicting more pages.
			// StateSync can cancel this request or replace it with a new sequence.
			if released > 0 || seq != 0 {
				c.pendingAck = seq
				if c.reportReclaim(seq) == nil {
					c.pendingAck = 0
				}
			}
			return
		}
		c.pendingAck = 0

		// Observe unused grant headroom on maintenance ticks. A queued I/O wake
		// from before a grant must not immediately return the newly granted pages.
		if tick || c.moreIdleReclaim {
			c.moreIdleReclaim = false
			free := s.Capacity - s.Used
			idle := free + s.Expired
			if s.Capacity > minimum && idle >= cacheBudgetUnit {
				wanted := min(s.Capacity-minimum, idle/cacheBudgetUnit*cacheBudgetUnit)
				if released := c.reclaimBudget(wanted); released > 0 {
					_ = c.reportReclaim(0)
					if released == cacheMaxBatch && wanted > released {
						c.moreIdleReclaim = true
						c.notify()
					}
					return
				}
			}
		}
	}
	if s.Demand > c.lastDemand {
		c.pendingGrowth = true
	}
	if s.Capacity >= s.Maximum {
		c.pendingGrowth = false
		c.lastDemand = s.Demand
		return
	}
	margin := cacheGrowthMargin(s.Capacity, c.growth)
	if !cacheHasGrowthDemand(s, margin) {
		c.pendingGrowth = false
		return
	}
	if !c.pendingGrowth || time.Now().Before(c.growAfter) {
		return
	}
	request := min(s.Maximum-s.Capacity, min(cacheMaxBatch, max(cacheBudgetUnit, c.growth*2)))
	delta, newBudget, _, cooldown, err := c.hooks.RequestCacheBudget(s.Capacity, request)
	if err != nil {
		c.growAfter = time.Now().Add(cacheRetryWait)
		return
	}
	if delta == 0 {
		c.growAfter = time.Now().Add(max(cacheRetryWait, cooldown))
		return
	}
	if newBudget != s.Capacity+delta {
		c.growAfter = time.Now().Add(cacheRetryWait)
		return
	}
	applied := s.Capacity
	for applied < newBudget && c.ctx.Err() == nil {
		next := min(newBudget, applied+cachePhysicalBatch)
		ok, resizeErr := c.cache.TryResizeCapacity(next)
		if resizeErr != nil || !ok {
			break
		}
		applied = next
		runtime.Gosched()
	}
	if applied < newBudget {
		if c.cache.Err() == nil && c.cache.BudgetSnapshot().Resident <= applied {
			_, _, _, _, _ = c.hooks.RequestCacheBudget(applied, 0)
			c.hooks.confirmCacheReclaimCurrent(applied)
		}
		c.growth = max(cachePhysicalBatch, applied-s.Capacity)
		c.setWatermark(c.growth)
		c.growAfter = time.Now().Add(cacheRetryWait)
		return
	}
	c.hooks.confirmCacheReclaimCurrent(applied)
	c.lastDemand = s.Demand
	c.pendingGrowth = false
	c.growth = delta
	c.setWatermark(delta)
}

func (c *elasticCacheController) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func cacheHasGrowthDemand(s diffstore.BudgetSnapshot, margin uint64) bool {
	if s.Capacity == 0 {
		return false
	}
	active := s.Used - min(s.Used, s.Expired)
	threshold := s.Capacity - min(s.Capacity, margin)
	return active >= threshold
}

// reclaimBudget releases at most one bounded request per invocation. The caller
// never requires the requested amount to be returned.
func (c *elasticCacheController) reclaimBudget(request uint64) uint64 {
	s := c.cache.BudgetSnapshot()
	minimum := min(s.Maximum, cacheBudgetUnit)
	if s.Capacity <= minimum || request == 0 || c.ctx.Err() != nil {
		return 0
	}
	size := min(request, min(cacheMaxBatch, s.Capacity-minimum))
	beforeDemand := s.Demand
	next := max(minimum, s.Capacity-size)
	applied := s.Capacity
	var reclaimedInBurst uint64
	for applied > next && c.ctx.Err() == nil {
		step := max(next, applied-cachePhysicalReclaimBatch)
		ok, err := c.cache.TryResizeCapacity(step)
		if err != nil || !ok {
			break
		}
		reclaimedInBurst += applied - step
		applied = step
		if reclaimedInBurst >= (1 << 20) {
			// The same gentle pacing applies to local and node-directed reclaim.
			time.Sleep(time.Millisecond)
			reclaimedInBurst = 0
		} else {
			runtime.Gosched()
		}
	}
	if applied < s.Capacity {
		c.growth = cacheBudgetUnit
		c.setWatermark(cacheBudgetUnit)
		c.lastDemand = beforeDemand
		c.pendingGrowth = false
	}
	return s.Capacity - applied
}

// Physical pages are already returned when the node sees a lower baseline.
// Keep the last confirmed charge if the backing hit a fatal rollback error.
func (c *elasticCacheController) reportReclaim(seq uint64) error {
	s := c.cache.BudgetSnapshot()
	current := s.Capacity
	if s.Resident > current {
		current = c.hooks.CacheReservation()
	}
	_, _, _, _, err := c.hooks.requestCacheBudget(current, 0, seq)
	return err
}

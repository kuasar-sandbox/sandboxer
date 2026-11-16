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
	cacheBudgetUnit           = uint64(8 << 20)
	cacheMaxBatch             = uint64(32 << 20)
	cachePhysicalBatch        = uint64(1 << 20)
	cachePhysicalReclaimBatch = uint64(256 << 10)
	cacheAge                  = 10 * time.Second
	cacheIdleTick             = time.Second
	cacheRetryWait            = 250 * time.Millisecond
	// Reclaim in 8MiB physical batches so foreground cache I/O can run
	// between batches rather than blocking under one large LRU scan.
	cacheActiveReclaimBatches = 32
	cacheIdleReclaimBatches   = 4
)

// NewCacheWithBudget returns the existing fixed cache when the controller is
// absent or predates cache grants. On negotiated nodes it acquires the initial
// host memory grant before touching a single payload page, and returns a
// lifetime-bound stop function for the autonomous cache budget loop.
func NewCacheWithBudget(ctx context.Context, hooks *ControllerHooks, maximum, maxDirty uint64) (*diffstore.Cache, func(), error) {
	if hooks == nil || !hooks.CacheSupported() {
		cache, err := diffstore.NewCache(maximum, maxDirty)
		return cache, func() {}, err
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
	cache, err := diffstore.NewElasticCache(maximum, maxDirty, first)
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
		wake: make(chan struct{}, 1), growth: first,
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
	cache       *diffstore.Cache
	hooks       *ControllerHooks
	ctx         context.Context
	wake        chan struct{}
	growth      uint64
	growAfter   time.Time
	shrinkAfter time.Time
}

func (c *elasticCacheController) setWatermark(step uint64) {
	capacity := c.cache.BudgetSnapshot().Capacity
	margin := min(capacity/2, max(uint64(4096), step/2))
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

func (c *elasticCacheController) adjust(tick bool) {
	if c.ctx.Err() != nil {
		return
	}
	s := c.cache.BudgetSnapshot()
	if tick {
		_ = c.cache.AgeClean(cacheAge)
		s = c.cache.BudgetSnapshot()
	}
	minimum := min(s.Maximum, cacheBudgetUnit)
	// Reclaim is a target, not a signal that the node has already obtained
	// those bytes. The active cache keeps ownership until physical release
	// succeeds and the node acknowledges the new baseline.
	if wanted := c.hooks.CacheTarget(); wanted > 0 {
		target := max(minimum, wanted)
		for step := 0; step < cacheActiveReclaimBatches && s.Capacity > target; step++ {
			if !c.shrinkTo(target, s.Capacity) {
				break
			}
			s = c.cache.BudgetSnapshot()
		}
		// Node-directed reclaimed cache should not immediately regrow.
		return
	}
	// Idle shrink can reuse a just expired entry until a whole unit can be
	// returned. Keep one unit of headroom around nonexpired hot data.
	if tick && time.Now().After(c.shrinkAfter) && s.Capacity > minimum {
		shrank := false
		for step := 0; step < cacheIdleReclaimBatches && s.Capacity > minimum; step++ {
			reclaimable := s.Capacity - min(s.Capacity, s.Used-s.Expired)
			if reclaimable < cacheBudgetUnit {
				break
			}
			next := s.Capacity - min(cacheBudgetUnit, s.Capacity-minimum)
			if s.Used-s.Expired+min(cacheBudgetUnit/2, next/2) > next ||
				!c.shrinkTo(next, s.Capacity) {
				break
			}
			shrank = true
			s = c.cache.BudgetSnapshot()
		}
		if shrank {
			return
		}
	}
	if time.Now().Before(c.growAfter) || s.Capacity >= s.Maximum || c.hooks.CacheTarget() != 0 {
		return
	}
	// Failed grants never consume emergency memory or force Guest memory
	// growth. Capacity exhausted by clean churn is also a growth signal.
	margin := min(s.Capacity/2, max(uint64(4096), c.growth/2))
	if s.Used < s.Capacity-margin {
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
	// Grant in large batches, prepare physical pages in small batches to
	// minimize lock hold times on foreground I/O.
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
		// Never return physical capacity already used by a foreground writer.
		if c.cache.Err() == nil && c.cache.BudgetSnapshot().Resident <= applied {
			_, _, _, _, _ = c.hooks.RequestCacheBudget(applied, 0)
		}
		c.growth = max(cachePhysicalBatch, applied-s.Capacity)
		c.setWatermark(c.growth)
		c.growAfter = time.Now().Add(time.Second)
		return
	}
	c.growth = delta
	c.setWatermark(delta)
	c.shrinkAfter = time.Now().Add(2 * cacheAge)
}

func (c *elasticCacheController) shrinkTo(target, prior uint64) bool {
	if target >= prior || c.ctx.Err() != nil {
		return false
	}
	// Keep the lock-free I/O path alive and bound the amount of cache-list
	// work done per invocation.
	next := max(target, prior-cacheBudgetUnit)
	applied := prior
	for applied > next && c.ctx.Err() == nil {
		step := max(next, applied-cachePhysicalReclaimBatch)
		ok, err := c.cache.TryResizeCapacity(step)
		if err != nil || !ok {
			break
		}
		applied = step
		runtime.Gosched()
	}
	if applied != next {
		// A partly reclaimed cache remains charged at its previous node
		// grant until a later successful complete batch.
		return false
	}
	if s := c.cache.BudgetSnapshot(); s.Resident > next {
		return false
	}
	// A failed / lost ACK retains the smaller safe local baseline in
	// ControllerHooks; StateSync later reconciles node overcharge.
	_, _, _, _, _ = c.hooks.RequestCacheBudget(next, 0)
	c.growth = cacheBudgetUnit
	c.setWatermark(cacheBudgetUnit)
	c.growAfter = time.Now().Add(2 * time.Second)
	c.shrinkAfter = time.Now().Add(cacheIdleTick)
	return true
}

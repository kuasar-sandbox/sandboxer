package resctl

import (
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

const cacheReclaimProbeInterval = 30 * time.Second

func cacheReclaimOpportunity(s diffstore.BudgetSnapshot) uint64 {
	minimum := min(s.Maximum, cacheBudgetUnit)
	if s.Capacity <= minimum {
		return 0
	}
	idle := s.Capacity - min(s.Capacity, s.Used) + s.Expired
	wanted := min(s.Capacity-minimum, idle)
	return wanted / cacheBudgetUnit * cacheBudgetUnit
}

// Counters are sampled in the background. Neither elapsed time, a new intent
// ID, nor an unrelated demand counter increment alone revives a zero result.
func newCacheReclaimObservation(old, current diffstore.BudgetSnapshot) bool {
	oldFree := old.Capacity - min(old.Capacity, old.Used)
	free := current.Capacity - min(current.Capacity, current.Used)
	return current.Capacity > old.Capacity || free > oldFree ||
		current.Expired > old.Expired || current.Dirty < old.Dirty
}

func sameCacheReclaimIntent(a, b *resource.CacheReclaimIntent) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// adjustCoordinatedReclaim returns true while a finite task or its completion
// owns the cache worker. Physical work never runs under a reservation RPC lock.
func (c *elasticCacheController) adjustCoordinatedReclaim(s diffstore.BudgetSnapshot, tick bool) bool {
	state := c.hooks.cacheReclaimState()
	if state.Done != nil {
		_ = c.hooks.syncCacheReclaim()
		return true
	}
	if state.Task != nil {
		task := *state.Task
		wanted := task.MaxBytes
		if task.Target != 0 {
			wanted = min(wanted, s.Capacity-min(s.Capacity, task.Target))
		}
		released := c.reclaimBudget(wanted)
		after := c.cache.BudgetSnapshot()
		current := after.Capacity
		if after.Resident > current {
			current = c.hooks.CacheReservation()
			released = 0
		}
		next := resource.CloneCacheReclaimIntent(state.Intent)
		if next != nil {
			next.Bytes = min(next.Bytes, cacheReclaimOpportunity(after))
			if next.Bytes == 0 {
				next = nil
			}
		}
		if released == 0 {
			next = nil
			c.reclaimZero = true
			if c.reclaimObservation == 0 {
				c.reclaimObservation = 1
			}
			c.reclaimProbeAfter = time.Now().Add(cacheReclaimProbeInterval)
		} else {
			c.reclaimZero = false
			// A yield may finish before this worker's first maintenance sample,
			// or writeback may expose new clean supply during a finite attempt.
			// Publish that genuine opportunity in the completion itself so the
			// node cannot mistake the next-heartbeat gap for autonomous failure.
			if !c.reclaimObservedSet || newCacheReclaimObservation(c.reclaimObserved, after) ||
				task.Source == resource.CacheReclaimAutonomous && task.IntentID == c.reclaimProbeIntent {
				c.reclaimObservation++
				if wanted := cacheReclaimOpportunity(after); wanted != 0 {
					c.reclaimIntentSeq++
					next = &resource.CacheReclaimIntent{ID: c.reclaimIntentSeq, Bytes: wanted, Observation: c.reclaimObservation}
				}
			}
		}
		if task.IntentID == c.reclaimProbeIntent {
			c.reclaimProbeIntent = 0
		}
		// A successful physical return is not a new observation that replenishes
		// already consumed opportunity. Keep only the original unissued balance.
		c.reclaimObserved, c.reclaimObservedSet = after, true
		if err := c.hooks.completeCacheReclaim(task, current, next); err == nil {
			_ = c.hooks.syncCacheReclaim()
		}
		return true
	}

	intent := resource.CloneCacheReclaimIntent(state.Intent)
	wanted := cacheReclaimOpportunity(s)
	newObservation := !c.reclaimObservedSet || newCacheReclaimObservation(c.reclaimObserved, s)
	if tick {
		if newObservation {
			c.reclaimObservation++
			c.reclaimZero = false
		}
		c.reclaimObserved, c.reclaimObservedSet = s, true
		if wanted != 0 && newObservation {
			c.reclaimIntentSeq++
			c.reclaimProbeIntent = 0
			intent = &resource.CacheReclaimIntent{ID: c.reclaimIntentSeq, Bytes: wanted, Observation: c.reclaimObservation}
		} else if wanted != 0 && intent == nil && c.reclaimZero && !time.Now().Before(c.reclaimProbeAfter) {
			c.reclaimIntentSeq++
			c.reclaimProbeIntent = c.reclaimIntentSeq
			intent = &resource.CacheReclaimIntent{ID: c.reclaimIntentSeq, Bytes: min(wanted, cacheBudgetUnit), Observation: c.reclaimObservation, Probe: true}
		}
	}
	if intent != nil {
		intent.Bytes = min(intent.Bytes, wanted)
		if intent.Bytes == 0 {
			intent = nil
		}
	}
	changed := !sameCacheReclaimIntent(intent, state.Intent)
	if changed && !c.hooks.setCacheReclaimIntent(intent, state.LastSeq) {
		return true // a heartbeat accepted a task after the sampling snapshot
	}
	if tick || changed {
		_ = c.hooks.syncCacheReclaim()
	}
	state = c.hooks.cacheReclaimState()
	return state.Task != nil || state.Done != nil
}

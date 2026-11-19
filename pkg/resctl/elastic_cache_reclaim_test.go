package resctl

import (
	"context"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

const coordinatedTestRun = "0123456789abcdef0123456789abcdef"

func disconnectedCoordinatedCache(t *testing.T, capacity uint64) (*elasticCacheController, *ControllerHooks) {
	t.Helper()
	cache, err := diffstore.NewElasticCache(64<<20, 32<<20, capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	h := &ControllerHooks{
		opts:        ControllerHookOptions{SocketPath: "disconnected"},
		lifetimeCtx: context.Background(), admitted: true, cacheSupported: true,
		cacheCoordinated: true, cacheMaximum: 64 << 20, cacheReservation: capacity,
		cacheReclaim: resource.CacheReclaimState{RunID: coordinatedTestRun, Current: capacity},
		cacheWake:    make(chan struct{}, 1), reconnectWake: make(chan struct{}, 1),
	}
	c := &elasticCacheController{cache: cache, hooks: h, ctx: context.Background(),
		wake: make(chan struct{}, 1), growth: cacheBudgetUnit, idleAfter: time.Second}
	return c, h
}

func TestCoordinatedCacheRequiresPermitAndRetainsCompletion(t *testing.T) {
	const mib = uint64(1 << 20)
	c, h := disconnectedCoordinatedCache(t, 12*mib)
	c.adjust(true)
	intent := h.cacheReclaimState().Intent
	if intent == nil || intent.Bytes != 8*mib || c.cache.BudgetSnapshot().Capacity != 12*mib {
		t.Fatalf("unapproved idle reclaim: intent=%+v cache=%+v", intent, c.cache.BudgetSnapshot())
	}
	for range 3 {
		c.adjust(true)
	}
	if got := h.cacheReclaimState().Intent; got == nil || *got != *intent {
		t.Fatalf("identical observation replenished intent: before=%+v after=%+v", intent, got)
	}
	task := resource.CacheReclaimTask{RunID: coordinatedTestRun, Seq: 1, Source: resource.CacheReclaimAutonomous, IntentID: intent.ID, MaxBytes: 4 * mib, Target: 8 * mib}
	if err := h.acceptCacheReclaimResponse(&task, 0); err != nil {
		t.Fatal(err)
	}
	c.adjust(false)
	state := h.cacheReclaimState()
	if state.Done == nil || state.Done.Current != 8*mib || state.Done.Intent == nil || state.Done.Intent.Bytes != 4*mib || h.CacheReservation() != 8*mib {
		t.Fatalf("missing physically completed outbox: %+v", state)
	}
	if local := h.localState(); local.cacheReclaim.Done == nil || local.cacheReservation != 8*mib {
		t.Fatalf("recovery snapshot lost return: %+v", local)
	}
	if _, _, _, _, err := h.RequestCacheBudget(8*mib, 4*mib); err == nil {
		t.Fatal("growth crossed an unacknowledged completion")
	}
	for range 3 {
		if err := h.acceptCacheReclaimResponse(&task, 0); err != nil {
			t.Fatal(err)
		}
		c.adjust(false)
	}
	if c.cache.BudgetSnapshot().Capacity != 8*mib {
		t.Fatal("lost ACK repeated physical reclaim")
	}
	next := task
	next.Seq++
	next.Target = 4 * mib
	if err := h.acceptCacheReclaimResponse(&next, 1); err != nil {
		t.Fatal(err)
	}
	c.adjust(false)
	if s := h.cacheReclaimState(); s.Done == nil || s.Done.Current != 4*mib || s.Done.Intent != nil {
		t.Fatalf("finite continuation did not end: %+v", s)
	}
	if err := h.acceptCacheReclaimResponse(nil, 2); err != nil {
		t.Fatal(err)
	}
	c.adjust(true)
	if s := h.cacheReclaimState(); s.Task != nil || s.Done != nil || s.Intent != nil || s.LastSeq != 2 {
		t.Fatalf("consumed opportunity resurrected: %+v", s)
	}
}

func TestCoordinatedCacheRejectsOverlappingAndChangedAuthority(t *testing.T) {
	_, h := disconnectedCoordinatedCache(t, 12<<20)
	task := resource.CacheReclaimTask{RunID: coordinatedTestRun, Seq: 1, Source: resource.CacheReclaimYield, MaxBytes: 4 << 20, Target: 8 << 20}
	if err := h.acceptCacheReclaimResponse(&task, 0); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*resource.CacheReclaimTask){
		func(t *resource.CacheReclaimTask) { t.Seq++ },
		func(t *resource.CacheReclaimTask) { t.MaxBytes *= 2 },
		func(t *resource.CacheReclaimTask) { t.RunID = "other" },
		func(t *resource.CacheReclaimTask) { t.Source = "force" },
		func(t *resource.CacheReclaimTask) { t.Target = 0 },
		func(t *resource.CacheReclaimTask) { t.Target++ },
		func(t *resource.CacheReclaimTask) { t.Target += 4 << 20 },
	} {
		bad := task
		change(&bad)
		if err := h.acceptCacheReclaimResponse(&bad, 0); err == nil {
			t.Fatalf("accepted bad task: %+v", bad)
		}
	}
	if err := h.acceptCacheReclaimResponse(nil, 0); err != nil {
		t.Fatal(err)
	}
	if got := h.cacheReclaimState().Task; got == nil || *got != task {
		t.Fatal("empty node response canceled execution authority")
	}
}

func TestCoordinatedCacheZeroEndsIntentAndRequiresNewObservation(t *testing.T) {
	c, h := disconnectedCoordinatedCache(t, 8<<20)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	c.ctx = cancelled
	task := resource.CacheReclaimTask{RunID: coordinatedTestRun, Seq: 1, Source: resource.CacheReclaimYield, MaxBytes: 4 << 20, Target: 4 << 20}
	if err := h.acceptCacheReclaimResponse(&task, 0); err != nil {
		t.Fatal(err)
	}
	c.adjustCoordinatedReclaim(c.cache.BudgetSnapshot(), false)
	if state := h.cacheReclaimState(); state.Done == nil || state.Done.Current != 8<<20 || state.Done.Intent != nil || !c.reclaimZero {
		t.Fatalf("zero attempt did not finish: %+v", state)
	}
	if err := h.acceptCacheReclaimResponse(nil, 1); err != nil {
		t.Fatal(err)
	}
	c.ctx = context.Background()
	base := diffstore.BudgetSnapshot{Capacity: 16 << 20, Maximum: 64 << 20, Used: 8 << 20, Dirty: 4 << 20, Demand: 10}
	changedDemand := base
	changedDemand.Demand++
	if newCacheReclaimObservation(base, changedDemand) {
		t.Fatal("demand counter alone revived a fruitless observation")
	}
	for _, change := range []func(*diffstore.BudgetSnapshot){
		func(s *diffstore.BudgetSnapshot) { s.Capacity += 4 << 20 },
		func(s *diffstore.BudgetSnapshot) { s.Used -= 4 << 20 },
		func(s *diffstore.BudgetSnapshot) { s.Expired += 4 << 20 },
		func(s *diffstore.BudgetSnapshot) { s.Dirty -= 4 << 20 },
	} {
		next := base
		change(&next)
		if !newCacheReclaimObservation(base, next) {
			t.Fatalf("new reclaimable state ignored: %+v", next)
		}
	}
	// A constant sample does not become active supply merely because time
	// advances. Only a finite, explicitly marked revalidation may be offered.
	c.reclaimObserved, c.reclaimObservedSet, c.reclaimObservation = base, true, 2
	c.reclaimProbeAfter = time.Now().Add(time.Hour)
	c.adjustCoordinatedReclaim(base, true)
	if h.cacheReclaimState().Intent != nil {
		t.Fatal("unchanged zero candidate became active supply")
	}
	c.reclaimProbeAfter = time.Now().Add(-time.Second)
	c.adjustCoordinatedReclaim(base, true)
	if intent := h.cacheReclaimState().Intent; intent == nil || !intent.Probe || intent.Bytes != cacheBudgetUnit || intent.Observation != 2 {
		t.Fatalf("missing finite revalidation: %+v", intent)
	}
}

func TestCoordinatedCacheGrowthNeedsPhysicalConfirmation(t *testing.T) {
	_, h := disconnectedCoordinatedCache(t, 12<<20)
	h.mu.Lock()
	h.cacheReservation = 20 << 20 // accepted grant, worker still has 12 MiB
	h.mu.Unlock()
	state := h.localState()
	if state.cacheReservation != 20<<20 || state.cacheReclaim.Current != 12<<20 {
		t.Fatalf("grant was mistaken for physical completion: %+v", state)
	}
	task := resource.CacheReclaimTask{RunID: coordinatedTestRun, Seq: 1, Source: resource.CacheReclaimYield, MaxBytes: 4 << 20, Target: 12 << 20}
	if err := h.acceptCacheReclaimResponse(&task, 0); err == nil {
		t.Fatal("accepted a task against unconfirmed growth")
	}
	// A disconnected, physically completed partial rollback lowers the safe
	// grant, but only the worker can confirm the newly allocated portion.
	_, _, _, _, _ = h.RequestCacheBudget(16<<20, 0)
	if state := h.localState(); state.cacheReservation != 16<<20 || state.cacheReclaim.Current != 12<<20 {
		t.Fatalf("rollback invented physical pages: %+v", state)
	}
	h.confirmCacheReclaimCurrent(16 << 20)
	if err := h.acceptCacheReclaimResponse(&task, 0); err != nil {
		t.Fatalf("confirmed physical capacity cannot accept task: %v", err)
	}
}

func TestCoordinatedCacheSuccessfulProbeRestoresSupply(t *testing.T) {
	c, h := disconnectedCoordinatedCache(t, 12<<20)
	c.reclaimObserved, c.reclaimObservedSet = c.cache.BudgetSnapshot(), true
	c.reclaimObservation, c.reclaimZero = 1, true
	c.reclaimProbeAfter = time.Now().Add(-time.Second)
	c.adjust(true)
	probe := h.cacheReclaimState().Intent
	if probe == nil || !probe.Probe {
		t.Fatalf("no probe: %+v", probe)
	}
	task := resource.CacheReclaimTask{RunID: coordinatedTestRun, Seq: 1, Source: resource.CacheReclaimAutonomous, IntentID: probe.ID, MaxBytes: 4 << 20, Target: 8 << 20}
	if err := h.acceptCacheReclaimResponse(&task, 0); err != nil {
		t.Fatal(err)
	}
	c.adjust(false)
	done := h.cacheReclaimState().Done
	if done == nil || done.Intent == nil || done.Intent.Probe || done.Intent.Observation <= probe.Observation || done.Intent.Bytes != 4<<20 {
		t.Fatalf("successful physical revalidation did not restore finite supply: %+v", done)
	}
}

package resctl

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

// The budget server charges the exact grants and actual physical returns.
func newIdlePolicyHooks(t *testing.T, onBudget func(resource.Message)) *ControllerHooks {
	t.Helper()
	var reservation uint64
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		switch req.Type {
		case resource.TypeAdmit:
			reservation = req.CacheInitial
			return &resource.Message{
				Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted,
				Token: "idle-test", GrantedInitialAlloc: req.StartupBudgetMemory,
				GrantedInitialCache: req.CacheInitial,
				ClientFeatures:      []string{resource.FeatureCacheBudgetV1},
			}, false
		case resource.TypeRequestCacheBudget:
			if req.CacheCurrent > reservation || req.CacheMaximum != 64<<20 {
				return &resource.Message{Type: resource.TypeError, Msg: "invalid cache request"}, false
			}
			reservation = req.CacheCurrent + req.CacheRequested
			if onBudget != nil {
				onBudget(req)
			}
			return &resource.Message{Type: resource.TypeCacheBudgetResponse,
				CacheReservation: reservation, CacheGranted: req.CacheRequested}, false
		default:
			return &resource.Message{Type: resource.TypeAck}, false
		}
	})
	cfg := testControllerConfig(t, server.sock)
	cfg.Resources.DiffCOW.CacheSize = "64MiB"
	return newAdmittedHooks(t, server, cfg, 0)
}

// An inactivity threshold changes only the idle statistic; the normal
// asynchronous worker decides whether and how many physical pages to return.
func TestElasticCacheIdleAfterTriggersOrdinaryReclaim(t *testing.T) {
	for _, tc := range []struct {
		name      string
		idleAfter time.Duration
		auto      bool
	}{
		{"short-idle", 50 * time.Millisecond, true},
		{"long-idle", 30 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const mib = uint64(1 << 20)
			h := newIdlePolicyHooks(t, nil)
			ctx := context.Background()
			cache, stop, err := NewCacheWithBudget(ctx, h, 64*mib, 32*mib, tc.idleAfter)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { stop(); _ = cache.Close() }()
			disk, err := diffstore.OpenDisk(filepath.Join(t.TempDir(), "idle.diff"), nil,
				diffstore.DiffInit{CreateSize: int64(64 * mib)}, diffstore.WithCache(cache))
			if err != nil {
				t.Fatal(err)
			}
			defer disk.Close()
			buf := bytes.Repeat([]byte{0x6d}, int(mib))
			for i := 0; i < 20; i++ {
				if _, err = disk.WriteAt(buf, int64(i)*int64(mib)); err != nil {
					t.Fatal(err)
				}
			}
			if err := disk.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for cache.BudgetSnapshot().Capacity < 16*mib && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if s := cache.BudgetSnapshot(); s.Capacity < 16*mib {
				t.Fatalf("no growth: %+v", s)
			}
			if tc.auto {
				deadline = time.Now().Add(5 * time.Second)
				for cache.BudgetSnapshot().Capacity > 4*mib && time.Now().Before(deadline) {
					time.Sleep(40 * time.Millisecond)
				}
				if s := cache.BudgetSnapshot(); s.Capacity != 4*mib || s.Resident != 4*mib {
					t.Fatalf("idle clean not physically returned: %+v", s)
				}
				if _, err = disk.ReadAt(buf, 0); err != nil {
					t.Fatal(err)
				}
				if buf[0] != 0x6d {
					t.Fatal("reclaim changed logical data")
				}
			} else {
				before := cache.BudgetSnapshot()
				time.Sleep(1500 * time.Millisecond)
				if after := cache.BudgetSnapshot(); after.Expired != 0 || after.Used < before.Used {
					// Unused preallocated buffers may return early, but no
					// active Clean page may be expired or evicted by idle logic.
					t.Fatalf("active clean was prematurely reclaimed: before=%+v after=%+v", before, after)
				}
			}
		})
	}
}

func TestElasticCacheReclaimCompletesWithZeroRelease(t *testing.T) {
	const miB = uint64(1 << 20)
	done := make(chan resource.Message, 4)
	h := newIdlePolicyHooks(t, func(req resource.Message) {
		if req.CacheReclaimDone != 0 {
			done <- req
		}
	})
	cache, stop, err := NewCacheWithBudget(context.Background(), h, 64*miB, 32*miB, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(); _ = cache.Close() }()
	if snap := cache.BudgetSnapshot(); snap.Capacity != 4*miB {
		t.Fatalf("initial cache %+v", snap)
	}
	// A node reclaim request is advisory. No pages can be returned below
	// the minimum 4 MiB, but the request must still complete.
	h.setCacheTarget(2*miB, 17)
	select {
	case ack := <-done:
		if ack.CacheReclaimDone != 17 || ack.CacheCurrent != 4*miB || ack.CacheRequested != 0 {
			t.Fatalf("wrong zero-byte advisory completion: %+v", ack)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("zero-byte reclaim never acknowledged")
	}
	if snap := cache.BudgetSnapshot(); snap.Capacity != 4*miB {
		t.Fatalf("zero-byte reclaim shrank below minimum: %+v", snap)
	}
}

// A page allocation can queue another wake while the preceding grant is in
// flight. Only an idle observation can declare its unused headroom idle.
func TestElasticCacheQueuedIOWakePreservesGrant(t *testing.T) {
	h := newIdlePolicyHooks(t, nil)
	ctx := context.Background()
	cache, err := diffstore.NewElasticCache(64<<20, 32<<20, cacheBudgetUnit)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	disk, err := diffstore.OpenDisk(filepath.Join(t.TempDir(), "queued-wake.diff"), nil,
		diffstore.DiffInit{CreateSize: 64 << 20}, diffstore.WithCache(cache))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	c := &elasticCacheController{
		cache: cache, hooks: h, ctx: ctx, wake: make(chan struct{}, 1),
		growth: cacheBudgetUnit, idleAfter: time.Hour,
	}
	c.setWatermark(c.growth)
	const miB = 1 << 20
	if _, err = disk.WriteAt(make([]byte, miB-4096), 0); err != nil {
		t.Fatal(err)
	}
	if len(c.wake) != 0 {
		t.Fatal("growth signaled before a quarter of the initial grant was used")
	}
	if _, err = disk.WriteAt(make([]byte, 4096), miB-4096); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.wake:
	default:
		t.Fatal("one MiB of demand did not wake the controller")
	}
	// Fill the initial grant before handling the wake. After the first
	// growth these same four MiB still exceed the new watermark.
	if _, err = disk.WriteAt(make([]byte, 3*miB), miB); err != nil {
		t.Fatal(err)
	}
	c.adjust(false)
	grant := cache.BudgetSnapshot().Capacity
	if grant != 12*miB || grant != h.CacheReservation() {
		t.Fatalf("first runtime grant: capacity=%d reservation=%d", grant, h.CacheReservation())
	}
	c.adjust(false)
	if got := cache.BudgetSnapshot().Capacity; got != grant {
		t.Fatalf("stale foreground wake returned new grant: got=%d want=%d", got, grant)
	}
	c.adjust(true)
	if s := cache.BudgetSnapshot(); s.Capacity != cacheBudgetUnit || s.Resident != s.Capacity || s.Capacity != h.CacheReservation() {
		t.Fatalf("idle observation failed to return free headroom: %+v reservation=%d", s, h.CacheReservation())
	}
	c.adjust(false)
	if cache.BudgetSnapshot().Capacity != cacheBudgetUnit {
		t.Fatal("stale wake regrew the grant without I/O")
	}
}

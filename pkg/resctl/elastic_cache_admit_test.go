package resctl

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestElasticCacheInitialGrantIsPartOfAdmit(t *testing.T) {
	const miB = uint64(1 << 20)
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		switch req.Type {
		case resource.TypeAdmit:
			if req.CacheInitial != 8*miB || req.CacheMaximum != 64*miB ||
				!resource.HasFeature(req.ClientFeatures, resource.FeatureCacheBudgetV1) {
				return &resource.Message{Type: resource.TypeError,
					Msg: fmt.Sprintf("unexpected cache admission contract: %+v", req)}, false
			}
			return &resource.Message{
				Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted,
				Token: "initial-grant-token", GrantedInitialAlloc: req.StartupBudgetMemory,
				GrantedInitialCache: req.CacheInitial,
				ClientFeatures:      []string{resource.FeatureCacheBudgetV1},
			}, false
		case resource.TypeRequestCacheBudget:
			return &resource.Message{Type: resource.TypeError, Msg: "initial cache was allocated twice"}, false
		case resource.TypeRelease:
			return &resource.Message{Type: resource.TypeAck}, false
		default:
			return &resource.Message{Type: resource.TypeAck}, false
		}
	})
	cfg := testControllerConfig(t, server.sock)
	cfg.Resources.DiffCOW.CacheSize = "64MiB"
	h := newAdmittedHooks(t, server, cfg, 0)
	if !h.CacheSupported() || h.CacheReservation() != 8*miB {
		t.Fatalf("cache not atomically granted at Admit: supported=%v reserved=%d",
			h.CacheSupported(), h.CacheReservation())
	}
	cache, stop, err := NewCacheWithBudget(context.Background(), h, 64*miB, 32*miB)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if got := cache.BudgetSnapshot().Resident; got != 8*miB {
		t.Fatalf("cache did not prepare initially granted pages: %d", got)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	// The test server's request capture is buffered, so an unexpected
	// post-admit cache grant is visible without a timing-sensitive read.
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case req := <-server.requests:
			if req.Type == resource.TypeRequestCacheBudget {
				t.Fatalf("initial grant redundantly requested after Admit: %+v", req)
			}
		case <-deadline:
			return
		}
	}
}

package resctl

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestCoordinatedCacheReconnectReplaysLostCompletion(t *testing.T) {
	const mib = uint64(1 << 20)
	var mu sync.Mutex
	var budget uint64
	var task *resource.CacheReclaimTask
	var ack uint64
	var dropped, recovered bool
	var doneCalls int
	features := []string{resource.FeatureCacheBudgetV1, resource.FeatureCacheReclaimV1}
	server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Type {
		case resource.TypeAdmit:
			run, err := resource.CacheReclaimRunFromFeatures(req.ClientFeatures)
			if err != nil || req.CacheReclaim == nil || req.CacheReclaim.RunID != run {
				return &resource.Message{Type: resource.TypeError, Msg: "missing coordinated initial state"}, false
			}
			budget = req.CacheInitial
			return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted,
				Token: "before", GrantedInitialAlloc: req.StartupBudgetMemory, GrantedInitialCache: budget,
				ClientFeatures: features}, false
		case resource.TypeRequestCacheBudget:
			budget = req.CacheCurrent + req.CacheRequested
			return &resource.Message{Type: resource.TypeCacheBudgetResponse, CacheReservation: budget, CacheGranted: req.CacheRequested}, false
		case resource.TypeCacheReclaimUpdate:
			if req.CacheReclaim == nil {
				return &resource.Message{Type: resource.TypeError, Msg: "missing reclaim state"}, false
			}
			state := req.CacheReclaim
			if state.Done != nil {
				doneCalls++
				if state.Done.Task.Seq > ack {
					budget = state.Done.Current
					ack = state.Done.Task.Seq
					task = nil
				}
				if ack == 1 && !dropped {
					if state.Done.Intent == nil || state.Done.Intent.Bytes != 4*mib {
						return &resource.Message{Type: resource.TypeError, Msg: "completion lost finite continuation"}, false
					}
					task = &resource.CacheReclaimTask{RunID: state.RunID, Seq: 2, Source: resource.CacheReclaimAutonomous,
						IntentID: state.Done.Intent.ID, MaxBytes: 4 * mib, Target: budget - 4*mib}
					dropped = true // accepted return and issued continuation, reply lost
					return nil, true
				}
			} else if state.Intent != nil && ack == 0 && task == nil {
				task = &resource.CacheReclaimTask{RunID: state.RunID, Seq: 1, Source: resource.CacheReclaimAutonomous,
					IntentID: state.Intent.ID, MaxBytes: 4 * mib, Target: budget - 4*mib}
			}
			return &resource.Message{Type: resource.TypeAck, CacheReservation: budget, CacheReclaimTask: task, CacheReclaimAck: ack}, false
		case resource.TypeStateSync:
			if req.CacheReclaim == nil || req.CacheReclaim.Done == nil || req.CacheReclaim.Done.Task.Seq != 1 ||
				req.CacheReclaim.LastSeq != 1 || req.AppliedCacheReservation != 8*mib || req.CacheReclaim.Done.Current != 8*mib {
				return &resource.Message{Type: resource.TypeError, Msg: "state sync lost accepted task/completed outbox"}, false
			}
			recovered = true
			return &resource.Message{Type: resource.TypeAck, Token: "after", NewAllocatable: req.AppliedAllocatableMemory,
				CacheReservation: budget, CacheReclaimTask: task, CacheReclaimAck: ack, ClientFeatures: features}, false
		default:
			return &resource.Message{Type: resource.TypeAck}, false
		}
	})
	cfg := testControllerConfig(t, server.sock)
	cfg.Resources.DiffCOW.CacheSize = "64MiB"
	h := newAdmittedHooks(t, server, cfg, 0)
	if !h.CacheCoordinated() {
		t.Fatal("coordination not negotiated")
	}
	if _, _, _, _, err := h.RequestCacheBudget(4*mib, 8*mib); err != nil {
		t.Fatal(err)
	}
	cache, err := diffstore.NewElasticCache(64*mib, 32*mib, 12*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	h.confirmCacheReclaimCurrent(12 * mib)
	c := &elasticCacheController{cache: cache, hooks: h, ctx: context.Background(), wake: make(chan struct{}, 1), idleAfter: time.Second}
	c.adjust(true)
	c.adjust(false)
	deadline := time.Now().Add(3 * time.Second)
	for {
		state := h.cacheReclaimState()
		if h.Connected() && state.Task != nil && state.Task.Seq == 2 && state.Done == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lost response did not recover: %+v connected=%v", state, h.Connected())
		}
		time.Sleep(time.Millisecond)
	}
	if cache.BudgetSnapshot().Capacity != 8*mib {
		t.Fatal("reconnect replayed the first physical task")
	}
	c.adjust(false)
	mu.Lock()
	gotBudget, gotDone, gotRecovered := budget, doneCalls, recovered
	mu.Unlock()
	if gotBudget != 4*mib || cache.BudgetSnapshot().Capacity != 4*mib || gotDone != 2 || !gotRecovered {
		t.Fatalf("bad recovered completion: node=%d local=%d completions=%d recovered=%v", gotBudget, cache.BudgetSnapshot().Capacity, gotDone, gotRecovered)
	}
}

func TestCacheCoordinationModeIsStickyAcrossReconnect(t *testing.T) {
	for _, coordinated := range []bool{false, true} {
		t.Run(fmt.Sprintf("coordinated-%v", coordinated), func(t *testing.T) {
			syncSeen := make(chan resource.Message, 8)
			var budget uint64
			var mu sync.Mutex
			server := startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
				mu.Lock()
				defer mu.Unlock()
				switch req.Type {
				case resource.TypeAdmit:
					budget = req.CacheInitial
					features := []string{resource.FeatureCacheBudgetV1}
					if coordinated {
						features = append(features, resource.FeatureCacheReclaimV1)
					}
					return &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "old",
						GrantedInitialAlloc: req.StartupBudgetMemory, GrantedInitialCache: budget, ClientFeatures: features}, false
				case resource.TypeStateSync:
					select {
					case syncSeen <- req:
					default:
					}
					// A coordinated run must refuse this downgrade. A legacy run
					// must not send coordination state or upgrade merely from lease support.
					return &resource.Message{Type: resource.TypeAck, Token: "new", NewAllocatable: req.AppliedAllocatableMemory,
						CacheReservation: budget, ClientFeatures: []string{resource.FeatureCacheBudgetV1}}, false
				default:
					return &resource.Message{Type: resource.TypeAck}, false
				}
			})
			cfg := testControllerConfig(t, server.sock)
			h := newAdmittedHooks(t, server, cfg, 0)
			h.sessionMu.Lock()
			h.markDisconnectedLocked(fmt.Errorf("test reconnect"))
			h.sessionMu.Unlock()
			select {
			case req := <-syncSeen:
				if (req.CacheReclaim != nil) != coordinated {
					t.Fatalf("wrong mode in state sync: %+v", req.CacheReclaim)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no state sync")
			}
			deadline := time.Now().Add(time.Second)
			for !coordinated && !h.Connected() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			// Serialize after the response-processing path, rather than assuming
			// the server writing a reply means the client has already applied it.
			h.sessionMu.Lock()
			connected, mode := h.Connected(), h.CacheCoordinated()
			h.sessionMu.Unlock()
			if mode != coordinated || connected == coordinated {
				t.Fatalf("unsafe mode change: negotiated=%v mode=%v connected=%v", coordinated, mode, connected)
			}
		})
	}
}

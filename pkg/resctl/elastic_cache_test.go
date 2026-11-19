package resctl

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestElasticCacheControllerGrowReclaimAndReadback(t *testing.T) {
	const mib = 1 << 20
	root := t.TempDir()
	socket := filepath.Join(root, "budget.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var nodeBudget uint64
	var nodeTarget uint64
	var rpcCount atomic.Int64
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			req, err := resource.ReadMessage(conn)
			if err != nil {
				return
			}
			var resp *resource.Message
			switch req.Type {
			case resource.TypeAdmit:
				resp = &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "cache-test-token",
					ClientFeatures: []string{resource.FeatureCacheBudgetV1}}
			case resource.TypeRequestCacheBudget:
				rpcCount.Add(1)
				mu.Lock()
				if req.CacheCurrent > nodeBudget || req.CacheMaximum != 64*mib {
					resp = &resource.Message{Type: resource.TypeError, Msg: "invalid cache baseline"}
				} else {
					nodeBudget = req.CacheCurrent
					granted := req.CacheRequested
					if nodeBudget+granted > 64*mib {
						granted = 64*mib - nodeBudget
					}
					nodeBudget += granted
					target := nodeTarget
					if target != 0 && nodeBudget <= target {
						nodeTarget = 0
						target = 0
					}
					resp = &resource.Message{Type: resource.TypeCacheBudgetResponse,
						CacheGranted: granted, CacheReservation: nodeBudget, CacheReclaimTarget: target}
				}
				mu.Unlock()
			default:
				resp = &resource.Message{Type: resource.TypeError, Msg: "unexpected test rpc"}
			}
			if err := resource.WriteMessage(conn, resp); err != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &resource.Client{SocketPath: socket}
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	defer func() { client.Close(); <-done }()
	_, err = client.Admit(resource.AdmitParams{SandboxID: "test", CacheMaximum: 64 * mib,
		ClientFeatures: []string{resource.FeatureCacheBudgetV1}})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &ControllerHooks{
		opts:   ControllerHookOptions{SocketPath: socket, SandboxID: "test"},
		client: client, lifetimeCtx: ctx, admitted: true, connected: true,
		cacheSupported: true, cacheMaximum: 64 * mib, cacheWake: make(chan struct{}, 1),
		reconnectWake: make(chan struct{}, 1),
	}
	cache, stop, err := NewCacheWithBudget(ctx, hooks, 64*mib, 32*mib, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(); _ = cache.Close() }()
	if got := cache.BudgetSnapshot().Resident; got != 4*mib {
		t.Fatalf("start resident=%d", got)
	}
	disk, err := diffstore.OpenDisk(filepath.Join(root, "disk.diff"), nil,
		diffstore.DiffInit{CreateSize: 64 * mib}, diffstore.WithCache(cache))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	for i := 0; i < 24; i++ {
		buf := bytes.Repeat([]byte{byte(i + 1)}, mib)
		if _, err := disk.WriteAt(buf, int64(i*mib)); err != nil {
			t.Fatal(err)
		}
	}
	if err := disk.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for cache.BudgetSnapshot().Capacity < 16*mib && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	budget := cache.BudgetSnapshot()
	if budget.Capacity < 16*mib || budget.Resident != budget.Capacity {
		t.Fatalf("auto growth did not prepare physical pages: %+v", budget)
	}
	cache.AgeClean(time.Millisecond)
	mu.Lock()
	nodeTarget = 8 * mib
	mu.Unlock()
	hooks.setCacheTarget(8 * mib)
	deadline = time.Now().Add(10 * time.Second)
	for cache.BudgetSnapshot().Capacity > 8*mib && time.Now().Before(deadline) {
		time.Sleep(40 * time.Millisecond)
	}
	if got := cache.BudgetSnapshot(); got.Capacity > 8*mib || got.Resident != got.Capacity {
		t.Fatalf("node reclaim did not physically shrink: %+v", got)
	}
	// Physical shrink can precede its asynchronous node acknowledgement.
	// The node must eventually observe the exact lower physical baseline.
	var gotNode uint64
	ackDeadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		gotNode = nodeBudget
		mu.Unlock()
		current := cache.BudgetSnapshot().Capacity
		if gotNode == current && hooks.CacheReservation() == current {
			break
		}
		if time.Now().After(ackDeadline) {
			t.Fatalf("cache return not reconciled: physical=%d node=%d hooks=%d", current, gotNode, hooks.CacheReservation())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rpcCount.Load() >= 24*256 {
		t.Fatalf("cache called node for individual 4K pages: rpc=%d", rpcCount.Load())
	}
	// A returned or idle cache must not reacquire grants without new I/O.
	noIOBaseline := cache.BudgetSnapshot().Capacity
	time.Sleep(3 * time.Second)
	if got := cache.BudgetSnapshot(); got.Capacity > noIOBaseline {
		t.Fatalf("cache grew without new I/O: %+v", got)
	}
	read := make([]byte, mib)
	for i := 0; i < 24; i++ {
		if _, err := disk.ReadAt(read, int64(i*mib)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(read, bytes.Repeat([]byte{byte(i + 1)}, mib)) {
			t.Fatalf("wrong block %d after reclaim", i)
		}
	}
}

package resctl

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

// Opt-in >1GiB host-memory test, intended for both x86 and ARM validation
// nodes, not small developer VMs or every PR test suite.
func TestElasticCacheBurst768(t *testing.T) {
	if os.Getenv("KUASAR_CACHE_BURST_TEST") != "1" {
		t.Skip("set KUASAR_CACHE_BURST_TEST=1 on a memory-rich validation node")
	}
	const mib = 1 << 20
	const total = 768 * mib
	const maximum = 1024 * mib
	root := t.TempDir()
	sock := filepath.Join(root, "budget.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	var mu sync.Mutex
	var reserved uint64
	var requests int
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
			var res *resource.Message
			switch req.Type {
			case resource.TypeAdmit:
				res = &resource.Message{Type: resource.TypeAdmitResponse, Status: resource.StatusAdmitted, Token: "burst-cache-token",
					ClientFeatures: []string{resource.FeatureCacheBudgetV1}}
			case resource.TypeRequestCacheBudget:
				mu.Lock()
				requests++
				if req.CacheCurrent > reserved || req.CacheMaximum != maximum {
					res = &resource.Message{Type: resource.TypeError, Msg: "cache baseline"}
				} else {
					reserved = req.CacheCurrent
					add := min(req.CacheRequested, maximum-reserved)
					reserved += add
					res = &resource.Message{Type: resource.TypeCacheBudgetResponse, CacheGranted: add, CacheReservation: reserved}
				}
				mu.Unlock()
			default:
				res = &resource.Message{Type: resource.TypeError, Msg: "unexpected rpc"}
			}
			if resource.WriteMessage(conn, res) != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &resource.Client{SocketPath: sock}
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(); <-done }()
	_, err = client.Admit(resource.AdmitParams{SandboxID: "burst", CacheMaximum: maximum, ClientFeatures: []string{resource.FeatureCacheBudgetV1}})
	if err != nil {
		t.Fatal(err)
	}
	h := &ControllerHooks{
		opts: ControllerHookOptions{SocketPath: sock, SandboxID: "burst"}, client: client,
		lifetimeCtx: ctx, admitted: true, connected: true, cacheSupported: true, cacheMaximum: maximum,
		cacheWake: make(chan struct{}, 1), reconnectWake: make(chan struct{}, 1),
	}
	cache, stop, err := NewCacheWithBudget(ctx, h, maximum, maximum/2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(); _ = cache.Close() }()
	disk, err := diffstore.OpenDisk(filepath.Join(root, "disk.diff"), nil, diffstore.DiffInit{CreateSize: total},
		diffstore.WithCache(cache))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	buffer := make([]byte, mib)
	writeLatency := make([]time.Duration, 0, 768)
	start := time.Now()
	for i := 0; i < 768; i++ {
		for j := range buffer {
			buffer[j] = byte((i + j) % 251)
		}
		one := time.Now()
		if _, err := disk.WriteAt(buffer, int64(i*mib)); err != nil {
			t.Fatal(err)
		}
		writeLatency = append(writeLatency, time.Since(one))
	}
	writeElapsed := time.Since(start)
	sort.Slice(writeLatency, func(i, j int) bool { return writeLatency[i] < writeLatency[j] })
	t.Logf("1MiB foreground write latency p50=%v p99=%v max=%v",
		writeLatency[len(writeLatency)/2],
		writeLatency[len(writeLatency)*99/100], writeLatency[len(writeLatency)-1])
	if err := disk.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("burst write completed, cache (before sequential reads): %+v", cache.BudgetSnapshot())
	if snap := cache.BudgetSnapshot(); snap.Capacity != snap.Resident || snap.Used > snap.Capacity {
		t.Fatalf("invalid physical/cache accounting after burst: %+v", snap)
	}
	start = time.Now()
	for pass := 0; pass < 8; pass++ {
		for i := 0; i < 768; i++ {
			if _, err := disk.ReadAt(buffer, int64(i*mib)); err != nil {
				t.Fatal(err)
			}
			if buffer[0] != byte(i%251) || buffer[len(buffer)-1] != byte((i+len(buffer)-1)%251) {
				t.Fatalf("corrupted data pass=%d block=%d", pass, i)
			}
		}
		snap := cache.BudgetSnapshot()
		t.Logf("read pass %d: %v, cache resident=%dMiB used=%dMiB", pass, time.Since(start), snap.Resident/mib, snap.Used/mib)
		if snap.Capacity >= total && snap.Used >= total {
			break
		}
		start = time.Now()
	}
	s := cache.BudgetSnapshot()
	mu.Lock()
	n := requests
	finalGranted := reserved
	mu.Unlock()
	t.Logf("write+drain elapsed %v, logical use=%d MiB, cap=%d MiB, physical=%d MiB, node grant=%d MiB, rpc=%d",
		writeElapsed, s.Used/mib, s.Capacity/mib, s.Resident/mib, finalGranted/mib, n)
	if s.Capacity < total || s.Used < total || s.Resident < total {
		t.Fatalf("repeated hot reads failed to grow/retain the 768MiB working set: %+v", s)
	}
	if n > 200 {
		t.Fatalf("too many node grant RPCs: %d", n)
	}
}

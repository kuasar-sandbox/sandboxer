package artifact

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	cacheclient "github.com/kuasar-sandbox/accelerator/pkg/cache/client"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/wire"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

// The listener itself disappears longer than the former refill budget. No
// proxy keeps the client's Dial successful, and no second logical caller
// rescues the original synchronous read after the endpoint comes back.
func TestDirectEndpointLongOutage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "direct.sock")
	var payloadReads, active, maximum atomic.Int32
	start := func() func() {
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var workers sync.WaitGroup
		conns := make(map[net.Conn]bool)
		accepted := make(chan struct{})
		go func() {
			defer close(accepted)
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				conns[c] = true
				mu.Unlock()
				n := active.Add(1)
				for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
				}
				workers.Add(1)
				go func() {
					defer workers.Done()
					defer active.Add(-1)
					defer c.Close()
					defer func() { mu.Lock(); delete(conns, c); mu.Unlock() }()
					b := bufio.NewReader(c)
					for {
						req, err := wire.ReadRequest(b)
						if err != nil {
							return
						}
						response := &wire.Response{Status: wire.StatusHit}
						if req.Opcode == wire.OpcodeObjectGet {
							payloadReads.Add(1)
							response.Value = cache.NewMemBlob([]byte("endpoint restored"))
						}
						req.Release()
						if wire.WriteResponse(c, response) != nil {
							return
						}
					}
				}()
			}
		}()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				l.Close()
				<-accepted
				mu.Lock()
				for c := range conns {
					c.Close()
				}
				mu.Unlock()
				workers.Wait()
			})
		}
		t.Cleanup(stop)
		return stop
	}
	stop := start()
	client, err := cacheclient.NewGetter(path, cacheclient.Options{Pool: 4, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	stop()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var attempts, dialFailures atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- readretry.Do(ctx, func() error {
			attempts.Add(1)
			result, blob, err := client.Get(ctx, store.PartitionChunk, store.ContentKey{})
			if blob != nil {
				defer blob.Release()
			}
			if err == nil && (result != cache.CacheHit || blob == nil || string(blob.Bytes()) != "endpoint restored") {
				return readerr.Mark(errors.New("wrong complete object"), false)
			}
			var op *net.OpError
			if errors.As(err, &op) && op.Op == "dial" {
				dialFailures.Add(1)
			}
			return err
		})
	}()
	finished := false
	defer func() {
		cancel()
		if !finished {
			<-done
		}
	}()
	fdCount := func() int {
		f, e := os.ReadDir("/proc/self/fd")
		if e != nil {
			t.Fatal(e)
		}
		return len(f)
	}
	baseFD, baseGo := fdCount(), runtime.NumGoroutine()
	maxFD, maxGo := baseFD, baseGo
	for sample := 1; sample <= 18; sample++ {
		timer := time.NewTimer(time.Until(started.Add(time.Duration(sample) * time.Second)))
		select {
		case err := <-done:
			timer.Stop()
			finished = true
			t.Fatalf("original logical read ended offline: %v", err)
		case <-timer.C:
		}
		f, g := fdCount(), runtime.NumGoroutine()
		maxFD, maxGo = max(maxFD, f), max(maxGo, g)
		if f > baseFD+8 || g > baseGo+8 {
			t.Fatalf("unbounded outage: fd=%d/%d goroutines=%d/%d", f, baseFD, g, baseGo)
		}
		t.Logf("offline=%s attempts=%d direct_dial_failures=%d fd=%d goroutines=%d", time.Since(started).Round(time.Millisecond), attempts.Load(), dialFailures.Load(), f, g)
	}
	stopRestored := start()
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("original logical read did not recover without another caller")
	}
	client.Close()
	stopRestored()
	if dialFailures.Load() < 10 || payloadReads.Load() != 1 || maximum.Load() > 4 || active.Load() != 0 {
		t.Fatalf("recovery contract: attempts=%d dial_failures=%d payload_reads=%d max_connections=%d active=%d", attempts.Load(), dialFailures.Load(), payloadReads.Load(), maximum.Load(), active.Load())
	}
	t.Logf("PASS elapsed=%s attempts=%d direct_dial_failures=%d completion=1 max_connections=%d max_fd=%d max_goroutines=%d", time.Since(started).Round(time.Millisecond), attempts.Load(), dialFailures.Load(), maximum.Load(), maxFD, maxGo)
}

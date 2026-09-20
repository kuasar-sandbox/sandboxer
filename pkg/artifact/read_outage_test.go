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
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

// Repeated direct endpoint failures must not leak resources; the same client
// must recover once its endpoint returns. Retry duration/count policy is covered
// deterministically by internal/readretry.
func TestDirectEndpointOutageRecovery(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var attempts, dialFailures atomic.Int32
	fdCount := func() int {
		f, e := os.ReadDir("/proc/self/fd")
		if e != nil {
			t.Fatal(e)
		}
		return len(f)
	}
	baseFD, baseGo := fdCount(), runtime.NumGoroutine()
	maxFD, maxGo := baseFD, baseGo
	for attempt := 0; attempt < 20; attempt++ {
		attempts.Add(1)
		result, blob, err := client.Get(ctx, store.PartitionChunk, store.ContentKey{})
		if blob != nil {
			blob.Release()
		}
		if err == nil {
			t.Fatalf("offline attempt %d unexpectedly completed: result=%v", attempt+1, result)
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			dialFailures.Add(1)
		}
		f, g := fdCount(), runtime.NumGoroutine()
		maxFD, maxGo = max(maxFD, f), max(maxGo, g)
		if f > baseFD+8 || g > baseGo+8 {
			t.Fatalf("unbounded outage: fd=%d/%d goroutines=%d/%d", f, baseFD, g, baseGo)
		}
	}

	stopRestored := start()
	result, blob, err := client.Get(ctx, store.PartitionChunk, store.ContentKey{})
	if err != nil {
		t.Fatal(err)
	}
	if blob == nil || result != cache.CacheHit || string(blob.Bytes()) != "endpoint restored" {
		t.Fatalf("restored endpoint returned result=%v blob=%v", result, blob)
	}
	blob.Release()
	client.Close()
	stopRestored()
	if dialFailures.Load() < 20 || payloadReads.Load() != 1 || maximum.Load() > 4 || active.Load() != 0 {
		t.Fatalf("recovery contract: attempts=%d dial_failures=%d payload_reads=%d max_connections=%d active=%d", attempts.Load(), dialFailures.Load(), payloadReads.Load(), maximum.Load(), active.Load())
	}
	t.Logf("PASS attempts=%d direct_dial_failures=%d completion=1 max_connections=%d max_fd=%d max_goroutines=%d", attempts.Load(), dialFailures.Load(), maximum.Load(), maxFD, maxGo)

}

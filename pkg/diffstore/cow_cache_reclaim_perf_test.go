package diffstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// Opt-in host test for tail latency while the node reclaims cold clean pages.
// Only a small hot working set is repeatedly accessed during large shrink.
func TestElasticCacheHotReadDuringReclaim(t *testing.T) {
	if os.Getenv("KUASAR_CACHE_RECLAIM_TEST") != "1" {
		t.Skip("large host memory reclaim benchmark")
	}
	const miB = uint64(1 << 20)
	cache, err := NewElasticCache(256*miB, 128*miB, 256*miB)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := OpenDisk(filepath.Join(t.TempDir(), "bench.diff"), nil,
		DiffInit{CreateSize: 256 * int64(miB)}, WithCache(cache))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		buf := bytes.Repeat([]byte{byte((i % 250) + 1)}, int(miB))
		if _, err := disk.WriteAt(buf, int64(i)*int64(miB)); err != nil {
			t.Fatal(err)
		}
	}
	if err := disk.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	latencies := make([]time.Duration, 0, 100000)
	errs := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, cowBlockSize)
		for len(latencies) < cap(latencies) {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			if _, err := disk.ReadAt(buf, 0); err != nil {
				errs <- err
				return
			}
			if buf[0] != 1 {
				errs <- os.ErrInvalid
				return
			}
			latencies = append(latencies, time.Since(start))
		}
	}()
	shrinkStart := time.Now()
	// Mirror the controller's 1MiB physical steps while returning node
	// reservations only after a full 8MiB budget unit is reclaimed.
	const pageBatch = uint64(256 << 10)
	for size := 256*miB - pageBatch; size >= 16*miB; size -= pageBatch {
		ok, err := cache.TryResizeCapacity(size)
		if err != nil || !ok {
			t.Fatalf("shrink at %d MiB: %v %v", size/miB, ok, err)
		}
	}
	shrinkElapsed := time.Since(shrinkStart)
	close(stop)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	if len(latencies) < 100 {
		t.Fatalf("too few hot reads %d", len(latencies))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	s := cache.BudgetSnapshot()
	t.Logf("reclaim duration=%v, hot read count=%d, p50=%v, p99=%v, p999=%v, max=%v, physical=%d MiB",
		shrinkElapsed, len(latencies), latencies[len(latencies)/2],
		latencies[len(latencies)*99/100], latencies[len(latencies)*999/1000],
		latencies[len(latencies)-1], s.Resident/miB)
	if s.Resident != 16*miB || s.Capacity != 16*miB {
		t.Fatalf("reclaim did not return 240MiB: %+v", s)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
}

package resctl

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

// Separate-process baseline for the elastic 768MiB workload. The disk and
// workload match TestElasticCacheBurst768 but cache size is fixed to 1GiB.
func TestFixedCacheBurst768Baseline(t *testing.T) {
	if os.Getenv("KUASAR_CACHE_BURST_TEST") != "1" {
		t.Skip("opt-in large host test")
	}
	const mib = 1 << 20
	cache, err := diffstore.NewCache(1024*mib, 512*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	disk, err := diffstore.OpenDisk(filepath.Join(t.TempDir(), "fixed.diff"), nil,
		diffstore.DiffInit{CreateSize: 768 * mib}, diffstore.WithCache(cache))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	buf := make([]byte, mib)
	latency := make([]time.Duration, 0, 768)
	start := time.Now()
	for i := 0; i < 768; i++ {
		for j := range buf {
			buf[j] = byte((i + j) % 251)
		}
		begin := time.Now()
		if _, err := disk.WriteAt(buf, int64(i*mib)); err != nil {
			t.Fatal(err)
		}
		latency = append(latency, time.Since(begin))
	}
	written := time.Since(start)
	if err := disk.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
	t.Logf("1MiB foreground write latency p50=%v p99=%v max=%v; total-write+drain=%v; used=%dMiB",
		latency[len(latency)/2], latency[len(latency)*99/100], latency[len(latency)-1],
		written, cache.Stats().Used/mib)
	for pass := 0; pass < 2; pass++ {
		start = time.Now()
		for i := 0; i < 768; i++ {
			if _, err := disk.ReadAt(buf, int64(i*mib)); err != nil {
				t.Fatal(err)
			}
			if buf[0] != byte(i%251) {
				t.Fatalf("corrupt block %d pass %d", i, pass)
			}
		}
		t.Logf("fixed read pass %d %v", pass, time.Since(start))
	}
}

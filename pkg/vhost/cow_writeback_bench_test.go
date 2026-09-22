package vhost

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
)

// discardWriteBody isolates staging/encryption CPU from storage latency. Only
// full overwrites are issued and the complete working set remains in cache.
type discardWriteBody struct{ diffBodyIO }

func (discardWriteBody) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }

func BenchmarkCOWWritebackBatch(b *testing.B) {
	for _, file := range []bool{false, true} {
		for _, encrypted := range []bool{false, true} {
			b.Run(fmt.Sprintf("file=%t/encrypted=%t", file, encrypted), func(b *testing.B) {
				// Admit a complete request before selection, so copy comparisons
				// have the same batch shape despite scheduler/timer variation.
				ready := make(chan struct{}, 4)
				cache, err := newCOWCache(DefaultCOWCacheSize, DefaultCOWMaxDirtySize, cacheHooks{beforeSelect: func() { <-ready }})
				if err != nil {
					b.Fatal(err)
				}
				opts := []BlockCOWOption{WithCOWCache(cache)}
				if encrypted {
					opts = append(opts, WithDiffEncryption(testDiffKey(27), true))
				}
				cow, err := OpenBlockCOW(filepath.Join(b.TempDir(), "diff"), nil, DiffInit{CreateSize: 4 << 20}, opts...)
				if err != nil {
					close(ready)
					cache.Close()
					b.Fatal(err)
				}
				defer func() { close(ready); cow.Close(); cache.Close() }()
				if !file {
					cow.diff.bodyIO = discardWriteBody{cow.diff.bodyIO}
				}
				probe := &workingSetIO{diffBodyIO: cow.diff.bodyIO}
				cow.diff.bodyIO = probe
				buf := make([]byte, maxDiffScratchSize)
				for i := range buf {
					buf[i] = byte(i%251 + 1)
				}
				for off := int64(0); off < cow.size; off += int64(len(buf)) {
					if _, err := cow.WriteAt(buf, off); err != nil {
						b.Fatal(err)
					}
					ready <- struct{}{}
				}
				if err := cow.Drain(context.Background()); err != nil {
					b.Fatal(err)
				}
				probe.writeCalls.Store(0)
				probe.writes.Store(0)
				runtime.GC()
				var mem runtime.MemStats
				runtime.ReadMemStats(&mem)
				b.SetBytes(int64(len(buf)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := cow.WriteAt(buf, int64(i%4*len(buf))); err != nil {
						b.Fatal(err)
					}
					ready <- struct{}{}
					if err := cow.Drain(context.Background()); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(probe.writeCalls.Load())/float64(b.N), "body-writes/op")
				b.ReportMetric(float64(probe.writes.Load())/float64(b.N), "body-B/op")
				b.ReportMetric(float64(mem.HeapInuse), "heap-inuse-B")
			})
		}
	}
}

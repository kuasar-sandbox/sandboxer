package vhost

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Multiple callers model backend contention; the current guest profile exposes
// one queue per disk. Each caller owns a disjoint region and its output buffer.
func BenchmarkCOWContention(b *testing.B) {
	for _, disks := range []int{1, 2} {
		for _, callers := range []int{1, 8} {
			for _, mixed := range []bool{false, true} {
				b.Run(fmt.Sprintf("disks=%d/callers=%d/mixed=%t", disks, callers, mixed), func(b *testing.B) {
					cache, err := NewCOWCache(DefaultCOWCacheSize, DefaultCOWMaxDirtySize)
					if err != nil {
						b.Fatal(err)
					}
					defer cache.Close()
					cows := make([]*BlockCOW, disks)
					for i := range cows {
						cows[i], err = OpenBlockCOW(filepath.Join(b.TempDir(), "diff"), nil, DiffInit{CreateSize: 1 << 20}, WithCOWCache(cache))
						if err != nil {
							b.Fatal(err)
						}
						defer cows[i].Close()
						if _, err := cows[i].WriteAt(make([]byte, 1<<20), 0); err != nil {
							b.Fatal(err)
						}
					}
					if err := cache.Drain(context.Background()); err != nil {
						b.Fatal(err)
					}
					var wg sync.WaitGroup
					start := make(chan struct{})
					samples := make([][1024]int64, callers)
					for caller := 0; caller < callers; caller++ {
						wg.Add(1)
						go func() {
							defer wg.Done()
							buf := make([]byte, cowBlockSize)
							cow := cows[caller%disks]
							operations := (b.N + callers - 1 - caller) / callers
							sampleCount := min(operations, len(samples[caller]))
							sampleIndex, nextSample := 0, 0
							<-start
							for i := caller; i < b.N; i += callers {
								off := int64((caller*16 + (i/callers)%16) * cowBlockSize)
								before := time.Now()
								var err error
								if mixed && i%4 == 0 {
									_, err = cow.WriteAt(buf, off)
								} else {
									_, err = cow.ReadAt(buf, off)
								}
								if i/callers == nextSample {
									samples[caller][sampleIndex] = time.Since(before).Nanoseconds()
									sampleIndex++
									nextSample = sampleIndex * operations / sampleCount
								}
								if err != nil {
									b.Error(err)
									return
								}
							}
						}()
					}
					b.SetBytes(cowBlockSize)
					b.ReportAllocs()
					b.ResetTimer()
					close(start)
					wg.Wait()
					if err := cache.Drain(context.Background()); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					var all []int64
					for _, s := range samples {
						for _, ns := range s {
							if ns != 0 {
								all = append(all, ns)
							}
						}
					}
					reportSamples(b, all)
				})
			}
		}
	}
}

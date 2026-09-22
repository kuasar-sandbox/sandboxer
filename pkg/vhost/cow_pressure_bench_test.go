package vhost

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These adapters deliberately keep instrumentation out of production. Artificial
// latency isolates ownership/lock waits; it is not a storage performance claim.
type pressureIO struct {
	diffBodyIO
	readDelay, writeDelay time.Duration
	started               chan struct{}
	armed                 atomic.Bool
	reads, hotMisses      atomic.Int64
	readNanos             atomic.Int64
	active, peak          *atomic.Int64
}

func (p *pressureIO) ReadAt(buf []byte, off int64) (int, error) {
	start := time.Now()
	p.reads.Add(1)
	if len(buf) == cowBlockSize {
		p.hotMisses.Add(1)
	}
	if p.active != nil {
		n := p.active.Add(1)
		for old := p.peak.Load(); old < n && !p.peak.CompareAndSwap(old, n); old = p.peak.Load() {
		}
		defer p.active.Add(-1)
	}
	if p.started != nil && p.armed.Swap(false) {
		p.started <- struct{}{}
	}
	if p.readDelay != 0 {
		time.Sleep(p.readDelay)
	}
	n, err := p.diffBodyIO.ReadAt(buf, off)
	p.readNanos.Add(time.Since(start).Nanoseconds())
	return n, err
}

func (p *pressureIO) WriteAt(buf []byte, off int64) (int, error) {
	if p.writeDelay != 0 && p.armed.Swap(false) {
		p.started <- struct{}{}
		time.Sleep(p.writeDelay)
	}
	return p.diffBodyIO.WriteAt(buf, off)
}

func pressureCache(b *testing.B, dirty uint64, hooks cacheHooks) *COWCache {
	b.Helper()
	c, err := newCOWCache(DefaultCOWCacheSize, dirty, hooks)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c
}

func pressureCOW(b *testing.B, cache *COWCache, size, upper int64) *BlockCOW {
	b.Helper()
	c, err := OpenBlockCOW(filepath.Join(b.TempDir(), "active.diff"), nil, DiffInit{CreateSize: size}, WithCOWCache(cache))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	// Untimed preparation bypasses cache admission so every first probe is cold.
	buf := make([]byte, maxDiffScratchSize)
	for off := int64(0); off < upper; off += int64(len(buf)) {
		if _, err := c.diff.WriteAt(buf[:min(int64(len(buf)), upper-off)], off); err != nil {
			b.Fatal(err)
		}
	}
	for block := int64(0); block < upper/cowBlockSize; block++ {
		c.markDirty(block)
	}
	return c
}

// One fixed reader issues 1 MiB cold requests while a writer targets a disjoint
// 4 KiB region. The other-disk control shares exactly the same cache budget.
func BenchmarkCOWStripeInterference(b *testing.B) {
	for _, otherDisk := range []bool{false, true} {
		for _, delay := range []time.Duration{0, time.Millisecond, 5 * time.Millisecond} {
			b.Run(fmt.Sprintf("other-disk=%t/delay=%s", otherDisk, delay), func(b *testing.B) {
				cache := pressureCache(b, DefaultCOWMaxDirtySize, cacheHooks{})
				reader := pressureCOW(b, cache, 128<<20, 64<<20)
				writer := reader
				if otherDisk {
					writer = pressureCOW(b, cache, 128<<20, 0)
				}
				probe := &pressureIO{diffBodyIO: reader.diff.bodyIO, readDelay: delay, started: make(chan struct{}, 1)}
				reader.diff.bodyIO = probe
				jobs, done := make(chan int64), make(chan error)
				var readElapsed time.Duration
				go func() {
					buf := make([]byte, maxDiffScratchSize)
					for off := range jobs {
						start := time.Now()
						_, err := reader.ReadAt(buf, off)
						readElapsed += time.Since(start)
						done <- err
					}
				}()
				defer close(jobs)
				buf := make([]byte, cowBlockSize)
				samples := make([]int64, min(b.N, 1024))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					probe.armed.Store(true)
					jobs <- int64(i%64) * maxDiffScratchSize
					<-probe.started
					start := time.Now()
					_, err := writer.WriteAt(buf, 96<<20)
					samples[i%len(samples)] = time.Since(start).Nanoseconds()
					readErr := <-done
					if err != nil || readErr != nil {
						b.Fatalf("write=%v read=%v", err, readErr)
					}
				}
				b.StopTimer()
				reportSamples(b, samples)
				b.ReportMetric(float64(b.N)/readElapsed.Seconds(), "read-MiB/s")
				b.ReportMetric(float64(probe.readNanos.Load())/float64(b.N)/1000, "body-read-us/op")
				b.ReportMetric(float64(probe.reads.Load())/float64(b.N), "body-reads/op")
			})
		}
	}
}

// A 4 MiB hot set is revisited once per 64 MiB of sequential traffic. Cold
// read admission and completed writeback are measured separately.
func BenchmarkCOWScanPollution(b *testing.B) {
	for _, scan := range []string{"none", "read", "writeback"} {
		b.Run(scan, func(b *testing.B) {
			cache := pressureCache(b, DefaultCOWMaxDirtySize, cacheHooks{})
			cow := pressureCOW(b, cache, 68<<20, 68<<20)
			buf := make([]byte, maxDiffScratchSize)
			for i := 0; i < 8; i++ {
				if _, err := cow.ReadAt(buf, int64(i%4)*maxDiffScratchSize); err != nil {
					b.Fatal(err)
				}
			}
			probe := &pressureIO{diffBodyIO: cow.diff.bodyIO}
			cow.diff.bodyIO = probe
			samples := make([]int64, min(b.N*16, 1024))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				off := int64(4+i%64) * maxDiffScratchSize
				var err error
				if scan == "read" {
					_, err = cow.ReadAt(buf, off)
				} else if scan == "writeback" {
					_, err = cow.WriteAt(buf, off)
					if err == nil {
						err = cow.Drain(context.Background())
					}
				}
				if err != nil {
					b.Fatal(err)
				}
				for j := 0; j < 16; j++ {
					off := int64((i*16+j)%1024) * cowBlockSize
					start := time.Now()
					if _, err := cow.ReadAt(buf[:cowBlockSize], off); err != nil {
						b.Fatal(err)
					}
					samples[(i*16+j)%len(samples)] = time.Since(start).Nanoseconds()
				}
			}
			b.StopTimer()
			reportSamples(b, samples)
			b.ReportMetric(100*(1-float64(probe.hotMisses.Load())/float64(b.N*16)), "hot-hit-percent")
			b.ReportMetric(float64(probe.reads.Load()), "body-read-calls")
			b.ReportMetric(float64(cache.Stats().PeakUsed), "cache-peak-B")
		})
	}
}

// A cold disk repeatedly publishes full batches while another disk serves hot
// pages from the same cache. Fixed callers expose global cache-lock contention
// without the same-disk stripe or direct workspace locks.
func BenchmarkCOWColdCacheContention(b *testing.B) {
	for _, callers := range []int{1, 8} {
		b.Run(fmt.Sprintf("hot-callers=%d", callers), func(b *testing.B) {
			cache := pressureCache(b, DefaultCOWMaxDirtySize, cacheHooks{})
			cold := pressureCOW(b, cache, 64<<20, 64<<20)
			hot := pressureCOW(b, cache, 1<<20, 1<<20)
			buf := make([]byte, maxDiffScratchSize)
			if _, err := hot.ReadAt(buf, 0); err != nil {
				b.Fatal(err)
			}
			var stop atomic.Bool
			var wg sync.WaitGroup
			start := make(chan struct{})
			samples := make([][1024]int64, callers)
			counts := make([]int, callers)
			for caller := 0; caller < callers; caller++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					page := make([]byte, cowBlockSize)
					<-start
					for !stop.Load() {
						before := time.Now()
						_, err := hot.ReadAt(page, int64(caller*16+counts[caller]%16)*cowBlockSize)
						samples[caller][counts[caller]%1024] = time.Since(before).Nanoseconds()
						counts[caller]++
						if err != nil {
							b.Error(err)
							return
						}
					}
				}()
			}
			b.SetBytes(maxDiffScratchSize)
			b.ReportAllocs()
			b.ResetTimer()
			close(start)
			for i := 0; i < b.N; i++ {
				if _, err := cold.ReadAt(buf, int64(i%64)*maxDiffScratchSize); err != nil {
					b.Error(err)
					break
				}
			}
			stop.Store(true)
			wg.Wait()
			b.StopTimer()
			var all []int64
			operations := 0
			for caller, count := range counts {
				operations += count
				all = append(all, samples[caller][:min(count, 1024)]...)
			}
			if len(all) > 0 {
				reportSamples(b, all)
			}
			b.ReportMetric(float64(operations)/b.Elapsed().Seconds(), "hot-ops/s")
		})
	}
}

// The first version's physical write is delayed. The second write either
// targets that frozen page, exhausts quota on another page, or has neither wait.
func BenchmarkCOWFrozenOverwrite(b *testing.B) {
	for _, mode := range []string{"same-page", "dirty-quota", "independent"} {
		for _, delay := range []time.Duration{time.Millisecond, 5 * time.Millisecond} {
			b.Run(fmt.Sprintf("%s/delay=%s", mode, delay), func(b *testing.B) {
				var frozen, quota atomic.Int64
				var cache *COWCache
				var cow *BlockCOW
				target, dirty := int64(0), uint64(DefaultCOWMaxDirtySize)
				if mode != "same-page" {
					target = cowBlockSize
				}
				if mode == "dirty-quota" {
					dirty = cowBlockSize
				}
				cache = pressureCache(b, dirty, cacheHooks{waiting: func() {
					if p := cache.pages[cacheKey{cow, target / cowBlockSize}]; p != nil && p.state == cacheWriteback {
						frozen.Add(1)
					} else if cache.dirtyUsed == cache.maxDirty {
						quota.Add(1)
					}
				}})
				cow = pressureCOW(b, cache, 1<<20, 1<<20)
				probe := &pressureIO{diffBodyIO: cow.diff.bodyIO, writeDelay: delay, started: make(chan struct{}, 1)}
				cow.diff.bodyIO = probe
				buf := make([]byte, cowBlockSize)
				samples := make([]int64, min(b.N, 1024))
				var frozenWaits, quotaWaits int64
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					probe.armed.Store(true)
					if _, err := cow.WriteAt(buf, 0); err != nil {
						b.Fatal(err)
					}
					cache.mu.Lock()
					cache.kickLocked(true)
					cache.mu.Unlock()
					<-probe.started
					f, q := frozen.Load(), quota.Load()
					start := time.Now()
					if _, err := cow.WriteAt(buf, target); err != nil {
						b.Fatal(err)
					}
					samples[i%len(samples)] = time.Since(start).Nanoseconds()
					frozenWaits += frozen.Load() - f
					quotaWaits += quota.Load() - q
					if err := cow.Drain(context.Background()); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				reportSamples(b, samples)
				b.ReportMetric(float64(frozenWaits)/float64(b.N), "frozen-waits/op")
				b.ReportMetric(float64(quotaWaits)/float64(b.N), "quota-waits/op")
			})
		}
	}
}

// Compare existing per-diff workspaces under a fixed number of callers. Each
// body probe shares an atomic counter, so peak reports actual outstanding I/O.
// Use -benchtime=512x to keep all requested pages cold and sample counts fixed.
func BenchmarkCOWReadConcurrency(b *testing.B) {
	for _, sandboxes := range []int{1, 2} {
		for _, disks := range []int{1, 2} {
			for _, callers := range []int{1, 8} {
				for _, delay := range []time.Duration{0, time.Millisecond} {
					b.Run(fmt.Sprintf("sandboxes=%d/disks=%d/callers=%d/delay=%s", sandboxes, disks, callers, delay), func(b *testing.B) {
						var active, peak, reads atomic.Int64
						var cows []*BlockCOW
						var probes []*pressureIO
						for s := 0; s < sandboxes; s++ {
							cache := pressureCache(b, DefaultCOWMaxDirtySize, cacheHooks{})
							for d := 0; d < disks; d++ {
								cow := pressureCOW(b, cache, 64<<20, 64<<20)
								probe := &pressureIO{diffBodyIO: cow.diff.bodyIO, readDelay: delay, active: &active, peak: &peak}
								cow.diff.bodyIO = probe
								cows, probes = append(cows, cow), append(probes, probe)
							}
						}
						var wg sync.WaitGroup
						start := make(chan struct{})
						samples := make([][128]int64, callers)
						for caller := 0; caller < callers; caller++ {
							wg.Add(1)
							go func() {
								defer wg.Done()
								buf := make([]byte, cowBlockSize)
								<-start
								for i := caller; i < b.N; i += callers {
									before := time.Now()
									_, err := cows[i%len(cows)].ReadAt(buf, int64((i*40503)%16384)*cowBlockSize)
									samples[caller][(i/callers)%128] = time.Since(before).Nanoseconds()
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
						for _, p := range probes {
							reads.Add(p.reads.Load())
						}
						b.ReportMetric(float64(peak.Load()), "body-inflight-peak")
						b.ReportMetric(float64(reads.Load())/float64(b.N), "body-reads/op")
					})
				}
			}
		}
	}
}

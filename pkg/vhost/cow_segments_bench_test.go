package vhost

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

// These fixtures exercise processChain, including descriptor walking and the
// queue's cancelable context. Payload allocations are guest-memory fixtures,
// outside measurement. Mixed segments deliberately split complete COW blocks.
func segmentSizes(total int, layout string) []int {
	step := map[string]int{"1x1m": 1 << 20, "4x256k": 256 << 10, "16x64k": 64 << 10, "256x4k": 4096}[layout]
	var sizes []int
	for left := total; left > 0; {
		n := step
		if layout == "mixed" {
			n = []int{512, 3584, 65024, 512, 16384, 49152}[len(sizes)%6]
		}
		n = min(n, left)
		sizes = append(sizes, n)
		left -= n
	}
	return sizes
}

func segmentQueue(backend Backend, kind uint32, sizes []int) (*Server, *virtq, []byte, []byte) {
	num := len(sizes) + 2
	dataStart := num*16 + 16
	total := 0
	for _, n := range sizes {
		total += n
	}
	mem := make([]byte, dataStart+total+1)
	s := NewServer("unused", backend, nil)
	const uva = 0x1000
	s.memTable.SetRegions([]MemRegion{{GuestPhysAddr: 0, UserspaceAddr: uva, MemorySize: uint64(len(mem)), mmapBytes: mem}})
	q := &virtq{num: uint32(num), descAddr: uva}
	put := func(i, addr, length int, flags uint16) {
		d := mem[i*16 : (i+1)*16]
		binary.LittleEndian.PutUint64(d[:8], uint64(addr))
		binary.LittleEndian.PutUint32(d[8:12], uint32(length))
		binary.LittleEndian.PutUint16(d[12:14], flags)
		binary.LittleEndian.PutUint16(d[14:16], uint16(i+1))
	}
	hdr := mem[num*16 : dataStart]
	binary.LittleEndian.PutUint32(hdr[:4], kind)
	put(0, num*16, 16, descFlagNext)
	pos := dataStart
	for i, n := range sizes {
		flags := descFlagNext
		if kind == BlkTypeIn {
			flags |= descFlagWrite
		}
		put(i+1, pos, n, flags)
		pos += n
	}
	put(num-1, pos, 1, descFlagWrite)
	return s, q, hdr, mem[dataStart:]
}

type segmentCountBackend struct {
	*CowBackend
	reads, writes int
}

func (b *segmentCountBackend) readAt(ctx context.Context, p []byte, off int64) (int, error) {
	b.reads++
	return b.C.readAt(ctx, p, off)
}
func (b *segmentCountBackend) writeAt(ctx context.Context, p []byte, off int64) (int, error) {
	b.writes++
	return b.C.writeAt(ctx, p, off)
}

func BenchmarkCOWSegments(b *testing.B) {
	for _, encrypted := range []bool{false, true} {
		for _, work := range []string{"cold-read", "cache-hit", "write"} {
			for _, layout := range []string{"1x1m", "4x256k", "16x64k", "256x4k", "mixed"} {
				b.Run(fmt.Sprintf("%s/%s/encrypted=%t", work, layout, encrypted), func(b *testing.B) {
					const request = 1 << 20
					const dataset = 64 << 20
					var opts []BlockCOWOption
					if encrypted {
						opts = append(opts, WithDiffEncryption([32]byte{91}, true))
					}
					cow, err := OpenBlockCOW(filepath.Join(b.TempDir(), "diff"), nil, DiffInit{CreateSize: dataset}, opts...)
					if err != nil {
						b.Fatal(err)
					}
					defer cow.Close()
					backend := &segmentCountBackend{CowBackend: &CowBackend{C: cow}}
					kind := uint32(BlkTypeIn)
					if work == "write" {
						kind = BlkTypeOut
					}
					s, q, hdr, data := segmentQueue(backend, kind, segmentSizes(request, layout))
					q.ctx, q.cancel = context.WithCancel(context.Background())
					defer q.cancel()
					for i := range data[:request] {
						data[i] = 0x6d
					}
					for i := 0; i < request; i += cowBlockSize {
						binary.LittleEndian.PutUint64(data[i:i+8], uint64(i))
					}
					expectedCRC := crc32.ChecksumIEEE(data[:request])
					if work != "write" {
						for off := int64(0); off < dataset; off += request {
							if err := writeFullAt(cow.diff, data[:request], off); err != nil {
								b.Fatal(err)
							}
						}
						for block := int64(0); block < dataset/cowBlockSize; block++ {
							cow.markDirty(block)
						}
						if work == "cache-hit" {
							if _, err := cow.ReadAt(data[:request], 0); err != nil {
								b.Fatal(err)
							}
						}
					}
					probe := &workingSetIO{diffBodyIO: cow.diff.bodyIO}
					cow.diff.bodyIO = probe
					var samples [4096]int64 // fixed-size diagnostic sample, no timed allocation
					sampleCount := min(b.N, len(samples))
					sampleIndex, nextSample := 0, 0
					var verification time.Duration
					b.SetBytes(request)
					b.ReportAllocs()
					runtime.GC()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						off := (i % (dataset / request)) * request
						if work == "cache-hit" {
							off = 0
						}
						binary.LittleEndian.PutUint64(hdr[8:16], uint64(off/SectorSize))
						if kind == BlkTypeOut {
							binary.LittleEndian.PutUint64(data[:8], uint64(off))
							binary.LittleEndian.PutUint64(data[8:16], uint64(i))
						}
						start := time.Now()
						n, err := s.processChain(q, 0)
						if i == nextSample {
							samples[sampleIndex] = time.Since(start).Nanoseconds()
							sampleIndex++
							nextSample = sampleIndex * b.N / sampleCount
						}
						want := 1
						if kind == BlkTypeIn {
							want += request
						}
						if err != nil || n != want || data[request] != BlkStatusOK {
							b.Fatalf("request: %d %v status=%d", n, err, data[request])
						}
						if kind == BlkTypeIn {
							verifyStart := time.Now()
							if crc32.ChecksumIEEE(data[:request]) != expectedCRC {
								b.Fatal("read payload mismatch")
							}
							verification += time.Since(verifyStart)
						}
					}
					if err := cow.Drain(context.Background()); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					// Full-payload validation is benchmark-only work. Avoid per-I/O
					// StopTimer/StartTimer MemStats scans; subtract its wall time.
					elapsed := b.Elapsed() - verification
					b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "ns/op")
					b.ReportMetric(float64(b.N*request)/elapsed.Seconds()/1e6, "MB/s")
					b.ReportMetric(float64(verification.Nanoseconds())/float64(b.N), "verification-ns/op")
					reportSamples(b, samples[:sampleCount])
					b.ReportMetric(float64(backend.reads+backend.writes)/float64(b.N), "cow-calls/op")
					b.ReportMetric(float64(probe.readCalls.Load())/float64(b.N), "body-reads/op")
					b.ReportMetric(float64(probe.writeCalls.Load())/float64(b.N), "body-writes/op")
					b.ReportMetric(float64(probe.reads.Load())/float64(b.N*request), "read-amplification")
					b.ReportMetric(float64(probe.writes.Load())/float64(b.N*request), "write-amplification")
					stats := cow.cache.Stats()
					b.ReportMetric(float64(stats.PeakUsed), "cache-peak-B")
					b.ReportMetric(float64(stats.PeakDirty), "dirty-peak-B")
					if kind == BlkTypeOut {
						// Check the actual diff after Drain, bypassing the clean cache.
						// Metrics above already captured only the timed workload I/O.
						var check [cowBlockSize]byte
						for rangeIndex := 0; rangeIndex < min(b.N, dataset/request); rangeIndex++ {
							off := rangeIndex * request
							last := rangeIndex + (b.N-1-rangeIndex)/(dataset/request)*(dataset/request)
							binary.LittleEndian.PutUint64(data[:8], uint64(off))
							binary.LittleEndian.PutUint64(data[8:16], uint64(last))
							for block := 0; block < request; block += cowBlockSize {
								n, err := cow.diff.ReadAt(check[:], int64(off+block))
								if err != nil || n != len(check) || !bytes.Equal(check[:], data[block:block+cowBlockSize]) {
									b.Fatalf("write payload mismatch at %d: n=%d err=%v", off+block, n, err)
								}
							}
						}
					}
				})
			}
		}
	}
}

func reportSamples(b *testing.B, samples []int64) {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	b.ReportMetric(float64(samples[len(samples)/2])/1000, "p50-us")
	b.ReportMetric(float64(samples[len(samples)*99/100])/1000, "p99-us")
}

package vhost

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This file is also run unchanged on the exact buffered base revision, using
// a tiny baseline adapter in place of cow_cache_bench_adapter_test.go. Run with
// -benchtime=1x: each case transfers across a 256 MiB working set, 8x defaults.
type workingSetCache struct {
	options []BlockCOWOption
	readAt  func(*BlockCOW, []byte, int64) (int, error)
	writeAt func(*BlockCOW, []byte, int64) (int, error)
	drain   func() error
	stats   func() map[string]float64
	close   func() error
}
type workingSetIO struct {
	diffBodyIO
	writes, reads         atomic.Uint64
	writeCalls, readCalls atomic.Uint64
}

func (p *workingSetIO) WriteAt(buf []byte, off int64) (int, error) {
	p.writeCalls.Add(1)
	n, err := p.diffBodyIO.WriteAt(buf, off)
	p.writes.Add(uint64(n))
	return n, err
}
func (p *workingSetIO) ReadAt(buf []byte, off int64) (int, error) {
	p.readCalls.Add(1)
	n, err := p.diffBodyIO.ReadAt(buf, off)
	p.reads.Add(uint64(n))
	return n, err
}

func BenchmarkCOWWorkingSet(b *testing.B) {
	for _, encrypted := range []bool{false, true} {
		for _, work := range []string{"seq-write", "random-4k", "partial-512", "hot-overwrite", "seq-read", "random-read", "multi-disk"} {
			b.Run(fmt.Sprintf("%s/encrypted=%t", work, encrypted), func(b *testing.B) { benchmarkCOWWorkingSet(b, work, encrypted, newWorkingSetCache) })
		}
	}
}
func benchmarkCOWWorkingSet(b *testing.B, work string, encrypted bool, factory func() (workingSetCache, error)) {
	const dataset = 256 << 20
	disks := 1
	if work == "multi-disk" {
		disks = 2
	}
	group, err := factory()
	if err != nil {
		b.Fatal(err)
	}
	defer group.close()
	if group.readAt == nil {
		group.readAt = (*BlockCOW).ReadAt
	}
	if group.writeAt == nil {
		group.writeAt = (*BlockCOW).WriteAt
	}
	cows := make([]*BlockCOW, disks)
	ios := make([]*workingSetIO, disks)
	for i := range cows {
		opts := append([]BlockCOWOption(nil), group.options...)
		if encrypted {
			opts = append(opts, WithDiffEncryption([32]byte{99}, true))
		}
		cows[i], err = OpenBlockCOW(filepath.Join(b.TempDir(), "active.diff"), nil, DiffInit{CreateSize: dataset / int64(disks)}, opts...)
		if err != nil {
			b.Fatal(err)
		}
		defer cows[i].Close()
		ios[i] = &workingSetIO{diffBodyIO: cows[i].diff.bodyIO}
		cows[i].diff.bodyIO = ios[i]
	}
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = 0x6d
	}
	reads := strings.Contains(work, "read")
	if reads || work == "hot-overwrite" {
		for _, cow := range cows {
			for off := int64(0); off < cow.size; off += int64(len(payload)) {
				if _, err := group.writeAt(cow, payload, off); err != nil {
					b.Fatal(err)
				}
			}
		}
		if err := group.drain(); err != nil {
			b.Fatal(err)
		}
	}
	for _, p := range ios {
		p.writes.Store(0)
		p.reads.Store(0)
		p.writeCalls.Store(0)
		p.readCalls.Store(0)
	}
	request := 4096
	if work == "seq-write" || work == "seq-read" || work == "multi-disk" {
		request = len(payload)
	} else if work == "partial-512" {
		request = 512
	}
	operations := dataset / cowBlockSize
	if request == len(payload) {
		operations = dataset / request
	}
	latencies := make([]int64, operations)
	runtime.GC()
	var admission, total time.Duration
	beforeIO := workingSetProcessIO(b)
	var cpuBefore, cpuAfter unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &cpuBefore); err != nil {
		b.Fatal(err)
	}
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	b.SetBytes(int64(operations * request))
	b.ResetTimer()
	for iter := 0; iter < b.N; iter++ {
		start := time.Now()
		for i := 0; i < operations; i++ {
			disk := i % disks
			index := i / disks
			off := int64(index * request)
			switch work {
			case "random-4k", "random-read", "partial-512":
				off = int64((i*40503)%(dataset/cowBlockSize)) * cowBlockSize
			case "hot-overwrite":
				block := (i * 40503) % (dataset / cowBlockSize)
				if i%10 != 0 {
					block %= 1024
				}
				off = int64(block * cowBlockSize)
			}
			if work == "partial-512" {
				off += 512
			}
			before := time.Now()
			if reads {
				_, err = group.readAt(cows[disk], payload[:request], off)
			} else {
				_, err = group.writeAt(cows[disk], payload[:request], off)
			}
			latencies[i] = time.Since(before).Nanoseconds()
			if err != nil {
				b.Fatal(err)
			}
			if reads && (payload[0] != 0x6d || payload[request-1] != 0x6d) {
				b.Fatal("read data mismatch")
			}
		}
		admission += time.Since(start)
		if err := group.drain(); err != nil {
			b.Fatal(err)
		}
		total += time.Since(start)
	}
	b.StopTimer()
	if err := unix.Getrusage(unix.RUSAGE_SELF, &cpuAfter); err != nil {
		b.Fatal(err)
	}
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	cpuNanos := cpuAfter.Utime.Nano() + cpuAfter.Stime.Nano() - cpuBefore.Utime.Nano() - cpuBefore.Stime.Nano()
	b.ReportMetric(float64(cpuNanos)/1e9/float64(b.N), "cpu-s")
	b.ReportMetric(float64(memAfter.Mallocs-memBefore.Mallocs)/float64(b.N*operations), "allocs/request")
	b.ReportMetric(float64(memAfter.TotalAlloc-memBefore.TotalAlloc)/float64(b.N*operations), "allocated-B/request")
	afterIO := workingSetProcessIO(b)
	for _, key := range []string{"read_bytes", "write_bytes", "cancelled_write_bytes"} {
		b.ReportMetric(float64(afterIO[key]-beforeIO[key])/float64(b.N), "proc-"+key+"-B")
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	b.ReportMetric(admission.Seconds()/float64(b.N), "admission-s")
	b.ReportMetric(total.Seconds()/float64(b.N), "including-drain-s")
	b.ReportMetric(float64(operations*request*b.N)/(1<<20)/total.Seconds(), "drained-MiB/s")
	b.ReportMetric(float64(latencies[len(latencies)*50/100])/1000, "p50-us")
	b.ReportMetric(float64(latencies[len(latencies)*99/100])/1000, "p99-us")
	var writes, readsBytes, writeCalls, readCalls, resident uint64
	for i, cow := range cows {
		writes += ios[i].writes.Load()
		writeCalls += ios[i].writeCalls.Load()
		readCalls += ios[i].readCalls.Load()
		readsBytes += ios[i].reads.Load()
		resident += workingSetResident(b, cow.diff)
	}
	b.ReportMetric(float64(writeCalls)/float64(b.N), "body-write-calls")
	b.ReportMetric(float64(readCalls)/float64(b.N), "body-read-calls")
	b.ReportMetric(float64(writes)/float64(b.N), "body-written-B")
	b.ReportMetric(float64(readsBytes)/float64(b.N), "body-read-B")
	b.ReportMetric(float64(resident), "file-resident-B")
	b.ReportMetric(float64(operations*request), "logical-B")
	b.ReportMetric(float64(readsBytes)/float64(b.N*operations*request), "read-amplification")
	b.ReportMetric(float64(writes)/float64(b.N*operations*request), "write-amplification")
	if reads {
		// These workloads issue aligned, full-page reads of previously written
		// upper pages. Every body page is a demand miss; no readahead occurs.
		misses := readsBytes / cowBlockSize
		demands := uint64(b.N * operations * request / cowBlockSize)
		b.ReportMetric(float64(misses)/float64(b.N), "cache-miss-pages")
		b.ReportMetric(float64(demands-misses)/float64(b.N), "cache-hit-pages")
	}
	for key, value := range group.stats() {
		b.ReportMetric(value, key)
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	b.ReportMetric(float64(mem.HeapInuse), "heap-inuse-B")
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		b.Fatal(err)
	}
	fields := strings.Fields(string(raw))
	rss, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(rss*uint64(os.Getpagesize())), "rss-B")
}
func workingSetResident(b *testing.B, d *diffFile) uint64 {
	b.Helper()
	m, err := unix.Mmap(int(d.f.Fd()), d.bodyOffset, int(d.logicalSize), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		b.Fatal(err)
	}
	defer unix.Munmap(m)
	vec := make([]byte, (len(m)+os.Getpagesize()-1)/os.Getpagesize())
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(unsafe.Pointer(&vec[0])))
	if errno != 0 {
		b.Fatal(errno)
	}
	var resident uint64
	for _, v := range vec {
		if v&1 != 0 {
			resident += uint64(os.Getpagesize())
		}
	}
	return resident
}

// Linux process I/O accounting includes filesystem allocation/metadata effects;
// it is separate from the active-body syscall counters and is not a device
// durability guarantee. No flushing/eviction is used to change these numbers.
func workingSetProcessIO(b *testing.B) map[string]uint64 {
	b.Helper()
	raw, err := os.ReadFile("/proc/self/io")
	if err != nil {
		b.Fatal(err)
	}
	result := make(map[string]uint64)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			b.Fatal(err)
		}
		result[strings.TrimSuffix(fields[0], ":")] = n
	}
	for _, key := range []string{"read_bytes", "write_bytes", "cancelled_write_bytes"} {
		if _, ok := result[key]; !ok {
			b.Fatalf("/proc/self/io missing %s", key)
		}
	}
	return result
}

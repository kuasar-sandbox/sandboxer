package diffstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/versioned"
	"golang.org/x/sys/unix"
)

// BenchmarkCheckpointCOWWriteback includes Drain in elapsed throughput. The
// Default 64 MiB working set exceeds the 32 MiB cache; frontend percentiles are reported
// separately and do not stand in for storage completion. Run on a recorded
// native filesystem with -benchtime=32768x (two complete traversals) or longer.
// CHECKPOINT_BENCH_MIB optionally selects a larger working set (for example,
// 128 with -benchtime=32768x touches every page once). Seed/setup is untimed.
func BenchmarkCheckpointCOWWriteback(b *testing.B) {
	for _, mode := range []string{"raw", "v1", "v2-no-version", "v2-shared", "v2-private", "v2-xts-no-version", "v2-xts-shared", "v2-xts-private"} {
		for _, workload := range []string{"first-sequential", "first-random", "sequential", "random", "partial", "hot", "mixed", "cold-read", "frequent-checkpoint", "gc-write"} {
			if strings.HasPrefix(workload, "first-") && strings.HasSuffix(mode, "-private") {
				continue
			}
			if (workload == "frequent-checkpoint" || workload == "gc-write") && (mode == "raw" || mode == "v1") {
				continue
			}
			b.Run(mode+"/"+workload, func(b *testing.B) { benchmarkCheckpointWriteback(b, mode, workload) })
		}
	}
}

// Complete Zero Discard is measured with a dense upper and a cache smaller than the range.
// Refill/Drain is excluded. The normal backend GC remains enabled, so total
// commits include concurrent reclamation and are not Discard-only commits.
func BenchmarkCheckpointDiscard(b *testing.B) {
	for _, mode := range []string{"raw", "v1", "v2-no-version", "v2-xts-no-version"} {
		b.Run(mode, func(b *testing.B) {
			const size = int64(4 << 20)
			const cacheSize = 1 << 20
			cache, err := NewCache(cacheSize, cacheSize/2)
			if err != nil {
				b.Fatal(err)
			}
			defer cache.Close()
			options := []DiskOption{WithCache(cache)}
			if strings.HasPrefix(mode, "v2") {
				options = append(options, WithCheckpoint("discard-benchmark", ""))
			}
			if mode == "v1" || strings.Contains(mode, "-xts-") {
				options = append(options, WithDiffEncryption([32]byte{1}, true))
			}
			cow, err := OpenDisk(filepath.Join(b.TempDir(), "disk.diff"), nil, DiffInit{CreateSize: size}, options...)
			if err != nil {
				b.Fatal(err)
			}
			defer cow.Close()
			backend, _ := cow.checkpointBackend()
			observer := &discardObserver{diffBackend: cow.diff}
			cow.diff = observer
			chunk := bytes.Repeat([]byte{0x45}, 1<<20)
			var commits, indexBytes, redoBytes, bitmapWrites uint64
			var userNS, systemNS, ackNS int64
			if single, ok := observer.diffBackend.(*diffFile); ok {
				single.beforePresenceWrite = func(int) error { bitmapWrites++; return nil }
			}
			var discardBitmapWrites uint64
			b.SetBytes(size)
			b.ResetTimer()
			b.StopTimer()
			for range b.N {
				for off := int64(0); off < size; off += int64(len(chunk)) {
					if _, err := cow.WriteAt(chunk, off); err != nil {
						b.Fatal(err)
					}
				}
				if err := cow.Drain(context.Background()); err != nil {
					b.Fatal(err)
				}
				var before versioned.Stats
				if backend != nil {
					before = backend.store.Stats()
				}
				var cpuBefore, cpuAfter unix.Rusage
				if err := unix.Getrusage(unix.RUSAGE_SELF, &cpuBefore); err != nil {
					b.Fatal(err)
				}
				bitmapBefore := bitmapWrites
				b.StartTimer()
				ackStart := time.Now()
				if err := cow.Discard(0, size); err != nil {
					b.Fatal(err)
				}
				ackNS += time.Since(ackStart).Nanoseconds()
				if err := cow.Drain(context.Background()); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				if err := unix.Getrusage(unix.RUSAGE_SELF, &cpuAfter); err != nil {
					b.Fatal(err)
				}
				userNS += cpuAfter.Utime.Nano() - cpuBefore.Utime.Nano()
				systemNS += cpuAfter.Stime.Nano() - cpuBefore.Stime.Nano()
				discardBitmapWrites += bitmapWrites - bitmapBefore
				if backend != nil {
					after := backend.store.Stats()
					commits += after.Commits - before.Commits
					indexBytes += after.IndexWritten - before.IndexWritten
					redoBytes += after.RedoWritten - before.RedoWritten
				}
			}
			b.ReportMetric(float64(len(observer.calls))/float64(b.N), "backend-calls/range")
			b.ReportMetric(float64(commits)/float64(b.N), "total-commits/range")
			b.ReportMetric(float64(indexBytes)/float64(b.N), "index-B/range")
			b.ReportMetric(float64(redoBytes)/float64(b.N), "redo-B/range")
			b.ReportMetric(float64(discardBitmapWrites*cowBlockSize)/float64(b.N), "presence-B/range")
			b.ReportMetric(float64(userNS)/float64(b.N), "cpu-user-ns/range")
			b.ReportMetric(float64(ackNS)/float64(b.N), "ack-ns/range")
			b.ReportMetric(float64(systemNS)/float64(b.N), "cpu-system-ns/range")
			b.ReportMetric(float64(size), "range-B")
			b.ReportMetric(float64(cacheSize), "cache-B")
		})
	}
}

func benchmarkCheckpointWriteback(b *testing.B, mode, workload string) {
	size := int64(64 << 20)
	if value := os.Getenv("CHECKPOINT_BENCH_MIB"); value != "" {
		mib, err := strconv.Atoi(value)
		if err != nil || mib < 64 || mib > 4096 {
			b.Fatal("CHECKPOINT_BENCH_MIB must be an integer from 64 to 4096")
		}
		size = int64(mib) << 20
	}
	blocks := int(size / cowBlockSize)
	ctx := context.Background()
	encrypted := mode == "v1" || strings.Contains(mode, "-xts-")
	mode = strings.Replace(mode, "-xts-", "-", 1)
	var options []DiskOption
	if mode != "raw" && mode != "v1" {
		options = append(options, WithCheckpoint("benchmark", ""))
	}
	if encrypted {
		options = append(options, WithDiffEncryption([32]byte{1}, true))
	}
	path := filepath.Join(b.TempDir(), "disk.diff")
	cow, err := OpenDisk(path, nil, DiffInit{CreateSize: size}, options...)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = cow.Close() }()
	chunk := bytes.Repeat([]byte{0x35}, 1<<20)
	firstWrite := strings.HasPrefix(workload, "first-")
	if firstWrite && b.N > blocks {
		b.Fatal("first-write benchmark must not revisit a page")
	}
	for off := int64(0); !firstWrite && off < size; off += int64(len(chunk)) {
		if _, err := cow.WriteAt(chunk, off); err != nil {
			b.Fatal(err)
		}
	}
	if err := cow.Drain(ctx); err != nil {
		b.Fatal(err)
	}
	if mode == "v2-shared" || mode == "v2-private" {
		if err := cow.Checkpoint(ctx, "initial"); err != nil {
			b.Fatal(err)
		}
	}
	if mode == "v2-private" {
		for off := int64(0); off < size; off += int64(len(chunk)) {
			if _, err := cow.WriteAt(chunk, off); err != nil {
				b.Fatal(err)
			}
		}
		if err := cow.Drain(ctx); err != nil {
			b.Fatal(err)
		}
	}
	// Reopening discards the userspace COW cache. The backend's O_DIRECT reads
	// also bypass the host page cache; the working set is twice the COW cache.
	if workload == "cold-read" {
		if err := cow.Close(); err != nil {
			b.Fatal(err)
		}
		cow, err = OpenDisk(path, nil, DiffInit{Existing: true}, options...)
		if err != nil {
			b.Fatal(err)
		}
	}
	checkpointLatencies := make([]int64, 0, 256)
	readPage := make([]byte, cowBlockSize)
	permutation := rand.New(rand.NewSource(327)).Perm(blocks)
	latencies := make([]int64, 0, min(b.N, 65536))
	var before versioned.Stats
	backend, _ := cow.checkpointBackend()
	if backend != nil {
		before = backend.store.Stats()
	}
	page := bytes.Repeat([]byte{0xc7}, cowBlockSize)
	length := cowBlockSize
	if workload == "partial" {
		length = 512
	}
	b.ReportAllocs()
	b.SetBytes(int64(length))
	b.ResetTimer()
	var cpuBefore, cpuAfter unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &cpuBefore); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		block := i % blocks
		if workload == "random" || workload == "first-random" || workload == "partial" || workload == "mixed" || workload == "cold-read" {
			block = permutation[block]
		}
		if workload == "hot" {
			block = permutation[i%1024]
		}
		off := int64(block * cowBlockSize)
		if workload == "partial" {
			off += 173
		}
		start := time.Now()
		if workload == "cold-read" || workload == "mixed" && i%2 == 1 {
			if n, err := cow.ReadAt(readPage, off); err != nil || n != len(readPage) {
				b.Fatal(n, err)
			}
			if readPage[0] != 0x35 && readPage[0] != 0xc7 {
				b.Fatal("unexpected read content")
			}
		} else if _, err := cow.WriteAt(page[:length], off); err != nil {
			b.Fatal(err)
		}
		if len(latencies) < cap(latencies) {
			latencies = append(latencies, time.Since(start).Nanoseconds())
		}
		period := blocks
		if workload == "hot" {
			period = 1024
		}
		if workload == "frequent-checkpoint" || workload == "gc-write" {
			period = 256
		}
		if backend != nil && (mode == "v2-shared" || workload == "frequent-checkpoint" || workload == "gc-write") && (i+1)%period == 0 {
			checkpointStart := time.Now()
			if err := cow.Checkpoint(ctx, CheckpointID(fmt.Sprintf("iteration-%d", i))); err != nil {
				b.Fatal(err)
			}
			if len(checkpointLatencies) < cap(checkpointLatencies) {
				checkpointLatencies = append(checkpointLatencies, time.Since(checkpointStart).Nanoseconds())
			}
			if workload == "gc-write" && i >= 4*period {
				if err := cow.DeleteCheckpoint(ctx, CheckpointID(fmt.Sprintf("iteration-%d", i-4*period))); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	drainStart := time.Now()
	if err := cow.Drain(ctx); err != nil {
		b.Fatal(err)
	}
	drainTime := time.Since(drainStart)
	if err := unix.Getrusage(unix.RUSAGE_SELF, &cpuAfter); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	b.ReportMetric(float64(cpuAfter.Utime.Nano()-cpuBefore.Utime.Nano())/1e9, "process-user-s")
	b.ReportMetric(float64(cpuAfter.Stime.Nano()-cpuBefore.Stime.Nano())/1e9, "process-system-s")
	b.ReportMetric(float64(size), "working-set-B")
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	b.ReportMetric(float64(memory.HeapAlloc), "heap-alloc-B")
	b.ReportMetric(float64(memory.HeapInuse), "heap-inuse-B")
	// RSS includes Go and native mappings; HeapAlloc/HeapInuse do not. These
	// are end-of-case observations, while the driver records process peak RSS.
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "VmRSS:" {
				if rss, err := strconv.ParseFloat(fields[1], 64); err == nil {
					b.ReportMetric(rss, "rss-KiB")
				}
			}
		}
	}
	b.ReportMetric(float64(drainTime.Nanoseconds())/float64(b.N), "drain-ns/op")
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		for _, p := range []int{50, 95, 99} {
			index := (len(latencies) - 1) * p / 100
			b.ReportMetric(float64(latencies[index]), fmt.Sprintf("frontend-p%d-ns", p))
		}
	}
	if len(checkpointLatencies) > 0 {
		sort.Slice(checkpointLatencies, func(i, j int) bool { return checkpointLatencies[i] < checkpointLatencies[j] })
		for _, p := range []int{50, 95, 99} {
			b.ReportMetric(float64(checkpointLatencies[(len(checkpointLatencies)-1)*p/100]), fmt.Sprintf("checkpoint-p%d-ns", p))
		}
	}
	if backend != nil {
		after := backend.store.Stats()
		b.ReportMetric(float64(after.Commits-before.Commits)/float64(b.N), "commits/op")
		for name, value := range map[string]uint64{"data-read-B/op": after.DataRead - before.DataRead, "data-write-B/op": after.DataWritten - before.DataWritten, "index-read-B/op": after.IndexRead - before.IndexRead, "index-write-B/op": after.IndexWritten - before.IndexWritten, "redo-B/op": after.RedoWritten - before.RedoWritten, "gc-B/op": after.GCWritten - before.GCWritten, "gc-read-B/op": after.GCRead - before.GCRead, "gc-punch-B/op": after.GCPunched - before.GCPunched, "redo-read-B/op": after.RedoRead - before.RedoRead, "allocated-slots/op": after.Allocated - before.Allocated, "reused-slots/op": after.Reused - before.Reused} {
			b.ReportMetric(float64(value)/float64(b.N), name)
		}
		hits, misses := after.CacheHits-before.CacheHits, after.CacheMisses-before.CacheMisses
		if hits+misses > 0 {
			b.ReportMetric(float64(hits)*100/float64(hits+misses), "index-cache-hit-%")
		}
	}
}

// TestCheckpointScalePhases is invoked by test/perf/diffstore against an actual
// initialized DIFF. A deterministic worker gate holds exactly 32 dirty pages;
// historical payload is never fabricated by logical truncation in this helper.
func TestCheckpointScalePhases(t *testing.T) {
	path := os.Getenv("DIFFSTORE_SCALE_PATH")
	if path == "" {
		t.Skip("requires the explicit test/perf/diffstore scale harness")
	}
	gib, err := strconv.Atoi(os.Getenv("DIFFSTORE_SCALE_GIB"))
	if err != nil || gib < 0 {
		t.Fatal("invalid DIFFSTORE_SCALE_GIB")
	}
	rounds, err := strconv.Atoi(os.Getenv("DIFFSTORE_SCALE_ROUNDS"))
	if err != nil || rounds < 1 {
		t.Fatal("invalid DIFFSTORE_SCALE_ROUNDS")
	}
	ctx := context.Background()
	options := []DiskOption{WithCheckpoint(fmt.Sprintf("scaling-%d", gib), "fixed-immutable-base")}
	for round := 0; round < rounds; round++ {
		func() {
			gate := newWorkerGate()
			cache, err := newCache(32<<20, 16<<20, cacheHooks{beforeWrite: func([]*cachePage) error { gate.wait(); return nil }})
			if err != nil {
				t.Fatal(err)
			}
			var cow *Disk
			defer func() {
				gate.open()
				if cow != nil {
					if err := cow.Close(); err != nil {
						t.Error(err)
					}
				}
				if err := cache.Close(); err != nil {
					t.Error(err)
				}
			}()
			start := time.Now()
			cow, err = OpenDisk(path, nil, DiffInit{Existing: true}, append(options, WithCache(cache))...)
			openNS := time.Since(start).Nanoseconds()
			if err != nil {
				t.Fatal(err)
			}
			backend, err := cow.checkpointBackend()
			if err != nil {
				t.Fatal(err)
			}
			beforeAdmission := backend.store.Stats()
			admissionStart := time.Now()
			for block := 0; block < 32; block++ {
				writePage(t, cow, block, byte(round+1))
			}
			awaitSignal(t, gate.entered)
			admissionNS := time.Since(admissionStart).Nanoseconds()
			pending := cache.Stats()
			beforeDrain := backend.store.Stats()
			if pending.DirtyUsed != 32*cowBlockSize || beforeDrain.DataWritten != beforeAdmission.DataWritten {
				t.Fatalf("expected exactly 32 pending pages and no completed data write: cache=%+v before=%+v after=%+v", pending, beforeAdmission, beforeDrain)
			}
			totalStart := time.Now()
			start = time.Now()
			if err := cow.WaitMaterialReady(ctx); err != nil {
				t.Fatal(err)
			}
			readyNS := time.Since(start).Nanoseconds()
			start = time.Now()
			gate.open()
			if err := cow.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			drainNS := time.Since(start).Nanoseconds()
			afterDrain := backend.store.Stats()
			if afterDrain.DataWritten-beforeDrain.DataWritten != 32*cowBlockSize {
				t.Fatalf("Drain did not commit exactly 32 data pages: before=%+v after=%+v", beforeDrain, afterDrain)
			}
			start = time.Now()
			if err := cow.WaitMaterialized(ctx); err != nil {
				t.Fatal(err)
			}
			materializedNS := time.Since(start).Nanoseconds()
			beforeCheckpoint := backend.store.Stats()
			start = time.Now()
			id := CheckpointID(fmt.Sprintf("vhost-scale-%d-%d", os.Getpid(), round))
			if err := cow.Checkpoint(ctx, id); err != nil {
				t.Fatal(err)
			}
			checkpointNS := time.Since(start).Nanoseconds()
			totalNS := time.Since(totalStart).Nanoseconds()
			afterCheckpoint := backend.store.Stats()
			if afterCheckpoint.DataRead != beforeCheckpoint.DataRead || afterCheckpoint.DataWritten != beforeCheckpoint.DataWritten {
				t.Fatal("checkpoint registration performed data I/O")
			}
			if err := cow.Close(); err != nil {
				t.Fatal(err)
			}
			cow = nil
			start = time.Now()
			reopened, err := OpenDisk(path, nil, DiffInit{Existing: true}, options...)
			reopenNS := time.Since(start).Nanoseconds()
			if err != nil {
				t.Fatal(err)
			}
			resumable := reopened.CanResumeCheckpoint(id)
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			if !resumable {
				t.Fatal("reopen lost checkpoint epoch")
			}
			row := map[string]any{
				"phase": "vhost-checkpoint", "history_gib": gib, "round": round,
				"pending_pages": 32, "pending_bytes": 32 * cowBlockSize,
				"open_ns": openNS, "admission_ns": admissionNS, "wait_ready_ns": readyNS,
				"drain_ns": drainNS, "materialized_ns": materializedNS,
				"checkpoint_ns": checkpointNS, "total_ns": totalNS, "reopen_ns": reopenNS,
				"before_drain": beforeDrain, "after_drain": afterDrain,
				"before_checkpoint": beforeCheckpoint, "after_checkpoint": afterCheckpoint,
				"note": "32 dirty pages gated before data write; total excludes initial Open/admission and final reopen; registration performs no payload I/O",
			}

			var usage unix.Rusage
			if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
				t.Fatal(err)
			}
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			row["process_cpu_user_ns"] = usage.Utime.Nano()
			row["process_cpu_system_ns"] = usage.Stime.Nano()
			row["process_peak_rss_kib"] = usage.Maxrss
			row["go_heap_bytes"] = memory.HeapAlloc
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Printf("vhost-scale %s\n", encoded)
		}()
	}
}

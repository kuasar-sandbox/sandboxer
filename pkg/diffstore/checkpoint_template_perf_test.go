package diffstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestCheckpointInitializationPhases compares fresh creation against the same
// current synchronous batching/bitmap baseline. Setup is outside the measured
// interval. Copy this probe unchanged to the control revision; it uses a type
// assertion only to observe the candidate's internal completion event.
func TestCheckpointInitializationPhases(t *testing.T) {
	if os.Getenv("CHECKPOINT_INIT_PERF") == "" {
		t.Skip("explicit initialization performance probe")
	}
	count, err := strconv.Atoi(os.Getenv("CHECKPOINT_INIT_DISKS"))
	if err != nil || count < 1 || count > 4 {
		t.Fatal("CHECKPOINT_INIT_DISKS must be 1..4")
	}
	action := os.Getenv("CHECKPOINT_INIT_ACTION")
	if action != "drain" && action != "checkpoint" && action != "close" {
		t.Fatal("CHECKPOINT_INIT_ACTION must be drain, checkpoint or close")
	}
	templated := os.Getenv("CHECKPOINT_INIT_TEMPLATE") == "1"
	encrypted := os.Getenv("CHECKPOINT_INIT_ENCRYPTED") == "1"
	const size = int64(32 << 30) // logical capacity only; this is not a 32 GiB data test
	dir := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil || fs.Type == unix.TMPFS_MAGIC {
		t.Fatal("requires native disk filesystem", err)
	}
	var init = DiffInit{CreateSize: size}
	if templated {
		templatePath := filepath.Join(dir, "template.raw")
		init.Template = rawTemplateFactory(templatePath)
		f, err := os.Create(templatePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(bytes.Repeat([]byte{0x61}, 64<<10), 0); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	cache, err := NewCache(DefaultCacheSize, DefaultMaxDirtySize)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	rows := make([]map[string]any, count)
	errorsFound := make([]error, count)
	var before, after runtime.MemStats
	var cpuBefore, cpuAfter unix.Rusage
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := unix.Getrusage(unix.RUSAGE_SELF, &cpuBefore); err != nil {
		t.Fatal(err)
	}
	ioBefore, _ := os.ReadFile("/proc/self/io")
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			row := map[string]any{"disk": i}
			rows[i] = row
			fail := func(err error) bool {
				if err != nil {
					errorsFound[i] = err
					return true
				}
				return false
			}
			options := []DiskOption{WithCheckpoint(fmt.Sprintf("init-%d", i), "base"), WithCache(cache)}
			if encrypted {
				options = append(options, WithDiffEncryption([32]byte{1}, true))
			}
			path := filepath.Join(dir, fmt.Sprintf("disk-%d.diff", i))
			opened := time.Now()
			cow, err := OpenDisk(path, presenceProbeBase{size}, init, options...)
			row["open_ns"] = time.Since(opened).Nanoseconds()
			if fail(err) {
				return
			}
			defer cow.Close()
			// Observers start at logical open and run independently of Drain;
			// sequential waits would hide Ready behind dirty-cache completion.
			type observation struct {
				ns  int64
				err error
			}
			var initialized, ready, published chan observation
			if action == "drain" {
				observe := func(wait func() error) chan observation {
					ch := make(chan observation, 1)
					go func() { err := wait(); ch <- observation{time.Since(opened).Nanoseconds(), err} }()
					return ch
				}
				initialized = observe(func() error {
					if owner, ok := any(cow.diff).(interface{ waitInitialized(context.Context) error }); ok {
						return owner.waitInitialized(context.Background())
					}
					return nil
				})
				ready = observe(func() error { return cow.WaitMaterialReady(context.Background()) })
				published = observe(func() error { return cow.WaitMaterialized(context.Background()) })
			}
			page := make([]byte, cowBlockSize)
			readAt := time.Now()
			_, err = cow.ReadAt(page, 0)
			row["first_read_call_ns"] = time.Since(readAt).Nanoseconds()
			row["open_to_first_read_ns"] = time.Since(opened).Nanoseconds()
			if fail(err) {
				return
			}
			want := byte(0xa7)
			if templated {
				want = 0x61
			}
			if page[0] != want {
				fail(fmt.Errorf("first lower byte %x != %x", page[0], want))
				return
			}
			writeAt := time.Now()
			_, err = cow.WriteAt([]byte{0x79}, 9)
			row["first_write_call_ns"] = time.Since(writeAt).Nanoseconds()
			row["open_to_first_write_ns"] = time.Since(opened).Nanoseconds()
			if fail(err) {
				return
			}
			if action == "close" {
				at := time.Now()
				err = cow.Close()
				row["immediate_close_ns"] = time.Since(at).Nanoseconds()
				if fail(err) {
					return
				}
			} else if action == "checkpoint" {
				at := time.Now()
				err = cow.Checkpoint(context.Background(), "first")
				row["immediate_checkpoint_ns"] = time.Since(at).Nanoseconds()
				if fail(err) {
					return
				}
			} else {
				at := time.Now()
				err = cow.Drain(context.Background())
				row["drain_ns"] = time.Since(at).Nanoseconds()
				if fail(err) {
					return
				}
				for name, ch := range map[string]chan observation{"initialization_observed_ns": initialized, "ready_observed_ns": ready, "published_observed_ns": published} {
					observed := <-ch
					if fail(observed.err) {
						return
					}
					row[name] = observed.ns
				}
			}
			var physical unix.Stat_t
			if fail(unix.Stat(path, &physical)) {
				return
			}
			row["allocated_bytes"], row["file_bytes"] = physical.Blocks*512, physical.Size
			if action != "close" {
				if _, err = cow.ReadAt(page, 0); fail(err) {
					return
				}
				if page[9] != 0x79 || page[8] != want {
					fail(fmt.Errorf("lost first write"))
					return
				}
				at := time.Now()
				err = cow.Close()
				row["close_ns"] = time.Since(at).Nanoseconds()
				if fail(err) {
					return
				}
			}
			row["total_ns"] = time.Since(opened).Nanoseconds()
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start).Nanoseconds()
	ioAfter, _ := os.ReadFile("/proc/self/io")
	if err := unix.Getrusage(unix.RUSAGE_SELF, &cpuAfter); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	result := map[string]any{"phase": "checkpoint-initialization", "label": os.Getenv("CHECKPOINT_INIT_LABEL"), "round": os.Getenv("CHECKPOINT_INIT_ROUND"), "disks": count, "template": templated, "encrypted": encrypted, "action": action, "logical_bytes": size, "rows": rows, "total_ns": elapsed, "cpu_user_ns": cpuAfter.Utime.Nano() - cpuBefore.Utime.Nano(), "cpu_system_ns": cpuAfter.Stime.Nano() - cpuBefore.Stime.Nano(), "allocated_bytes": after.TotalAlloc - before.TotalAlloc, "heap_inuse_bytes": after.HeapInuse, "max_rss_kib": cpuAfter.Maxrss, "proc_io_before": string(ioBefore), "proc_io_after": string(ioAfter), "cache": cache.Stats()}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("INITIALIZATION_RESULT %s\n", encoded)
	for _, err := range errorsFound {
		if err != nil {
			t.Error(err)
		}
	}
}

// TestCheckpointTemplatePhases is the opt-in entry point used by
// test/perf/checkpoint_template.py. Each invocation initializes and verifies a
// complete 64 MiB template on the actual filesystem, then times fresh target
// Open, Ready, materialization, and final publication separately.
func TestCheckpointTemplatePhases(t *testing.T) {
	mode := os.Getenv("VHOST_TEMPLATE_PERF_MODE")
	if mode == "" {
		t.Skip("requires explicit test/perf/checkpoint_template.py harness")
	}
	if mode != "raw" && mode != "v1" && mode != "v2" && mode != "v2-xts" {
		t.Fatal("invalid VHOST_TEMPLATE_PERF_MODE")
	}
	round, err := strconv.Atoi(os.Getenv("VHOST_TEMPLATE_PERF_ROUND"))
	if err != nil || round < 1 {
		t.Fatal("invalid VHOST_TEMPLATE_PERF_ROUND")
	}
	const size = int64(64 << 20)
	ctx := context.Background()
	dir := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type == unix.TMPFS_MAGIC {
		t.Fatal("template throughput requires a real disk filesystem")
	}
	var options []DiskOption
	if mode == "v1" || mode == "v2-xts" {
		options = append(options, WithDiffEncryption([32]byte{1}, true))
	}
	templatePath := filepath.Join(dir, "template.diff")
	// Provisioning input stays raw, independently of the active target format.
	// The historical mode names select target encryption/checkpoint policy.
	template, err := os.Create(templatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer template.Close()
	if err := template.Truncate(size); err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	if _, err := rand.New(rand.NewSource(327)).Read(chunk); err != nil {
		t.Fatal(err)
	}
	want := sha256.New()
	for off := int64(0); off < size; off += int64(len(chunk)) {
		if n, err := template.WriteAt(chunk, off); err != nil || n != len(chunk) {
			t.Fatal(n, err)
		}
		_, _ = want.Write(chunk)
	}
	if err := template.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := template.Close(); err != nil {
		t.Fatal(err)
	}
	if mode == "v2" || mode == "v2-xts" {
		options = append(options, WithCheckpoint("template-perf", ""))
	}
	path := filepath.Join(dir, "target.diff")
	var cpuBefore unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &cpuBefore); err != nil {
		t.Fatal(err)
	}
	totalStart := time.Now()
	start := time.Now()
	cow, err := OpenDisk(path, nil, DiffInit{CreateSize: size, Template: rawTemplateFactory(templatePath)}, options...)
	openNS := time.Since(start).Nanoseconds()
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()
	start = time.Now()
	if err := cow.WaitMaterialReady(ctx); err != nil {
		t.Fatal(err)
	}
	readyNS := time.Since(start).Nanoseconds()
	start = time.Now()
	if err := cow.WaitMaterialized(ctx); err != nil {
		t.Fatal(err)
	}
	materializedNS := time.Since(start).Nanoseconds()
	totalNS := time.Since(totalStart).Nanoseconds()
	var cpuAfter unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &cpuAfter); err != nil {
		t.Fatal(err)
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	var physical unix.Stat_t
	if err := unix.Stat(path, &physical); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{
		"phase": "vhost-template", "mode": mode, "round": round, "template_payload_bytes": size,
		"open_ns": openNS, "wait_ready_ns": readyNS, "materialized_ns": materializedNS, "total_ns": totalNS,
		"sustained_MB_per_second": float64(size) * 1e3 / float64(totalNS),
		"cpu_user_ns":             cpuAfter.Utime.Nano() - cpuBefore.Utime.Nano(),
		"cpu_system_ns":           cpuAfter.Stime.Nano() - cpuBefore.Stime.Nano(),
		"process_peak_rss_kib":    cpuAfter.Maxrss, "go_heap_bytes": memory.HeapAlloc,
		"allocated_file_bytes": physical.Blocks * 512, "file_bytes": physical.Size,
		"filesystem_type": fmt.Sprintf("0x%x", fs.Type),
		"note":            "64 MiB fully populated immutable raw source; historical mode names select target policy only; v1 and v2-xts share key/required policy; total includes final materialization publication and initial synchronization; source setup and final full SHA256 verification are outside timing; process peak RSS includes setup",
	}
	if backend, err := cow.checkpointBackend(); err == nil {
		stats := backend.store.Stats()
		if stats.DataWritten != uint64(size) {
			t.Fatalf("materializer committed %d data bytes, want %d", stats.DataWritten, size)
		}
		row["stats"] = stats
	}
	got := sha256.New()
	for off := int64(0); off < size; off += int64(len(chunk)) {
		if n, err := cow.ReadAt(chunk, off); err != nil || n != len(chunk) {
			t.Fatal(n, err)
		}
		_, _ = got.Write(chunk)
	}
	if !bytes.Equal(want.Sum(nil), got.Sum(nil)) {
		t.Fatal("materialized template SHA256 mismatch")
	}
	row["verified_sha256"] = hex.EncodeToString(got.Sum(nil))
	if err := cow.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("vhost-template %s\n", encoded)
}

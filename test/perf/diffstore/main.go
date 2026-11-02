// Command diffstore measures actual O_DIRECT payload initialization, fixed-root
// checkpoint registration, and presence reconstruction on a Linux filesystem.
// It is deliberately separate from the ordinary test suite: the default run
// writes 11 GiB and requires an explicit, empty task-owned output directory.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kuasar-sandbox/sandboxer/internal/buildversion"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"golang.org/x/sys/unix"
)

type sample struct {
	Phase       string          `json:"phase"`
	GiB         int             `json:"history_gib"`
	Round       int             `json:"round"`
	Nanoseconds int64           `json:"duration_ns"`
	Bytes       int64           `json:"payload_bytes,omitempty"`
	Allocated   int64           `json:"allocated_bytes,omitempty"`
	Physical    int64           `json:"file_bytes,omitempty"`
	Stats       diffstore.Stats `json:"stats"`
	Note        string          `json:"note,omitempty"`
	CPUUserNS   int64           `json:"process_cpu_user_ns"`
	CPUSystemNS int64           `json:"process_cpu_system_ns"`
	MaxRSSKiB   int64           `json:"process_peak_rss_kib"`
	HeapBytes   uint64          `json:"go_heap_bytes"`
}

func emit(enc *json.Encoder, value sample) error {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return err
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	value.CPUUserNS = usage.Utime.Nano()
	value.CPUSystemNS = usage.Stime.Nano()
	value.MaxRSSKiB = usage.Maxrss
	value.HeapBytes = memory.HeapAlloc
	return enc.Encode(value)
}

func main() {
	buildversion.ExitIfRequested()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	dir := flag.String("directory", "", "new task-owned directory on a real Linux filesystem")
	sizes := flag.String("sizes-gib", "0,1,10", "actual payload history sizes; files are removed between sizes")
	rounds := flag.Int("rounds", 5, "independent registrations and index reconstruction samples per size")
	vhostBinary := flag.String("vhost-test-binary", "", "optional compiled pkg/diffstore test binary for fixed-pending COW phase measurements")
	flag.Parse()
	if *dir == "" || *rounds < 1 {
		return fmt.Errorf("--directory and positive --rounds are required")
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if err = os.Mkdir(abs, 0700); err != nil {
		return fmt.Errorf("measurement requires a new directory: %w", err)
	}
	var fs unix.Statfs_t
	if err = unix.Statfs(abs, &fs); err != nil {
		return err
	}
	if fs.Type == unix.TMPFS_MAGIC {
		return fmt.Errorf("tmpfs cannot establish direct-I/O acceptance")
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(map[string]any{"phase": "environment", "directory": abs, "filesystem_type": fmt.Sprintf("0x%x", fs.Type), "free_bytes": uint64(fs.Bavail) * uint64(fs.Bsize), "rounds": *rounds, "clock": "monotonic wall time", "pending_payload_bytes": 0, "engine": "diffstore v2 O_DIRECT", "process_durability": "completed syscalls and replay; no host-power-loss claim", "go_version": runtime.Version(), "cpu_count": runtime.NumCPU(), "command": os.Args, "vhost_helper": *vhostBinary}); err != nil {
		return err
	}
	for _, word := range strings.Split(*sizes, ",") {
		gib, err := strconv.Atoi(word)
		if err != nil || gib < 0 || gib > 1024 {
			return fmt.Errorf("invalid history GiB %q", word)
		}
		if err = measure(enc, abs, gib, *rounds, *vhostBinary); err != nil {
			return err
		}
	}
	return nil
}
func measure(enc *json.Encoder, dir string, gib, rounds int, vhostBinary string) error {
	ctx := context.Background()
	path := filepath.Join(dir, fmt.Sprintf("history-%d.diff", gib))
	size := int64(gib) << 30
	logical := max(size, 1<<30)
	start := time.Now()
	s, err := diffstore.Create(path, diffstore.Options{DiffID: fmt.Sprintf("scaling-%d", gib), BaseID: "fixed-immutable-base", LogicalSize: logical})
	if err != nil {
		return err
	}
	defer func() { s.Close() }()
	pages := make([]diffstore.Page, 256)
	data := make([]byte, len(pages)*diffstore.PageSize)
	for i := range data {
		data[i] = byte(i*17 + 35)
	}
	for off := int64(0); off < size; {
		count := int(min(int64(len(pages)), (size-off)/diffstore.PageSize))
		for i := 0; i < count; i++ {
			block := uint64(off/diffstore.PageSize) + uint64(i)
			p := data[i*diffstore.PageSize : (i+1)*diffstore.PageSize]
			binary.LittleEndian.PutUint64(p, block)
			pages[i] = diffstore.Page{Block: block, Data: p}
		}
		if err = s.WritePages(ctx, pages[:count]); err != nil {
			return err
		}
		off += int64(count) * diffstore.PageSize
		if off%(1<<30) == 0 {
			if err = emit(enc, sample{Phase: "initialize-progress", GiB: gib, Nanoseconds: time.Since(start).Nanoseconds(), Bytes: off, Stats: s.Stats()}); err != nil {
				return err
			}
		}
	}
	initialize := time.Since(start)
	syncStart := time.Now()
	if err = s.Sync(); err != nil {
		return err
	}
	syncTime := time.Since(syncStart)
	var st unix.Stat_t
	if err = unix.Stat(path, &st); err != nil {
		return err
	}
	if err = emit(enc, sample{Phase: "initialize", GiB: gib, Nanoseconds: initialize.Nanoseconds(), Bytes: size, Allocated: st.Blocks * 512, Physical: st.Size, Stats: s.Stats(), Note: "Actual non-hole payload writes; synchronous engine has zero pending writes at measurement boundary"}); err != nil {
		return err
	}
	if err = emit(enc, sample{Phase: "initial-sync", GiB: gib, Nanoseconds: syncTime.Nanoseconds(), Note: "Initial publication sync; Checkpoint itself does not add fsync"}); err != nil {
		return err
	}
	for round := 0; round < rounds; round++ {
		before := s.Stats()
		start = time.Now()
		if err = s.Checkpoint(ctx, fmt.Sprintf("sample-%d", round)); err != nil {
			return err
		}
		elapsed := time.Since(start)
		after := s.Stats()
		if after.DataRead != before.DataRead || after.DataWritten != before.DataWritten {
			return fmt.Errorf("checkpoint scanned payload")
		}
		delta := diffstore.Stats{DataRead: after.DataRead - before.DataRead, DataWritten: after.DataWritten - before.DataWritten, IndexRead: after.IndexRead - before.IndexRead, IndexWritten: after.IndexWritten - before.IndexWritten, RedoWritten: after.RedoWritten - before.RedoWritten, CacheHits: after.CacheHits - before.CacheHits, CacheMisses: after.CacheMisses - before.CacheMisses}
		if err = emit(enc, sample{Phase: "checkpoint", GiB: gib, Round: round, Nanoseconds: elapsed.Nanoseconds(), Stats: delta, Note: "Metadata registration with zero queued payload; template wait and Drain are in the separate vhost helper"}); err != nil {
			return err
		}
	}
	if err = s.Close(); err != nil {
		return err
	}
	for round := 0; round < rounds; round++ {
		start = time.Now()
		s, err = diffstore.Open(path, diffstore.Options{})
		if err != nil {
			return err
		}
		openTime := time.Since(start)
		start = time.Now()
		bitmap := make([]uint64, (logical/diffstore.PageSize+63)/64)
		var count uint64
		if err = s.Scan(ctx, func(block uint64) error { bitmap[block/64] |= uint64(1) << (block % 64); count++; return nil }); err != nil {
			return err
		}
		scanTime := time.Since(start)
		if count != uint64(size/diffstore.PageSize) {
			return fmt.Errorf("presence reconstruction has %d blocks, expected %d", count, size/diffstore.PageSize)
		}
		if s.Stats().DataRead != 0 {
			return fmt.Errorf("presence reconstruction read payload")
		}
		if err = emit(enc, sample{Phase: "reopen-control", GiB: gib, Round: round, Nanoseconds: openTime.Nanoseconds()}); err != nil {
			return err
		}
		if err = emit(enc, sample{Phase: "reopen-presence", GiB: gib, Round: round, Nanoseconds: scanTime.Nanoseconds(), Stats: s.Stats(), Note: fmt.Sprintf("%d mapped blocks; %d-byte bitmap; cost scales with index size", count, len(bitmap)*8)}); err != nil {
			return err
		}
		if err = s.Close(); err != nil {
			return err
		}
	}
	if vhostBinary != "" {
		cmd := exec.Command(vhostBinary, "-test.run=^TestCheckpointScalePhases$", "-test.v", "-test.timeout=30m")
		cmd.Env = append(os.Environ(), "DIFFSTORE_SCALE_PATH="+path, "DIFFSTORE_SCALE_GIB="+strconv.Itoa(gib), "DIFFSTORE_SCALE_ROUNDS="+strconv.Itoa(rounds))
		out, runErr := cmd.CombinedOutput()
		logPath := filepath.Join(dir, fmt.Sprintf("vhost-scale-%d.log", gib))
		if err = os.WriteFile(logPath, out, 0600); err != nil {
			return err
		}
		if runErr != nil {
			return fmt.Errorf("vhost scale phases failed (see %s): %w", logPath, runErr)
		}
		for _, line := range bytes.Split(out, []byte{'\n'}) {
			if p := bytes.IndexByte(line, '{'); p >= 0 && json.Valid(line[p:]) {
				if err = enc.Encode(json.RawMessage(line[p:])); err != nil {
					return err
				}
			}
		}
	}
	// Only the exact files this invocation created are removed. Raw samples stay
	// on stdout, suitable for attaching to the PR/Issue rather than source docs.
	if err = os.Remove(path); err != nil {
		return err
	}
	return os.Remove(path + ".lock")
}

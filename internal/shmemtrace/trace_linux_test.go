package shmemtrace

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"golang.org/x/sys/unix"
)

func TestAtomicReservationEncoding(t *testing.T) {
	var out bytes.Buffer
	if _, err := fetchAdd(asm.R0, asm.R1).Marshal(&out, binary.LittleEndian); err != nil {
		t.Fatal(err)
	}
	b := out.Bytes()
	if len(b) != 8 || b[0] != 0xdb || binary.LittleEndian.Uint32(b[4:]) != 1 {
		t.Fatalf("FETCH_ADD encoding = %x", b)
	}
	if binary.Size(event{}) != eventSize {
		t.Fatalf("event wire size %d", binary.Size(event{}))
	}
}

func TestDefaultOffAndStrictInput(t *testing.T) {
	t.Setenv(Env, "")
	var rows []string
	logf := func(f string, args ...any) { rows = append(rows, fmt.Sprintf(f, args...)) }
	if tr := ArmShmemAllocationTraceFromEnv(-1, -1, logf); tr != nil || len(rows) != 0 {
		t.Fatal("default-off performed work")
	}
	for _, raw := range []string{"", " 4194304", "+1", "-1", "0x", "1_024", "4M", "18446744073709551616"} {
		if _, err := ParsePage(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"4194304", "0x400000", "0X400000"} {
		if v, err := ParsePage(raw); err != nil || v != 4194304 {
			t.Errorf("rejected valid %q", raw)
		}
	}
	t.Setenv(Env, "+4194304")
	if tr := ArmShmemAllocationTraceFromEnv(-1, 8<<20, logf); tr != nil || len(rows) != 1 {
		t.Fatal("malformed setting not failed once")
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(rows[0], Prefix)), &event); err != nil {
		t.Fatal(err)
	}
	if event["event"] != "failed" || event["seq"] != float64(1) || event["stage"] != "parse" {
		t.Fatalf("bad failure %v", event)
	}
}

func TestBoundedSymbolsAndMessage(t *testing.T) {
	var rows []string
	tr := &Trace{common: common(4194304), logf: func(f string, args ...any) { rows = append(rows, fmt.Sprintf(f, args...)) }, syms: []symbol{{0x1000, "shmem_fault"}}}
	if tr.resolve(0x1001) != "shmem_fault+0x1" || tr.resolve(1) != "unknown" {
		t.Fatal("symbol resolution")
	}
	frames := make([]string, 32)
	for i := range frames {
		frames[i] = strings.Repeat("x", 96)
	}
	tr.emit("insert", map[string]any{"frames": frames, "comm": strings.Repeat("x", 16), "pid_scope": "initial", "inserter_pid": uint32(1<<31 - 1), "inserter_tid": uint32(1<<31 - 1), "index": uint64(1<<63 - 1), "nr_pages": 1048576, "ktime_ns": uint64(1<<64 - 1), "result": int64(-4095), "stack_errno": int64(-4095)})
	if len(rows) != 1 || len(rows[0]) >= 4096 {
		t.Fatal("unbounded record")
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(rows[0], Prefix)), &row); err != nil {
		t.Fatal(err)
	}
	if len(row["frames"].([]any)) > 32 {
		t.Fatal("unbounded frames")
	}
}

// Native tests require explicit opt-in plus BPF/perf privilege. Normal package
// tests exercise the default-off path without trying privileged syscalls.
func TestNativeLifetimeAndStartupCleanup(t *testing.T) {
	if os.Getenv("SHMEMTRACE_NATIVE_TEST") != "1" {
		t.Skip("native BPF control not requested")
	}
	fd, err := unix.MemfdCreate("shmem-native-cleanup", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err = unix.Ftruncate(fd, 8<<20); err != nil {
		t.Fatal(err)
	}
	countFD := func() int {
		v, e := os.ReadDir("/proc/self/fd")
		if e != nil {
			t.Fatal(e)
		}
		return len(v)
	}
	warm, err := arm(fd, 8<<20, 4<<20, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = warm.Close(); err != nil {
		t.Fatal(err)
	}
	before := countFD()
	closed := make(chan struct{})
	var mu sync.Mutex
	var rows []map[string]any
	tr, err := arm(fd, 8<<20, 4<<20, func(f string, args ...any) {
		var row map[string]any
		if e := json.Unmarshal([]byte(strings.TrimPrefix(fmt.Sprintf(f, args...), Prefix)), &row); e != nil {
			panic(e)
		}
		mu.Lock()
		rows = append(rows, row)
		mu.Unlock()
		if row["event"] == "closed" {
			close(closed)
		}
	}, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic close did not run")
	}
	if err = tr.Close(); err != nil {
		t.Fatal(err)
	}
	if tr.timer.Stop() {
		t.Fatal("timer remained armed after automatic close")
	}
	if len(tr.syms) != 0 {
		t.Fatal("symbol storage retained after close")
	}
	mu.Lock()
	if len(rows) != 2 || rows[0]["event"] != "armed" || rows[1]["event"] != "closed" {
		t.Fatalf("bad lifetime records %v", rows)
	}
	mu.Unlock()
	if after := countFD(); before != after {
		t.Fatalf("automatic-close FD leak %d -> %d", before, after)
	}
	manual, err := arm(fd, 8<<20, 4<<20, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = manual.Close(); err != nil {
		t.Fatal(err)
	}
	if manual.timer.Stop() {
		t.Fatal("manual Close failed to stop timer")
	}
	if after := countFD(); before != after {
		t.Fatalf("manual-close FD leak %d -> %d", before, after)
	}
	calls := 0
	bad, err := armWithAttach(fd, 8<<20, 4<<20, nil, time.Hour, func(p *ebpf.Program, ret bool) (*perfProbe, error) {
		calls++
		if !ret {
			return nil, unix.EINVAL
		}
		return attachPMU(p, true)
	})
	var stage *stageError
	if bad != nil || !errors.As(err, &stage) || stage.stage != "attach_entry" || calls != 2 {
		t.Fatalf("unexpected injected startup failure %v", err)
	}
	if after := countFD(); before != after {
		t.Fatalf("startup cleanup FD leak %d -> %d", before, after)
	}
	p, err := ebpf.NewProgram(&ebpf.ProgramSpec{Name: "closed_probe", Type: ebpf.Kprobe, License: "GPL", Instructions: asm.Instructions{asm.Mov.Imm(asm.R0, 0), asm.Return()}})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	if probe, err := attachPMU(p, true); err == nil {
		_ = probe.Close()
		t.Fatal("closed program unexpectedly attached")
	}
	if after := countFD(); before != after {
		t.Fatalf("PMU ioctl failure FD leak %d -> %d", before, after)
	}
}

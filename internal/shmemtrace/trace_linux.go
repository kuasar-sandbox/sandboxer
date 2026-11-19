package shmemtrace

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"
)

const Env = "SANDBOX_UFFD_TRACE_SHMEM_PAGE"
const Prefix = "uffd-shmem-allocation "
const outputLimit = 16
const traceDuration = 30 * time.Second

type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

type symbol struct {
	addr uint64
	name string
}
type Trace struct {
	common                 map[string]any
	logf                   func(string, ...any)
	seq                    uint64
	emitted, dropped       uint64
	readErr                error
	pending, stats, events *ebpf.Map
	entryProg, returnProg  *ebpf.Program
	entryLink, returnLink  *perfProbe
	reader                 *ringbuf.Reader
	done                   chan struct{}
	syms                   []symbol
	once                   sync.Once
	closeErr               error
	timer                  *time.Timer
}

func errno(err error) int {
	if err == nil {
		return 0
	}
	var e unix.Errno
	if errors.As(err, &e) && e <= 4095 {
		return int(e)
	}
	return -1
}
func common(page uint64) map[string]any {
	return map[string]any{"v": 1, "seq": uint64(0), "event": "", "pid": os.Getpid(), "page": page, "memfd_dev": uint64(0), "memfd_ino": uint64(0)}
}

// ParsePage is intentionally the same narrow decimal/explicit-hex grammar as
// the bounded UFFD diagnostics. Invalid configuration never changes startup.
func ParsePage(raw string) (uint64, error) {
	if len(raw) == 0 || len(raw) > 20 {
		return 0, fmt.Errorf("invalid page")
	}
	base, digits := 10, raw
	if strings.HasPrefix(raw, "0x") || strings.HasPrefix(raw, "0X") {
		base, digits = 16, raw[2:]
	}
	if digits == "" {
		return 0, fmt.Errorf("invalid page")
	}
	for _, c := range digits {
		if c >= '0' && c <= '9' || base == 16 && (c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			continue
		}
		return 0, fmt.Errorf("invalid page")
	}
	return strconv.ParseUint(digits, base, 64)
}

func ArmShmemAllocationTraceFromEnv(fd int, size int, logf func(string, ...any)) *Trace {
	raw := os.Getenv(Env)
	if raw == "" {
		return nil
	}
	page, err := ParsePage(raw)
	stage := "parse"
	var t *Trace
	if err == nil {
		t, err = Arm(fd, size, page, logf)
		if e := new(stageError); errors.As(err, &e) {
			stage = e.stage
		}
	}
	if err == nil {
		return t
	}
	c := common(page)
	if size > 0 {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil {
			c["memfd_dev"] = uint64(st.Dev)
			c["memfd_ino"] = st.Ino
		}
	}
	x := &Trace{common: c, logf: logf}
	x.emit("failed", map[string]any{"stage": stage, "errno": errno(err)})
	return nil
}

// Arm attaches only ephemeral PMU event FDs owned by this process and schedules
// their closure after 30 seconds. It does not write tracefs, pin maps/links, install tools,
// or change any host setting.
func Arm(fd int, size int, page uint64, logf func(string, ...any)) (*Trace, error) {
	return arm(fd, size, page, logf, traceDuration)
}

func arm(fd int, size int, page uint64, logf func(string, ...any), duration time.Duration) (*Trace, error) {
	return armWithAttach(fd, size, page, logf, duration, attachPMU)
}

func armWithAttach(fd int, size int, page uint64, logf func(string, ...any), duration time.Duration, attach func(*ebpf.Program, bool) (*perfProbe, error)) (_ *Trace, retErr error) {
	t := &Trace{common: common(page), logf: logf}
	defer func() {
		if retErr != nil {
			t.cleanup()
		}
	}()
	fail := func(stage string, err error) (*Trace, error) { return nil, &stageError{stage, err} }
	if os.Getpagesize() != 4096 || size <= 0 || size%4096 != 0 || page%4096 != 0 || page > uint64(size) || 4096 > uint64(size)-page {
		return fail("parse", fmt.Errorf("unsupported page range"))
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fail("identity", err)
	}
	if st.Size != int64(size) || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fail("identity", fmt.Errorf("unexpected backing size or type"))
	}
	t.common["memfd_dev"] = uint64(st.Dev)
	t.common["memfd_ino"] = st.Ino
	start := uint64(0)
	if page >= 8192 {
		start = page - 8192
	}
	end := page + 8192
	if end > uint64(size) {
		end = uint64(size)
	}
	l, err := kernelLayout()
	if err != nil {
		return fail("btf", err)
	}
	kdev := uint32(unix.Major(uint64(st.Dev))<<20 | unix.Minor(uint64(st.Dev)))
	t.pending, err = ebpf.NewMap(&ebpf.MapSpec{Name: "shmem_pending", Type: ebpf.Hash, KeySize: 8, ValueSize: eventSize, MaxEntries: kernelLimit})
	if err != nil {
		return fail("maps", err)
	}
	t.stats, err = ebpf.NewMap(&ebpf.MapSpec{Name: "shmem_stats", Type: ebpf.Array, KeySize: 4, ValueSize: 32, MaxEntries: 1})
	if err != nil {
		return fail("maps", err)
	}
	t.events, err = ebpf.NewMap(&ebpf.MapSpec{Name: "shmem_events", Type: ebpf.RingBuf, MaxEntries: 32768})
	if err != nil {
		return fail("maps", err)
	}
	t.entryProg, err = ebpf.NewProgram(entryProgram(l, t.pending, t.stats, kdev, st.Ino, start, end))
	if err != nil {
		return fail("program", err)
	}
	t.returnProg, err = ebpf.NewProgram(returnProgram(l, t.pending, t.stats, t.events))
	if err != nil {
		return fail("program", err)
	}
	t.reader, err = ringbuf.NewReader(t.events)
	if err != nil {
		return fail("reader", err)
	}
	t.syms = loadSymbols()
	// Attach returns first: no entry can be tracked without an exit observer.
	t.returnLink, err = attach(t.returnProg, true)
	if err != nil {
		return fail("attach_return", err)
	}
	t.entryLink, err = attach(t.entryProg, false)
	if err != nil {
		return fail("attach_entry", err)
	}
	t.done = make(chan struct{})
	t.emit("armed", map[string]any{"window_start": start, "window_end": end, "page_size": 4096, "max_records": outputLimit})
	go t.read()
	// Publish the timer before allowing its callback to close the trace, even
	// when a short duration is injected by the native lifecycle tests.
	ready := make(chan struct{})
	t.timer = time.AfterFunc(duration, func() { <-ready; _ = t.Close() })
	close(ready)
	return t, nil
}

func (t *Trace) emit(kind string, extra map[string]any) {
	t.seq++
	r := make(map[string]any, len(t.common)+len(extra))
	for k, v := range t.common {
		r[k] = v
	}
	for k, v := range extra {
		r[k] = v
	}
	r["seq"], r["event"] = t.seq, kind
	b, _ := json.Marshal(r)
	for len(Prefix)+len(b) >= 4096 {
		f, ok := r["frames"].([]string)
		if !ok || len(f) == 0 {
			return
		}
		r["frames"] = f[:len(f)-1]
		b, _ = json.Marshal(r)
	}
	if t.logf != nil {
		t.logf("%s%s", Prefix, b)
	}
}

func (t *Trace) read() {
	defer close(t.done)
	for {
		r, err := t.reader.Read()
		if err != nil {
			if !errors.Is(err, ringbuf.ErrFlushed) && !errors.Is(err, os.ErrClosed) {
				t.readErr = err
			}
			return
		}
		var e event
		if len(r.RawSample) != eventSize || binary.Read(bytes.NewReader(r.RawSample), binary.NativeEndian, &e) != nil {
			t.dropped++
			continue
		}
		if t.emitted >= outputLimit {
			t.dropped++
			continue
		}
		stackErr := int64(0)
		count := 0
		if e.StackSize < 0 {
			stackErr = e.StackSize
			if stackErr < -4095 {
				stackErr = -1
			}
		} else if e.StackSize <= 256 {
			count = int(e.StackSize) / 8
		}
		frames := make([]string, 0, count)
		for _, pc := range e.Stack[:count] {
			frames = append(frames, t.resolve(pc))
		}
		comm := string(bytes.TrimRight(e.Comm[:], "\x00"))
		comm = strings.Map(func(c rune) rune {
			if c < 32 || c > 126 {
				return '?'
			}
			return c
		}, comm)
		t.emit("insert", map[string]any{"pid_scope": "initial", "inserter_pid": uint32(e.PIDTID >> 32), "inserter_tid": uint32(e.PIDTID), "comm": comm, "index": e.Index, "nr_pages": e.Pages, "ktime_ns": e.Ktime, "result": e.Result, "stack_errno": stackErr, "frames": frames})
		t.emitted++
	}
}

func (t *Trace) pendingCount() (uint64, error) {
	var k uint64
	var v event
	var n uint64
	it := t.pending.Iterate()
	for it.Next(&k, &v) {
		n++
		if n >= kernelLimit {
			break
		}
	}
	return n, it.Err()
}

func (t *Trace) Close() error {
	if t == nil {
		return nil
	}
	t.once.Do(func() {
		if t.timer != nil {
			t.timer.Stop()
		}
		var pending uint64
		deadline := time.Now().Add(250 * time.Millisecond)
		for {
			var err error
			pending, err = t.pendingCount()
			if err != nil {
				t.closeErr = errors.Join(t.closeErr, err)
				break
			}
			if pending == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		// Keep entry/return pairing intact while draining. Stop returns first:
		// entries racing shutdown can remain pending, but an untracked nested
		// return must never consume the outer call's pending event.
		if t.returnLink != nil {
			t.closeErr = errors.Join(t.closeErr, t.returnLink.Close())
			t.returnLink = nil
		}
		if t.entryLink != nil {
			t.closeErr = errors.Join(t.closeErr, t.entryLink.Close())
			t.entryLink = nil
		}
		var err error
		pending, err = t.pendingCount()
		t.closeErr = errors.Join(t.closeErr, err)
		if err := t.reader.Flush(); err != nil {
			t.closeErr = errors.Join(t.closeErr, err)
			_ = t.reader.Close()
		}
		<-t.done
		var stats [4]uint64
		key := uint32(0)
		if err := t.stats.Lookup(key, &stats); err != nil {
			t.closeErr = errors.Join(t.closeErr, err)
		}
		dropped := t.dropped + stats[1] + stats[3]
		if stats[0] > kernelLimit {
			dropped += stats[0] - kernelLimit
		}
		t.closeErr = errors.Join(t.closeErr, t.readErr)
		t.emit("closed", map[string]any{"matched": stats[0], "emitted": t.emitted, "dropped": dropped, "pending": pending, "nested": stats[2], "entry_missed": int64(-1), "return_missed": int64(-1), "errno": errno(t.closeErr)})
		t.cleanup()
	})
	return t.closeErr
}

func (t *Trace) cleanup() {
	if t.returnLink != nil {
		_ = t.returnLink.Close()
		t.returnLink = nil
	}
	if t.entryLink != nil {
		_ = t.entryLink.Close()
		t.entryLink = nil
	}
	if t.reader != nil {
		_ = t.reader.Close()
	}
	if t.entryProg != nil {
		_ = t.entryProg.Close()
	}
	if t.returnProg != nil {
		_ = t.returnProg.Close()
	}
	for _, m := range []*ebpf.Map{t.pending, t.stats, t.events} {
		if m != nil {
			_ = m.Close()
		}
	}
	t.syms = nil
}

func loadSymbols() []symbol {
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []symbol
	s := bufio.NewScanner(f)
	for s.Scan() {
		p := strings.Fields(s.Text())
		if len(p) < 3 || !strings.Contains("tTwW", p[1]) {
			continue
		}
		v, e := strconv.ParseUint(p[0], 16, 64)
		if e != nil || v == 0 {
			continue
		}
		name := p[2]
		if len(name) > 80 {
			continue
		}
		valid := true
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.$:/-", c)) {
				valid = false
				break
			}
		}
		if valid {
			out = append(out, symbol{v, name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].addr < out[j].addr })
	return out
}
func (t *Trace) resolve(pc uint64) string {
	i := sort.Search(len(t.syms), func(i int) bool { return t.syms[i].addr > pc }) - 1
	if i < 0 {
		return "unknown"
	}
	v := t.syms[i]
	off := pc - v.addr
	if off > 1<<20 {
		return "unknown"
	}
	s := fmt.Sprintf("%s+0x%x", v.name, off)
	if len(s) > 96 {
		return "unknown"
	}
	return s
}

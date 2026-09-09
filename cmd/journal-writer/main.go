// journal-writer is the test harness for the FE2026072900156 performance
// test plan: it drives sandbox-ctl's journald write path at controlled
// rate / size / concurrency so we can measure the journal entry ceiling
// (Test 1-A), sandbox-ctl's full stdout read+split+send path (Test 1-B),
// N-way service fan-in (Test 2), and the rsyslog dynaFile routing chain
// (Test 3).
//
// It intentionally mirrors sandbox-ctl's journaldWriter (pkg/stdio/stdio.go):
// build the fields map (SYSLOG_IDENTIFIER + KUASAR_*), then hand each line
// to journal.Send with PriInfo. The fields map and the journal.Send
// invocation must match sandbox-ctl exactly: rsyslog's dynaFile template
// (§1.2) depends on KUASAR_SANDBOX_ID being present.
//
// It self-reports lines/s, bytes/s, drop count, latency p50/p99. That
// self-report is the only way to detect silent journal drops that the
// kernel socket buffer will happily hide: journal.Send returns nil for
// messages that were never stored (see §3.4 three-way comparison).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/journal"
)

// buildLine returns the pre-computed log line: (lineSize-1) 'x' bytes +
// a single trailing newline, exactly the framing sandbox-ctl uses on its
// stdout reader (§3.1 code sample). Build once, reuse for every send.
func buildLine(lineSize int) string {
	if lineSize < 2 {
		lineSize = 2
	}
	return strings.Repeat("x", lineSize-1) + "\n"
}

// sender is the single journal call site. Everything else is scaffolding
// to drive it the way sandbox-ctl does (§3.1 code sample).
type sender struct {
	fields map[string]string
}

func newSender(tag, sandboxID, runID string) *sender {
	fields := map[string]string{"SYSLOG_IDENTIFIER": tag}
	if sandboxID != "" {
		fields["KUASAR_SANDBOX_ID"] = sandboxID
	}
	if runID != "" {
		fields["KUASAR_RUN_ID"] = runID
	}
	return &sender{fields: fields}
}

func (s *sender) send(line string) error {
	return journal.Send(line, journal.PriInfo, s.fields)
}

// stats is the writer-side observation channel. It exists because
// journal.Send returns nil even when the kernel dropped the message on the
// floor (§3.4: "journal.Send 成功 != 已存进 journal"). We surface drop
// counts so the operator can run the three-way count comparison.
type stats struct {
	sent    atomic.Int64
	dropped atomic.Int64
	hist    *hist
}

func newStats() *stats {
	return &stats{hist: newHist()}
}

func (s *stats) record(d time.Duration, err error) {
	if err != nil {
		s.dropped.Add(1)
	}
	s.sent.Add(1)
	s.hist.record(d)
}

func (s *stats) report(start time.Time, lineSize int) {
	elapsed := time.Since(start)
	sec := elapsed.Seconds()
	if sec <= 0 {
		sec = 1
	}
	sent := s.sent.Load()
	dropped := s.dropped.Load()
	lines := float64(sent) / sec
	bytes := float64(sent) * float64(lineSize) / sec
	dropPct := 0.0
	if sent > 0 {
		dropPct = 100 * float64(dropped) / float64(sent)
	}
	fmt.Printf("sent=%d dropped=%d drop_rate=%.2f%% lines/s=%.2f bytes/s=%.2f p50=%s p99=%s\n",
		sent, dropped, dropPct, lines, bytes,
		s.hist.quantile(0.50).String(), s.hist.quantile(0.99).String())
}

// hist is a fixed-bound log-bucketed latency recorder. Unbounded slices
// blow up RAM at 100k lines/s (§3.1 says record per journal.Send call),
// so we bucket and interpolate. Bounds are µs-aligned so p50/p99 stay
// meaningful even for the fastest calls.
type hist struct {
	bounds []int64
	counts []int64
	total  atomic.Int64
}

func newHist() *hist {
	bounds := []int64{
		1_000, 2_000, 5_000,
		10_000, 20_000, 50_000,
		100_000, 200_000, 500_000,
		1_000_000, 2_000_000, 5_000_000,
		10_000_000, 20_000_000, 50_000_000,
		100_000_000, 200_000_000, 500_000_000,
		1_000_000_000, 5_000_000_000, 10_000_000_000,
	}
	return &hist{
		bounds: bounds,
		counts: make([]int64, len(bounds)+1),
	}
}

func (h *hist) record(d time.Duration) {
	if d < 0 {
		d = 0
	}
	ns := d.Nanoseconds()
	i := sort.Search(len(h.bounds), func(i int) bool { return h.bounds[i] >= ns })
	if i == len(h.bounds) {
		i = len(h.bounds) - 1
	}
	h.counts[i]++
	h.total.Add(1)
}

func (h *hist) quantile(p float64) time.Duration {
	if p <= 0 {
		return 0
	}
	total := h.total.Load()
	if total == 0 {
		return 0
	}
	target := int64(p*float64(total) + 0.5)
	cum := int64(0)
	for i, c := range h.counts {
		cum += c
		if cum >= target {
			return time.Duration(h.bounds[i])
		}
	}
	return time.Duration(h.bounds[len(h.bounds)-1])
}

func main() {
	rate := flag.Int("rate", 0, "lines per second; 0 = max rate, no sleep")
	lineSize := flag.Int("line-size", 200, "bytes per line (must be >= 2)")
	duration := flag.Int("duration", 600, "test duration in seconds")
	tag := flag.String("tag", "sandbox", "SYSLOG_IDENTIFIER field value")
	sandboxID := flag.String("sandbox-id", "", "KUASAR_SANDBOX_ID field value")
	runID := flag.String("run-id", "", "KUASAR_RUN_ID field value")
	pipeMode := flag.Bool("pipe-mode", false, "simulate sandbox-ctl full path: stdout pipe → io.Copy → newline split → journal.Send")
	flag.Parse()

	if *lineSize < 2 {
		fmt.Fprintln(os.Stderr, "journal-writer: --line-size must be >= 2")
		os.Exit(2)
	}
	if *duration <= 0 {
		fmt.Fprintln(os.Stderr, "journal-writer: --duration must be > 0")
		os.Exit(2)
	}
	if !journal.Enabled() {
		fmt.Fprintln(os.Stderr, "journal-writer: journald socket not available (is systemd-journald running?)")
		os.Exit(2)
	}

	s := newSender(*tag, *sandboxID, *runID)
	st := newStats()
	line := buildLine(*lineSize)
	lineBytes := []byte(line)
	deadline := time.Now().Add(time.Duration(*duration) * time.Second)

	var tickCh <-chan time.Time
	if *rate > 0 {
		ticker := time.NewTicker(time.Second / time.Duration(*rate))
		defer ticker.Stop()
		tickCh = ticker.C
	}

	// Graceful exit on SIGINT/SIGTERM (e.g. operator Ctrl+C). Without this
	// the writer goroutine would keep ticking to a far-future deadline
	// after the operator wants the test to stop.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	stopNow := make(chan struct{})
	var stopOnce atomic.Bool
	go func() {
		select {
		case <-sigCh:
			stopOnce.Store(true)
			close(stopNow)
		case <-stopNow:
		}
	}()

	start := time.Now()
	done := make(chan struct{})

	go func() {
		defer close(done)
		if *pipeMode {
			runPipe(s, lineBytes, tickCh, deadline, stopNow, st)
		} else {
			runDirect(s, line, tickCh, deadline, stopNow, st)
		}
	}()

	<-done
	st.report(start, *lineSize)
}

// runDirect: generate line → journal.Send. No io.Copy, no newline split,
// just the raw entry path. The floor: how fast can journald accept?
func runDirect(s *sender, line string, tickCh <-chan time.Time, deadline time.Time, stopNow <-chan struct{}, st *stats) {
	for {
		if tickCh != nil {
			select {
			case <-tickCh:
			case <-stopNow:
				return
			}
		}
		if time.Now().After(deadline) {
			return
		}
		t0 := time.Now()
		err := s.send(line)
		st.record(time.Since(t0), err)
	}
}

// runPipe: simulate sandbox-ctl's full stdout path.
//
// goroutine A (writer): generate log lines, write to an os.Pipe write end.
// This is the sandbox process writing to its stdout pipe — the channel's
// buffer is 64KB, so a slow reader will backpressure the writer.
//
// goroutine B (reader): io.Copy-style chunked read, split by '\n', send
// each line to journal. This is sandbox-ctl's loop in stdio.go:263-278.
//
// The pipe is the sandbox stdout; the reader is sandbox-ctl. The
// writer→reader gap is the "stdout pipe + read + split" overhead that
// Test 1-B is designed to isolate against Test 1-A.
func runPipe(s *sender, line []byte, tickCh <-chan time.Time, deadline time.Time, stopNow <-chan struct{}, st *stats) {
	pr, pw, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "journal-writer: os.Pipe: %v\n", err)
		return
	}
	defer pr.Close()

	// goroutine A: writer side of the stdout pipe.
	//
	// CRITICAL: the writer goroutine owns pw and MUST close it on its way
	// out. Without that close, the reader blocks forever on br.ReadString
	// waiting for data that will never arrive — even if the main goroutine
	// also has a `defer pw.Close()`, that defer is blocked on the same
	// br.ReadString.
	//
	// The convention here: the writer closes pw (it owns the write end and
	// is the only one who knows when it's done), the reader just reads and
	// treats EOF as "writer is done, drain and stop".
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer pw.Close()
		for {
			if tickCh != nil {
				select {
				case <-tickCh:
				case <-stopNow:
					return
				}
			}
			if time.Now().After(deadline) {
				return
			}
			if _, err := pw.Write(line); err != nil {
				// pipe broken: stop, let reader drain.
				return
			}
		}
	}()

	// goroutine B: reader (sandbox-ctl) loop.
	//
	// Match sandbox-ctl: bufio.Reader with a generous buffer, ReadString('\n'),
	// TrimRight "\r", skip empty, journal.Send(line, PriInfo, fields).
	//
	// Note the asymmetry with the §3.1 code sample: in pipe mode the reader
	// strips the trailing "\n" before sending (sandbox-ctl's framing rule at
	// stdio.go:272), so the MESSAGE sent to journald is lineSize-1 bytes.
	// In non-pipe mode we send the full lineSize-byte string including the
	// trailing newline (per §3.1). The 1-byte delta is deliberate.
	br := bufio.NewReaderSize(pr, 1<<20) // 1MB, matches --line-size upper bound
	for {
		chunk, rerr := br.ReadString('\n')
		if len(chunk) > 0 {
			content := strings.TrimSuffix(chunk, "\n")
			content = strings.TrimRight(content, "\r")
			if content != "" {
				t0 := time.Now()
				serr := s.send(content)
				st.record(time.Since(t0), serr)
			}
		}
		if rerr != nil {
			break
		}
	}
	<-writerDone
}

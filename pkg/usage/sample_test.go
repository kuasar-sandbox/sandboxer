package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time                                  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Ticker(time.Duration) (<-chan time.Time, func()) { panic("not scheduled") }

func samplerFixture(t *testing.T) *Sampler {
	t.Helper()
	c := &testClock{now: time.Now()}
	p, err := newProcReader(2)
	if err != nil {
		t.Fatal(err)
	}
	s := &Sampler{m: testManager(&faultWriter{}), clock: c, start: c.now, interval: time.Second,
		proc: p, guest: &guestlink.UsageClient{}, epoch: "epoch", sources: make(map[string]string)}
	for _, name := range []string{"ch.cpu", "guest.cpu", "guest.vcpu.0", "guest.vcpu.1", "sandbox_ctl.cpu"} {
		if err := s.m.Counter(name, "old/"+name, 10, 100, true); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestTickerJitterAndMissingSlots(t *testing.T) {
	for _, tc := range []struct {
		at   time.Duration
		want int64
	}{
		{0, 0}, {time.Second + 1, 1}, {2*time.Second - 1, 2},
		{3*time.Second + 3500, 3}, {5*time.Second - 3500, 5},
	} {
		if got := tickSlot(tc.at, time.Second); got != tc.want {
			t.Fatalf("at=%s: %d != %d", tc.at, got, tc.want)
		}
	}
	// Exercise real timer delivery as well; this is not a VM/performance test.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	first := <-ticker.C
	for i := int64(1); i < 6; i++ {
		at := <-ticker.C
		slot := tickSlot(at.Sub(first), 20*time.Millisecond)
		if slot < i {
			t.Fatalf("timer moved backwards: %s slot=%d", at.Sub(first), slot)
		}
	}
}

func TestDeadlineResultStillBreaksContinuity(t *testing.T) {
	s := samplerFixture(t)
	name := "ch.rss_anon"
	if err := s.m.Gauge(name, s.source(name, ""), 1, 0, 42, OK, 0); err != nil {
		t.Fatal(err)
	}
	s.clock.(*testClock).now = s.start.Add(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.acceptResult(ctx, 0, 2, sampleResult{source: 0, apply: func() { t.Fatal("expired observation applied") }})
	if err := s.m.Gauge(name, s.source(name, ""), 3, int64(2*time.Second), 50, OK, 0); err != nil {
		t.Fatal(err)
	}
	g := s.m.View().Live.Gauges[0]
	if g.CoveredTotal != 0 || g.SpanTotal != uint64(2*time.Second) {
		t.Fatalf("crossed timeout: %+v", g)
	}
}

func TestFinalCHFailuresMarkEveryCounter(t *testing.T) {
	for _, mode := range []string{"busy", "stat", "timeout", "waitid"} {
		t.Run(mode, func(t *testing.T) {
			s := samplerFixture(t)
			s.request.Store(1)
			for _, field := range []string{"rss_anon", "rss_file"} {
				if err := s.m.Gauge("ch."+field, s.source("ch."+field, ""), 1, 0, 42, OK, 0); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			release := make(chan struct{})
			if mode == "busy" {
				s.slots[0].busy.Store(true)
			}
			if mode == "waitid" {
				cancel()
			}
			s.proc.read = func(string, int64) ([]byte, error) {
				if mode == "timeout" {
					<-release
				}
				return nil, errors.New("unavailable")
			}
			s.FinalCH(ctx)
			close(release)
			for _, c := range s.m.View().Live.Counters {
				if c.Name != "sandbox_ctl.cpu" && c.Complete {
					t.Fatalf("unknown tail labeled complete: %+v", c)
				}
			}
			for _, g := range s.m.View().Live.Gauges {
				if g.Continuous || g.Status == OK || g.CoveredTotal != 0 || g.LastValue != 42 {
					t.Fatalf("failed final RSS fabricated an observation: %+v", g)
				}
			}
		})
	}
}

func TestFinalSelfBusyMarksUnknown(t *testing.T) {
	s := samplerFixture(t)
	s.slots[1].busy.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Stop(ctx)
	for _, c := range s.m.View().Live.Counters {
		if c.Name == "sandbox_ctl.cpu" && c.Complete {
			t.Fatalf("unknown self tail: %+v", c)
		}
	}
}

func TestFinalRSSClosesOnlyAcceptedIntervals(t *testing.T) {
	for _, mode := range []string{"idle", "guest-pending", "ch-unapplied", "self-unapplied", "paused"} {
		t.Run(mode, func(t *testing.T) {
			s := samplerFixture(t)
			s.pid = 123
			clock := s.clock.(*testClock)
			setTime := func(at time.Duration) {
				clock.mu.Lock()
				clock.now = s.start.Add(at)
				clock.mu.Unlock()
			}
			final := false
			s.proc.readDir = func(string) ([]os.DirEntry, error) { return nil, nil }
			s.proc.read = func(path string, _ int64) ([]byte, error) {
				if filepath.Base(path) == "stat" {
					pid := filepath.Base(filepath.Dir(path))
					return []byte(strings.Replace(string(procFixture("process")), "123 (", pid+" (", 1)), nil
				}
				if final {
					clock.mu.Lock()
					clock.now = clock.now.Add(200 * time.Millisecond)
					clock.mu.Unlock()
					return []byte("RssAnon: 8 kB\nRssFile: 9 kB\n"), nil
				}
				return []byte("RssAnon: 1 kB\nRssFile: 2 kB\n"), nil
			}
			s.request.Store(1)
			s.readProcess(s.pid, "ch", 1, false)()
			s.readProcess(os.Getpid(), "sandbox_ctl", 1, false)()
			for _, name := range []string{"guest.memory", "filesystem.root"} {
				if err := s.m.Gauge(name, name, 1, 0, 42, OK, 0); err != nil {
					t.Fatal(err)
				}
			}
			setTime(500 * time.Millisecond)
			if strings.Contains(mode, "pending") || strings.Contains(mode, "unapplied") {
				s.request.Store(2)
				s.cancelRound = func() {}
				// A completed raw read is not accepted until its callback runs.
				// Both slots are free here, including the unapplied source.
				for _, process := range []struct {
					pid  int
					name string
				}{{s.pid, "ch"}, {os.Getpid(), "sandbox_ctl"}} {
					apply := s.readProcess(process.pid, process.name, 2, false)
					if (mode == "ch-unapplied" && process.name == "ch") || (mode == "self-unapplied" && process.name == "sandbox_ctl") {
						continue
					}
					apply()
				}
			}
			if mode == "paused" {
				s.Pause()
				s.Resume()
			}
			s.m.Save(clock.Now())
			awaitSave(t, s.m)
			before := s.m.View().Saved.Snapshot
			final = true
			setTime(time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			s.FinalCH(ctx)
			s.acceptResult(ctx, 0, 2, sampleResult{source: 0, apply: func() { t.Fatal("fenced callback applied") }})
			setTime(1500 * time.Millisecond)
			s.Stop(ctx)
			v := s.m.View()
			if v.Saved == nil || !v.Saved.Snapshot.Closed {
				t.Fatalf("final record missing: %+v", v)
			}
			for _, g := range v.Saved.Snapshot.Gauges {
				if g.Name == "guest.memory" || g.Name == "filesystem.root" {
					if g.CoveredTotal != 0 || g.IntegralTotal != (Uint128{}) {
						t.Fatalf("unobserved Guest tail extended: %+v", g)
					}
					continue
				}
				at := 1100 * time.Millisecond // Actual read-window midpoint, not receive time.
				if strings.HasPrefix(g.Name, "sandbox_ctl.") {
					at = 1600 * time.Millisecond
				}
				covered := uint64(at)
				if mode == "paused" || (mode == "ch-unapplied" && strings.HasPrefix(g.Name, "ch.")) || (mode == "self-unapplied" && strings.HasPrefix(g.Name, "sandbox_ctl.")) {
					covered = 0
				}
				left, peak := uint64(1024), uint64(8*1024)
				if strings.HasSuffix(g.Name, "rss_file") {
					left, peak = 2*1024, 9*1024
				}
				prior := uint64(0)
				for _, old := range before.Gauges {
					if old.Name == g.Name {
						prior = old.CoveredTotal
					}
				}
				if g.CoveredTotal != covered || g.IntegralTotal != product(left, covered) || g.LastValueAt != int64(at) || g.Window.Covered != covered-prior || g.Window.Area != product(left, covered-prior) || g.Window.Peak != peak {
					t.Fatalf("final RSS boundary/area/window: %+v; want covered=%s", g, time.Duration(covered))
				}
			}
		})
	}
}

func TestRecoveredCounterCompleteness(t *testing.T) {
	for _, closed := range []bool{false, true} {
		for _, endpoint := range []bool{false, true} {
			s := sampleRecord().Snapshot
			c := Counter{Name: "guest.cpu", Status: Missing}
			if endpoint {
				c = Counter{Name: "guest.cpu"}
				_ = c.Observe("source-A", 100, 100, true)
			}
			s.Counters, s.Closed = []Counter{c}, closed
			s.newRun("new", time.Now(), time.Second, 5*time.Minute)
			if err := s.Counters[0].Observe("source-B", 5, 100, true); err != nil {
				t.Fatal(err)
			}
			if s.Counters[0].Complete {
				t.Fatalf("closed=%v endpoint=%v: %+v", closed, endpoint, s.Counters[0])
			}
		}
	}
	var c Counter
	_ = c.Observe("thread-A", 100, 100, true)
	_ = c.Observe("thread-B", 5, 100, true)
	if c.Complete || c.KnownTotal != (Uint128{Lo: 1050000000}) {
		t.Fatalf("replacement: %+v", c)
	}
}

func TestClosedRecordCannotProveAdjacentRun(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "zero-write"
		if partial {
			name = "partial-append"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			open := func(epoch string) *Manager {
				m, err := Open(dir, "test", epoch, time.Now(), time.Second, 5*time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
			a := open("run-A")
			if err := a.Counter("guest.cpu", "process-A", 100, 100, true); err != nil {
				t.Fatal(err)
			}
			if !a.View().Live.Counters[0].Complete {
				t.Fatal("known-created file and process lost completeness")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			a.Close(ctx, time.Now())
			cancel()
			b := open("run-B")
			if err := b.Counter("guest.cpu", "process-B", 20, 100, true); err != nil {
				t.Fatal(err)
			}
			if partial {
				v := b.View()
				frame, err := EncodeRecord(Record{Sequence: v.Saved.Sequence + 1, Snapshot: *v.Live})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := b.writer.WriteAt(frame[:len(frame)/2], v.SavedEnd); err != nil {
					t.Fatal(err)
				}
			}
			// Logical process-disappearance test, not a power-loss experiment.
			b.closeFiles()
			c := open("run-C")
			defer c.closeFiles()
			if err := c.Counter("guest.cpu", "process-C", 5, 100, true); err != nil {
				t.Fatal(err)
			}
			v := c.View()
			counter := v.Live.Counters[0]
			if counter.Complete || counter.KnownTotal != (Uint128{Lo: 1050000000}) || v.UnknownTail != partial {
				t.Fatalf("lost run-B CPU mislabeled or known total changed: %+v, unknown_tail=%v", counter, v.UnknownTail)
			}
		})
	}
}

func TestStableGuestSourcesRejectChangedDomain(t *testing.T) {
	s := samplerFixture(t)
	for _, name := range []string{"guest.memory", "filesystem.root"} {
		first, valid := s.stableSource(name, "domain-A")
		if !valid {
			t.Fatal("first domain rejected")
		}
		if got, valid := s.stableSource(name, "domain-B"); valid || got != first {
			t.Fatal("domain change accepted")
		}
		if got, valid := s.stableSource(name, "domain-A"); !valid || got != first {
			t.Fatal("valid domain lost")
		}
	}
}

func TestExistingEmptyOrPartialFileRetainsUnknownHistory(t *testing.T) {
	for _, partial := range []bool{false, true} {
		dir := t.TempDir()
		var data []byte
		if partial {
			data = []byte("KUU")
		}
		if err := os.WriteFile(filepath.Join(dir, "test.usage"), data, 0600); err != nil {
			t.Fatal(err)
		}
		m, err := Open(dir, "test", "new", time.Now(), time.Second, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Counter("guest.cpu", "new-process", 5, 100, true); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		m.Close(ctx, time.Now())
		cancel()
		if m.View().Saved.Snapshot.Counters[0].Complete {
			t.Fatal("unknown old history became complete")
		}
		m, err = Open(dir, "test", "next", time.Now(), time.Second, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		_ = m.Counter("guest.cpu", "next-process", 5, 100, true)
		if m.View().Live.Counters[0].Complete {
			t.Fatal("unknown prefix lost on second recovery")
		}
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		m.Close(ctx, time.Now())
		cancel()
	}
}

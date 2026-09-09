package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
			if s.Counters[0].Complete != (closed && endpoint) {
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

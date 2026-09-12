package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sampleLoopClock struct {
	testClock
	samples chan time.Time
	flushes chan time.Time
}

func (c *sampleLoopClock) Ticker(interval time.Duration) (<-chan time.Time, func()) {
	if interval == time.Second {
		return c.samples, func() {}
	}
	return c.flushes, func() {}
}

func TestMissingTicksRespectFinalFence(t *testing.T) {
	for _, mode := range []string{"running", "finalizing"} {
		t.Run(mode, func(t *testing.T) {
			s := samplerFixture(t)
			c := &sampleLoopClock{testClock: testClock{now: s.start}, samples: make(chan time.Time), flushes: make(chan time.Time)}
			s.clock, s.flush = c, 5*time.Minute
			s.proc.readDir = func(string) ([]os.DirEntry, error) { return nil, nil }
			s.proc.read = func(path string, _ int64) ([]byte, error) {
				if filepath.Base(path) == "stat" {
					pid := filepath.Base(filepath.Dir(path))
					return []byte(strings.Replace(string(procFixture("process")), "123 (", pid+" (", 1)), nil
				}
				return []byte("RssAnon: 1 kB\nRssFile: 2 kB\n"), nil
			}
			setTime := func(at time.Duration) {
				c.mu.Lock()
				c.now = s.start.Add(at)
				c.mu.Unlock()
			}
			ctx, cancel := context.WithCancel(context.Background())
			s.Start(ctx, 123)
			t.Cleanup(func() {
				cancel()
				select {
				case <-s.done:
				case <-time.After(3 * time.Second):
					t.Error("sampler loop did not finish")
				}
			})
			waitRound := func(id uint64) {
				t.Helper()
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					v := s.m.View()
					settled := len(v.Live.Gauges) == 4
					for _, g := range v.Live.Gauges {
						settled = settled && g.LastRequest == id
					}
					s.mu.Lock()
					settled = settled && s.cancelRound == nil
					s.mu.Unlock()
					if settled {
						return
					}
					time.Sleep(time.Millisecond)
				}
				t.Fatalf("round %d did not settle", id)
			}
			tick := func() {
				t.Helper()
				select {
				case c.samples <- c.Now():
				case <-time.After(3 * time.Second):
					t.Fatal("sampler did not receive the tick")
				}
			}
			waitRound(1)
			setTime(time.Second)
			tick()
			waitRound(2)
			endCtx, endCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer endCancel()
			if mode == "finalizing" {
				setTime(1100 * time.Millisecond)
				s.FinalCH(endCtx)
			}
			setTime(3 * time.Second)
			tick() // The 2s delivery was missed.
			want := uint64(3 * time.Second)
			if mode == "running" {
				waitRound(3)
				setTime(4 * time.Second)
				want = uint64(2 * time.Second) // 0→1 and 3→4, not the known gap.
			} else {
				// The reaper may still be draining os/exec output before Stop.
				// Receiving a second tick proves the previous case ran while fenced.
				tick()
			}
			s.Stop(endCtx)
			v := s.m.View()
			if v.Saved == nil || !v.Saved.Snapshot.Closed {
				t.Fatalf("final record missing: %+v", v)
			}
			for _, g := range v.Saved.Snapshot.Gauges {
				if strings.HasPrefix(g.Name, "sandbox_ctl.") {
					left := uint64(1024)
					if strings.HasSuffix(g.Name, "rss_file") {
						left *= 2
					}
					if g.CoveredTotal != want || g.IntegralTotal != product(left, want) {
						t.Fatalf("tick/final fence changed valid RSS intervals: %+v; want covered=%s", g, time.Duration(want))
					}
				}
			}
		})
	}
}

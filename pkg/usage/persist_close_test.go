package usage

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Fail the initial append's Sync and rollback Sync, then allow recovery of
// exactly that uncertain F. The ordinary blocked WriteAt remains injectable.
type closeRetryWriter struct {
	faultWriter
	syncFailures atomic.Int32
}

func (w *closeRetryWriter) Sync() error {
	for remaining := w.syncFailures.Load(); remaining > 0; remaining = w.syncFailures.Load() {
		if w.syncFailures.CompareAndSwap(remaining, remaining-1) {
			return syscall.EIO
		}
	}
	return w.faultWriter.Sync()
}

func TestCloseFencesProducersBeforeSealing(t *testing.T) {
	for _, mode := range []string{"idle", "pending", "recovered-rollback", "recovered-uncertain", "recovered-same-epoch"} {
		t.Run(mode, func(t *testing.T) {
			w := &closeRetryWriter{faultWriter: faultWriter{entered: make(chan struct{}, 2), release: make(chan struct{})}}
			release := sync.OnceFunc(func() { close(w.release) })
			t.Cleanup(release)
			m := testManager(w)
			if mode == "recovered-rollback" || mode == "recovered-uncertain" || mode == "recovered-same-epoch" {
				r := Record{Sequence: 7, Snapshot: m.live.clone()}
				r.Snapshot.Closed = true
				var err error
				w.data, err = EncodeRecord(r)
				if err != nil {
					t.Fatal(err)
				}
				s := r.Snapshot.clone()
				epoch := "new-epoch"
				if mode == "recovered-same-epoch" {
					epoch = r.Snapshot.RunEpoch
				}
				s.newRun(epoch, time.Now(), time.Second, time.Minute)
				m = newManager(s, Recovery{Record: &r, End: int64(len(w.data))}, w, nil)
			}
			var closed atomic.Int32
			m.closeFn = func() { closed.Add(1) }
			if err := m.Counter("cpu", "process", 1, 100, true); err != nil {
				t.Fatal(err)
			}
			if err := m.Gauge("ram", "memory", 1, 0, 100, OK, 0); err != nil {
				t.Fatal(err)
			}
			if mode != "idle" {
				m.Save(time.Now())
				<-w.entered
			}
			if err := m.Counter("cpu", "process", 2, 100, true); err != nil {
				t.Fatal(err)
			}
			if err := m.Gauge("ram", "memory", 2, 1e9, 200, OK, 0); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() { m.Close(ctx, time.Now()); close(done) }()
			defer func() { release(); <-done }()
			for !m.View().Live.Closed {
				select {
				case <-ctx.Done():
					t.Fatal("Close did not establish its admission fence")
				case <-time.After(time.Millisecond):
				}
			}
			if mode == "idle" {
				<-w.entered
			}
			before, frozen := m.View(), cloneRecord(m.pending)
			for i := 0; i < 100; i++ {
				if err := m.Counter("cpu", "process", 3, 100, true); !errors.Is(err, context.Canceled) {
					t.Fatalf("closing Counter accepted: %v", err)
				}
				if err := m.Counter("new-cpu", "process", 1, 100, true); !errors.Is(err, context.Canceled) {
					t.Fatalf("closing Counter creation accepted: %v", err)
				}
				for _, status := range []string{OK, Missing, Paused, Unsupported} {
					if err := m.Gauge("ram", "memory", 3, 2e9, 300, status, 0); !errors.Is(err, context.Canceled) {
						t.Fatalf("closing Gauge accepted: %v", err)
					}
				}
				m.CounterMissing("cpu", true)
				m.CounterMissing("new-cpu", true)
				m.BreakGauges(Missing)
				m.discontinue()
			}
			if !reflect.DeepEqual(before, m.View()) || !reflect.DeepEqual(frozen, m.pending) {
				t.Fatal("producer mutated live or F while Close was saving")
			}
			if mode != "idle" {
				w.mu.Lock()
				if mode == "recovered-rollback" || mode == "recovered-same-epoch" {
					w.writeErr = syscall.ENOSPC
				} else if mode == "recovered-uncertain" {
					w.syncFailures.Store(2)
				}
				w.mu.Unlock()
				w.release <- struct{}{}
				select {
				case <-w.entered:
				case <-done:
					t.Fatal("previous epoch's closed S was mistaken for current completion")
				case <-ctx.Done():
					t.Fatal("Close did not attempt its second save")
				}
				w.mu.Lock()
				w.writeErr, w.syncErr = nil, nil
				w.mu.Unlock()
			}
			release()
			<-done
			v := m.View()
			if mode == "recovered-uncertain" {
				// Two attempts were spent on the same uncertain F. Retrying F
				// must not turn it into a new closed record that falsely includes A.
				if v.Saved == nil || v.Saved.Snapshot.Closed || v.Saved.Snapshot.RunEpoch != v.Live.RunEpoch || v.Saved.Snapshot.Counters[0].LastRaw != 1 {
					t.Fatalf("uncertain F identity changed: %+v", v)
				}
			} else {
				if v.Saved == nil || !v.Saved.Snapshot.Closed || !reflect.DeepEqual(v.Saved.Snapshot.Counters, before.Live.Counters) {
					t.Fatalf("Close lost accepted pre-fence counters: %+v", v)
				}
				g := v.Saved.Snapshot.Gauges[0]
				if g.IntegralTotal != (Uint128{Lo: 100e9}) || g.CoveredTotal != 1e9 || g.SpanTotal != 1e9 || g.Window.Peak != 200 {
					t.Fatalf("Close lost accepted pre-fence gauge input: %+v", g)
				}
			}
			if closed.Load() != 1 || v.Saving || v.SaveError != "" {
				t.Fatalf("Close lifecycle: descriptors=%d view=%+v", closed.Load(), v)
			}
			w.mu.Lock()
			data := append([]byte(nil), w.data...)
			w.mu.Unlock()
			if _, _, err := ReadHistory(bytes.NewReader(data), int64(len(data)), 0, 10, "test"); err != nil {
				t.Fatal(err)
			}
			m.Close(ctx, time.Now())
			if closed.Load() != 1 || !reflect.DeepEqual(v, m.View()) {
				t.Fatal("repeated Close changed completed state")
			}
		})
	}
}

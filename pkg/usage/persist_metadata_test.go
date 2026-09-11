package usage

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenRejectsUnencodableIdentityBeforeFileAccess(t *testing.T) {
	for _, bad := range []string{"", strings.Repeat("x", maxString+1), strings.Repeat("😀", 65), "bad\xff"} {
		for _, field := range []string{"epoch", "sandbox ID"} {
			for _, mode := range []string{"absent-directory", "new-file", "saved-file", "locked-file"} {
				t.Run(fmt.Sprintf("%s/%s/%x", field, mode, bad), func(t *testing.T) {
					base := t.TempDir()
					if mode == "absent-directory" {
						base = filepath.Join(base, "absent")
					}
					path := filepath.Join(base, "test.usage")
					var before []byte
					if mode == "saved-file" || mode == "locked-file" {
						var err error
						before, err = EncodeRecord(sampleRecord())
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, before, 0o600); err != nil {
							t.Fatal(err)
						}
						f, err := os.Open(path)
						if err != nil {
							t.Fatal(err)
						}
						defer f.Close()
						if mode == "locked-file" {
							if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
								t.Fatal(err)
							}
						}
					}
					id, epoch := "test", "new"
					if field == "epoch" {
						epoch = bad
					} else {
						id = bad
					}
					m, err := Open(base, id, epoch, time.Now(), time.Second, time.Minute)
					if m != nil {
						m.closeFiles()
					}
					if m != nil || err == nil || !strings.Contains(err.Error(), field) {
						t.Fatalf("Open = %p, %v; want identity rejection before I/O", m, err)
					}
					if mode == "absent-directory" {
						if _, err := os.Stat(base); !os.IsNotExist(err) {
							t.Fatalf("created absent directory: %v", err)
						}
						return
					}
					entries, err := os.ReadDir(base)
					want := 0
					if before != nil {
						want = 1
					}
					if err != nil || len(entries) != want {
						t.Fatalf("unexpected directory contents: %v, %v", entries, err)
					}
					if before != nil {
						got, err := os.ReadFile(path)
						if err != nil || !bytes.Equal(got, before) {
							t.Fatalf("history changed: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestManagerRejectsUnencodableMetadata(t *testing.T) {
	for _, saving := range []bool{false, true} {
		t.Run(fmt.Sprint(saving), func(t *testing.T) {
			w := &faultWriter{}
			m := testManager(w)
			if err := m.Counter("cpu", "process", 1, 100, true); err != nil {
				t.Fatal(err)
			}
			if err := m.Gauge("ram", "memory", 10, 0, 100, OK, 0); err != nil {
				t.Fatal(err)
			}
			releaseSave := func() {}
			if saving {
				w.entered, w.release = make(chan struct{}, 1), make(chan struct{})
				m.Save(time.Now())
				<-w.entered
				releaseSave = sync.OnceFunc(func() { close(w.release); awaitSave(t, m) })
				t.Cleanup(releaseSave)
			}
			before, frozen := m.View(), cloneRecord(m.pending)
			check := func(err error, wantError bool) {
				t.Helper()
				if (err != nil) != wantError {
					t.Fatalf("error = %v, wantError = %v", err, wantError)
				}
				if !reflect.DeepEqual(before, m.View()) || !reflect.DeepEqual(frozen, m.pending) {
					t.Fatal("rejected metadata changed live, saved or frozen state")
				}
			}
			for _, bad := range []string{"", strings.Repeat("x", maxString+1), strings.Repeat("😀", 65), "bad\xff"} {
				check(m.Counter(bad, "process", 2, 100, true), true)
				check(m.Gauge(bad, "memory", 11, 1e9, 200, OK, 0), true)
				m.CounterMissing(bad, true)
				check(nil, false)
				if bad == "" { // Empty Gauge source and status are valid codec values.
					continue
				}
				for _, name := range []string{"cpu", "new-cpu"} {
					check(m.Counter(name, bad, 2, 100, true), true)
				}
				check(m.Gauge("new-ram", bad, 11, 1e9, 200, OK, 0), true)
				check(m.Gauge("new-ram", "memory", 11, 1e9, 200, bad, 0), true)
				for _, request := range []uint64{0, 9, 10} {
					check(m.Gauge("ram", bad, request, 1e9, 200, bad, 0), false)
				}
				check(m.Gauge(bad, bad, 0, 1e9, 200, bad, 0), false)
			}
			check(m.Counter("ram", "process", 2, 100, true), true)
			check(m.Gauge("cpu", "memory", 11, 1e9, 200, OK, 0), true)
			m.CounterMissing("ram", true)
			check(nil, false)
			if saving {
				releaseSave()
			}
			m.Save(time.Now())
			awaitSave(t, m)
			v := m.View()
			if v.SaveError != "" || v.Saved == nil {
				t.Fatalf("rejected input poisoned subsequent save: %+v", v)
			}
		})
	}
}

func TestRejectedGaugeMetadataBreaksContinuity(t *testing.T) {
	for _, bad := range []string{strings.Repeat("x", maxString+1), strings.Repeat("😀", 65), "bad\xff"} {
		for _, field := range []string{"source", "status", "break"} {
			for _, saving := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%t/%x", field, saving, bad), func(t *testing.T) {
					w := &faultWriter{}
					m := testManager(w)
					if err := m.Gauge("ram", "memory", 10, 0, 100, OK, 0); err != nil {
						t.Fatal(err)
					}
					if saving {
						w.entered, w.release = make(chan struct{}, 1), make(chan struct{})
						m.Save(time.Now())
						<-w.entered
						defer func() { close(w.release); awaitSave(t, m) }()
					}
					frozen := cloneRecord(m.pending)
					if field == "break" {
						m.BreakGauges(bad)
					} else {
						source, status := "memory", OK
						if field == "source" {
							source = bad
						} else {
							status = bad
						}
						if err := m.Gauge("ram", source, 11, 1e9, 200, status, 0); err == nil {
							t.Fatal("unencodable input accepted")
						}
					}
					g := m.View().Live.Gauges[0]
					if g.Continuous || g.Status != Invalid || g.Source != "memory" || !reflect.DeepEqual(frozen, m.pending) {
						t.Fatalf("rejection corrupted metadata/F or retained continuity: %+v", g)
					}
					if field != "break" {
						if g.LastRequest != 11 {
							t.Fatalf("rejected request did not fence older data: %+v", g)
						}
						if err := m.Gauge("ram", "memory", 11, 1e9, 200, OK, 0); err != nil || !reflect.DeepEqual(g, m.View().Live.Gauges[0]) {
							t.Fatal("late replacement of rejected request was accepted")
						}
					}
					if err := m.Gauge("ram", "memory", 12, 2e9, 300, OK, 0); err != nil {
						t.Fatal(err)
					}
					g = m.View().Live.Gauges[0]
					if g.IntegralTotal != (Uint128{}) || g.CoveredTotal != 0 {
						t.Fatalf("integrated across known rejected observation: %+v", g)
					}
					if _, err := EncodeRecord(Record{Sequence: 1, Snapshot: *m.View().Live}); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestManagerAcceptsCodecStringBoundaries(t *testing.T) {
	for _, text := range []string{strings.Repeat("x", maxString), strings.Repeat("😀", 64)} {
		m, err := Open(t.TempDir(), "test", text, time.Now(), time.Second, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		defer m.closeFiles()
		if err := m.Counter(text, text, 100, 100, true); err != nil {
			t.Fatal(err)
		}
		for i, status := range []string{OK, Missing, Invalid, Paused, Unsupported, "", text} {
			if err := m.Gauge("ram", "", uint64(i+1), int64(i)*1e9, 100, status, 0); err != nil {
				t.Fatal(err)
			}
		}
		m.CounterMissing("unknown", true)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		m.Close(ctx, time.Now())
		cancel()
		v := m.View()
		if v.Saved == nil || v.SaveError != "" {
			t.Fatalf("valid boundaries failed to save: %+v", v)
		}
		b, err := EncodeRecord(*v.Saved)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeRecord(b, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManagerReservesRecordCapacity(t *testing.T) {
	w := &faultWriter{}
	m := testManager(w)
	// Keep one short source to exercise expansion, not just new-name rejection.
	if err := m.Counter("short", "s", 0, 100, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxCounters; i++ {
		name := fmt.Sprintf("%0256d", i)
		before := m.View()
		if err := m.Counter(name, strings.Repeat("s", maxString), 0, 100, true); err != nil {
			if i == 0 || !reflect.DeepEqual(before, m.View()) {
				t.Fatalf("capacity rejection changed state at %d: %v", i, err)
			}
			break
		}
	}
	// Consume the remaining budget with shorter metrics until source growth
	// would exceed the limit. Every accepted value must remain encodable.
	for i := 0; i < MaxCounters; i++ {
		if err := m.Counter(fmt.Sprintf("filler%d", i), "s", 0, 100, true); err != nil {
			break
		}
	}
	before := m.View()
	if err := m.Counter("short", strings.Repeat("s", maxString), 0, 100, true); err == nil {
		t.Fatal("source expansion beyond reserved capacity accepted")
	}
	if !reflect.DeepEqual(before, m.View()) {
		t.Fatal("source expansion changed state")
	}
	m.CounterMissing(strings.Repeat("z", maxString), true)
	if !reflect.DeepEqual(before, m.View()) {
		t.Fatal("CounterMissing exceeded record budget")
	}
	if err := m.Gauge("ram", "memory", 1, 0, 1, OK, 0); err == nil {
		t.Fatal("Gauge exceeded record budget")
	}
	// Maximal endpoint growth must still fit without accepting any new strings.
	for _, c := range m.View().Live.Counters {
		if err := m.Counter(c.Name, c.Source, ^uint64(0), 100, true); err != nil {
			t.Fatal(err)
		}
	}
	m.Save(time.Now())
	awaitSave(t, m)
	if v := m.View(); v.SaveError != "" || v.Saved == nil {
		t.Fatalf("reserved numeric growth failed to save: %+v", v)
	}
}

func TestRecordSizeBoundCoversNumericGrowth(t *testing.T) {
	r := sampleRecord()
	u := ^uint64(0)
	r.Sequence, r.SavedUTC = u, -1<<63
	s := &r.Snapshot
	s.StartedUTC, s.SampleInterval, s.FlushInterval = -1<<63, 1<<62-1, 1<<63-1
	s.Counters = []Counter{{Name: "cpu", Source: "process", KnownTotal: Uint128{u, u}, LastRaw: u, Hertz: u,
		Remainder: u - 1, SourceKnown: true, Complete: true, Status: Missing}}
	s.Gauges = []Gauge{{Name: "ram", Source: "memory", IntegralTotal: Uint128{u, u}, SpanTotal: u, CoveredTotal: u,
		LastValue: u, LastValueAt: -1 << 63, LastAt: -1 << 63, LastRequest: u,
		PositionKnown: true, ValueKnown: true, Continuous: true, Status: strings.Repeat("s", maxString),
		Window: Window{Area: Uint128{u, u}, Span: u, Covered: u, Peak: u, PeakAt: -1 << 63,
			PeakKnown: true, Samples: u, MaxReadWindow: u}}}
	b, err := EncodeRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	if bound := recordSizeBound(*s); len(b) > bound || bound-len(b) > 32 {
		t.Fatalf("bound %d, full-width encoding %d", bound, len(b))
	}
}

func TestOpenPreservesReadableRecordWithoutGrowthCapacity(t *testing.T) {
	r := sampleRecord()
	r.Snapshot.Counters, r.Snapshot.Gauges = nil, nil
	for i := 0; i < 996; i++ {
		r.Snapshot.Counters = append(r.Snapshot.Counters, Counter{Name: fmt.Sprintf("%0256d", i),
			Source: strings.Repeat("s", maxString), Hertz: 100, SourceKnown: true, Status: OK})
	}
	b, err := EncodeRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	if recordSizeBound(r.Snapshot) <= MaxRecordBytes {
		t.Fatal("fixture has enough growth space")
	}
	base := t.TempDir()
	path := filepath.Join(base, "test.usage")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		m, err := Open(base, "test", "new", time.Now(), time.Second, time.Minute)
		if m != nil {
			m.closeFiles()
		}
		if m != nil || err == nil || !strings.Contains(err.Error(), "continued accumulation") {
			t.Fatalf("Open accepted exhausted growth capacity: %p, %v", m, err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
			f.Close()
			t.Fatal(err)
		}
		recovery, err := Recover(f, int64(len(b)), "test")
		f.Close()
		if err != nil || recovery.Record == nil || recovery.End != int64(len(b)) {
			t.Fatalf("offline recovery lost valid record: %+v, %v", recovery, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, b) {
		t.Fatalf("rejected live owner changed saved bytes: %v", err)
	}
}

func TestManagerCapacityPreservesLifecycleBreaks(t *testing.T) {
	for _, status := range []string{Paused, Unsupported} {
		m := testManager(nil)
		if err := m.Gauge("ram", "s", 1, 0, 10, OK, 0); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < MaxCounters; i++ {
			if err := m.Counter(fmt.Sprintf("%0256d", i), strings.Repeat("s", maxString), 0, 100, true); err != nil {
				break
			}
		}
		for i := 0; i < MaxCounters; i++ {
			if err := m.Counter(fmt.Sprintf("filler%d", i), "s", 0, 100, true); err != nil {
				break
			}
		}
		long := strings.Repeat("s", maxString)
		if err := m.Gauge("ram", long, 2, 1e9, 20, OK, 0); err == nil {
			t.Fatal("gauge source exceeded reserved capacity")
		}
		g := m.View().Live.Gauges[0]
		if g.Status != Invalid || g.Continuous || g.LastRequest != 2 || g.Source != "s" {
			t.Fatalf("capacity rejection failed to fence observation: %+v", g)
		}
		if err := m.Gauge("ram", long, 3, 2e9, 20, status, 0); err != nil {
			t.Fatalf("unused lifecycle source charged against capacity: %v", err)
		}
		g = m.View().Live.Gauges[0]
		if g.Status != status || g.PositionKnown || g.LastRequest != 3 || g.Source != "s" {
			t.Fatalf("lifecycle break failed: %+v", g)
		}
		if err := m.Gauge("ram", "s", 4, 3e9, 20, OK, 0); err != nil {
			t.Fatal(err)
		}
		if g := m.View().Live.Gauges[0]; g.IntegralTotal != (Uint128{}) || g.CoveredTotal != 0 {
			t.Fatalf("known rejection was integrated: %+v", g)
		}
	}
}

func TestManagerCapacityAllowsSupportedTopology(t *testing.T) {
	m := testManager(nil)
	for i := 0; i < MaxCounters; i++ {
		if err := m.Counter(fmt.Sprintf("guest.vcpu.%d.cpu", i), strings.Repeat("s", 128), 100, 100, true); err != nil {
			t.Fatalf("supported counter %d: %v", i, err)
		}
	}
	for i := 0; i < MaxGauges; i++ {
		if err := m.Gauge(fmt.Sprintf("filesystem.%d", i), strings.Repeat("s", maxString), 1, 0, 100, OK, 0); err != nil {
			t.Fatalf("supported gauge %d: %v", i, err)
		}
	}
	if _, err := EncodeRecord(Record{Sequence: 1, Snapshot: *m.View().Live}); err != nil {
		t.Fatal(err)
	}
}

func TestManagerGaugeArithmeticRejectionFencesRequest(t *testing.T) {
	m := testManager(nil)
	if err := m.Gauge("ram", "memory", 10, 0, 100, OK, 0); err != nil {
		t.Fatal(err)
	}
	m.live.Gauges[0].IntegralTotal = Uint128{^uint64(0), ^uint64(0)}
	if err := m.Gauge("ram", "memory", 11, 1e9, 200, OK, 0); err != ErrOverflow {
		t.Fatalf("expected rejected arithmetic: %v", err)
	}
	before := m.View()
	if g := before.Live.Gauges[0]; g.Status != Invalid || g.Continuous || g.LastRequest != 11 {
		t.Fatalf("arithmetic error did not fence request: %+v", g)
	}
	if err := m.Gauge("ram", "replacement", 11, 1e9, 0, OK, 0); err != nil || !reflect.DeepEqual(before, m.View()) {
		t.Fatal("same request was replaced after arithmetic rejection")
	}
	if err := m.Gauge("ram", "memory", 12, 2e9, 300, OK, 0); err != nil {
		t.Fatal(err)
	}
	if g := m.View().Live.Gauges[0]; g.CoveredTotal != 0 || g.SpanTotal != 2e9 || g.IntegralTotal != before.Live.Gauges[0].IntegralTotal {
		t.Fatalf("integrated across arithmetic failure: %+v", g)
	}
}

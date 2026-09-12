package usage

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenRejectsIntervalsBeforeFileAccess(t *testing.T) {
	for _, tc := range []struct {
		name          string
		sample, flush time.Duration
	}{
		{"zero-sample", 0, time.Minute},
		{"negative-sample", -1, time.Minute},
		{"zero-flush", time.Second, 0},
		{"negative-flush", time.Second, -1},
		{"flush-before-sample", 2 * time.Second, time.Second},
		{"sample-overflow", 1 << 62, 1<<63 - 1},
		{"maximum-duration", 1<<63 - 1, 1<<63 - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"absent-directory", "new-file", "saved-file", "locked-file"} {
				t.Run(mode, func(t *testing.T) {
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
						if mode == "locked-file" {
							f, err := os.OpenFile(path, os.O_RDWR, 0)
							if err != nil {
								t.Fatal(err)
							}
							t.Cleanup(func() { _ = f.Close() })
							if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
								t.Fatal(err)
							}
						}
					}
					m, err := Open(base, "test", "new", time.Now(), tc.sample, tc.flush)
					if m != nil {
						m.closeFiles()
					}
					if m != nil || err == nil || !strings.Contains(err.Error(), "interval") {
						t.Fatalf("Open = (%p, %v), want interval rejection before I/O", m, err)
					}
					got, readErr := os.ReadFile(path)
					if before == nil {
						if !os.IsNotExist(readErr) {
							t.Fatalf("invalid Open created a file: %v", readErr)
						}
					} else if readErr != nil || !bytes.Equal(got, before) {
						t.Fatalf("invalid Open changed existing history: %v", readErr)
					}
				})
			}
		})
	}
}

func TestOpenAcceptsIntervalBoundaries(t *testing.T) {
	for _, pair := range [][2]time.Duration{{1, 1}, {time.Second, 5 * time.Minute}, {1<<62 - 1, 1<<63 - 1}} {
		m, err := Open(t.TempDir(), "test", "new", time.Now(), pair[0], pair[1])
		if err != nil {
			t.Fatalf("intervals %v: %v", pair, err)
		}
		view := m.View()
		m.closeFiles()
		if view.Live.SampleInterval != int64(pair[0]) || view.Live.FlushInterval != int64(pair[1]) {
			t.Fatalf("intervals changed: %+v", view.Live)
		}
	}
}

func TestManagerGaugeRejectsUnorderedStatuses(t *testing.T) {
	for _, status := range []string{OK, Missing, Invalid, Paused, Unsupported} {
		for _, saving := range []bool{false, true} {
			name := status
			if saving {
				name += "/saving"
			}
			t.Run(name, func(t *testing.T) {
				w := &faultWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
				m := testManager(w)
				if err := m.Gauge("ram", "original", 10, 0, 10, OK, 0); err != nil {
					t.Fatal(err)
				}
				if err := m.Gauge("ram", "original", 12, 1e9, 20, OK, 0); err != nil {
					t.Fatal(err)
				}
				if saving {
					m.Save(time.Now())
					<-w.entered
					t.Cleanup(func() { close(w.release); awaitSave(t, m) })
				}
				before := m.View()
				for _, request := range []uint64{0, 11, 12} {
					if err := m.Gauge("ram", "different", request, 100e9, 1000, status, time.Second); err != nil {
						t.Fatal(err)
					}
					if after := m.View(); !reflect.DeepEqual(after, before) {
						t.Fatalf("request %d / %s changed newer state:\nbefore=%+v\nafter=%+v", request, status, before.Live.Gauges[0], after.Live.Gauges[0])
					}
				}
				if err := m.Gauge("ram", "original", 13, 2e9, 30, OK, 0); err != nil {
					t.Fatal(err)
				}
				g := m.View().Live.Gauges[0]
				if g.IntegralTotal != (Uint128{Lo: 30e9}) || g.CoveredTotal != 2e9 || g.SpanTotal != 2e9 || g.Window.Samples != 3 {
					t.Fatalf("rejected status interrupted continuity: %+v", g)
				}
			})
		}
	}
}

func TestManagerGaugeZeroDoesNotCreateResource(t *testing.T) {
	for _, status := range []string{OK, Missing, Invalid, Paused, Unsupported} {
		m := testManager(nil)
		if err := m.Gauge("ram", "source", 0, 0, 100, status, 0); err != nil {
			t.Fatal(err)
		}
		if got := m.View().Live.Gauges; len(got) != 0 {
			t.Fatalf("zero request / %s created resource: %+v", status, got)
		}
	}
}

func TestManagerGaugeOrderedBreakReopensTimeDomain(t *testing.T) {
	for _, status := range []string{Paused, Unsupported} {
		m := testManager(nil)
		for _, v := range []struct {
			id, value uint64
			at        int64
			status    string
		}{{1, 10, 0, OK}, {2, 20, 1e9, OK}, {3, 0, 2e9, status}, {4, 30, 20e9, OK}, {5, 40, 21e9, OK}} {
			if err := m.Gauge("ram", "source", v.id, v.at, v.value, v.status, 0); err != nil {
				t.Fatal(err)
			}
			if v.id == 3 {
				g := m.View().Live.Gauges[0]
				if g.Status != status || g.PositionKnown || g.ValueKnown || g.Continuous || g.LastRequest != 3 {
					t.Fatalf("ordered break did not close domain: %+v", g)
				}
			}
		}
		g := m.View().Live.Gauges[0]
		if g.IntegralTotal != (Uint128{Lo: 40e9}) || g.CoveredTotal != 2e9 || g.SpanTotal != 2e9 || g.Window.Samples != 4 {
			t.Fatalf("%s domain incorrectly integrated: %+v", status, g)
		}
	}
}

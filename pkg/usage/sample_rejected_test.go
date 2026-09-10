package usage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A proc reader can invalidate its cached identity before the deadline/epoch
// gate rejects the observation. No ticks from that observation may be added,
// but a later accepted read must not resurrect disproved source completeness.
func TestRejectedVCPUSourceLoss(t *testing.T) {
	for _, reject := range []string{"deadline", "generation"} {
		for _, mode := range []string{"exit", "ambiguous", "renamed", "discovery-replaced"} {
			t.Run(reject+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Join(root, "123", "task", "17"), 0700); err != nil {
					t.Fatal(err)
				}
				clock := &testClock{now: time.Now()}
				m := testManager(&faultWriter{})
				phase := 0
				stat := func(pid int, comm string, born, raw uint64) []byte {
					fields := make([]string, 41)
					for i := range fields {
						fields[i] = "0"
					}
					fields[0], fields[14-3], fields[15-3], fields[20-3] = "S", "1000", "10", "2"
					if phase > 0 && (mode == "ambiguous" || mode == "discovery-replaced") {
						fields[20-3] = "3"
					}
					fields[22-3], fields[43-3] = strconv.FormatUint(born, 10), strconv.FormatUint(raw, 10)
					return []byte(fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(fields, " ")))
				}
				p := &procReader{root: root, boot: "boot", hertz: 100, vcpuCount: 1,
					threads: make(map[int]vcpuThread), readDir: os.ReadDir}
				p.read = func(path string, _ int64) ([]byte, error) {
					switch {
					case strings.HasSuffix(path, "/task/17/stat"):
						if phase > 0 && mode == "exit" {
							return nil, os.ErrNotExist
						}
						comm, born := "vcpu0", uint64(42)
						if phase == 1 && mode == "renamed" {
							comm = "other"
						}
						if phase > 0 && mode == "discovery-replaced" {
							born++
						}
						return stat(17, comm, born, uint64(10+phase*5)), nil
					case strings.HasSuffix(path, "/task/18/stat"):
						return stat(18, "vcpu0", 45, 1), nil
					case strings.HasSuffix(path, "/stat"):
						return stat(123, "cloud-hypervisor", 20, 100), nil
					case strings.HasSuffix(path, "/status"):
						return []byte("RssAnon: 1 kB\nRssFile: 2 kB\n"), nil
					}
					return nil, os.ErrNotExist
				}
				s := &Sampler{m: m, proc: p, clock: clock, start: clock.now, epoch: "run", sources: make(map[string]string)}
				s.readProcess(123, "ch", 1, false)()
				phase = 1
				if mode == "ambiguous" {
					if err := os.Mkdir(filepath.Join(root, "123", "task", "18"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				apply := s.readProcess(123, "ch", 2, false)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if reject == "deadline" {
					cancel()
				} else {
					s.generation++
				}
				s.acceptResult(ctx, 0, 2, sampleResult{source: 0, apply: apply})
				for _, c := range m.View().Live.Counters {
					if c.Name == "guest.vcpu.0" && c.KnownTotal != (Uint128{Lo: 100000000}) {
						t.Fatalf("rejected observation added ticks: %+v", c)
					}
				}
				phase = 2
				if mode == "ambiguous" {
					if err := os.Remove(filepath.Join(root, "123", "task", "18")); err != nil {
						t.Fatal(err)
					}
				}
				s.readProcess(123, "ch", 3, false)()
				wantTicks := uint64(20)
				if mode == "exit" {
					wantTicks = 10
				} else if mode == "discovery-replaced" {
					wantTicks = 30
				}
				for _, c := range m.View().Live.Counters {
					if c.Name == "guest.vcpu.0" && (c.Complete || c.KnownTotal != (Uint128{Lo: wantTicks * 10000000})) {
						t.Fatalf("rejected %s source loss was forgotten: %+v", mode, c)
					}
				}
				if len(p.incomplete) != 1 {
					t.Fatalf("source validity state is not bounded by vCPU count: %d", len(p.incomplete))
				}
			})
		}
	}
}

func TestRejectedInitialVCPUIdentity(t *testing.T) {
	for _, mode := range []string{"process-eio", "task-eio", "replaced", "same", "known-process-eio"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "123", "task", "17"), 0700); err != nil {
				t.Fatal(err)
			}
			clock := &testClock{now: time.Now()}
			m := testManager(&faultWriter{})
			phase := 0
			stat := func(pid int, comm string, born, raw uint64) []byte {
				fields := make([]string, 41)
				for i := range fields {
					fields[i] = "0"
				}
				fields[0], fields[14-3], fields[15-3], fields[20-3] = "S", "1000", "10", "2"
				if phase == 2 && mode == "replaced" {
					fields[20-3] = "3"
				}
				fields[22-3], fields[43-3] = strconv.FormatUint(born, 10), strconv.FormatUint(raw, 10)
				return []byte(fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(fields, " ")))
			}
			p := &procReader{root: root, boot: "boot", hertz: 100, vcpuCount: 1,
				threads: make(map[int]vcpuThread), readDir: os.ReadDir}
			p.read = func(path string, _ int64) ([]byte, error) {
				switch {
				case strings.HasSuffix(path, "/task/17/stat"):
					if phase == 1 && mode == "task-eio" {
						return nil, syscall.EIO
					}
					born, raw := uint64(42), uint64(10)
					if phase == 2 {
						raw = 25
						if mode == "replaced" {
							born++
						}
					}
					return stat(17, "vcpu0", born, raw), nil
				case strings.HasSuffix(path, "/123/stat"):
					if phase == 1 && (mode == "process-eio" || mode == "known-process-eio") {
						return nil, syscall.EIO
					}
					return stat(123, "cloud-hypervisor", 20, 100), nil
				case strings.HasSuffix(path, "/status"):
					return []byte("RssAnon: 1 kB\nRssFile: 2 kB\n"), nil
				}
				return nil, os.ErrNotExist
			}
			s := &Sampler{m: m, proc: p, clock: clock, start: clock.now, epoch: "run", sources: make(map[string]string)}
			if mode == "known-process-eio" {
				s.readProcess(123, "ch", 1, false)()
			}
			phase = 1
			apply := s.readProcess(123, "ch", 2, false)
			s.generation++
			s.acceptResult(context.Background(), 0, 2, sampleResult{source: 0, apply: apply})
			if mode != "known-process-eio" && len(m.View().Live.Counters) != 0 {
				t.Fatal("rejected first observation changed the counters")
			}
			phase = 2
			s.readProcess(123, "ch", 3, false)()
			found := false
			for _, c := range m.View().Live.Counters {
				if c.Name != "guest.vcpu.0" {
					continue
				}
				found = true
				wantComplete := mode == "same" || mode == "known-process-eio"
				if c.Complete != wantComplete || c.KnownTotal != (Uint128{Lo: 250000000}) || c.LastRaw != 25 || !c.SourceKnown {
					t.Fatalf("first accepted counter: %+v; want complete=%v", c, wantComplete)
				}
			}
			if !found || len(p.incomplete) != 1 {
				t.Fatalf("counter absent or source validity unbounded: found=%v size=%d", found, len(p.incomplete))
			}
		})
	}
}

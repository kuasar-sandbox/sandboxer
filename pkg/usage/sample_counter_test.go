package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestVCPUReadCompleteness(t *testing.T) {
	for _, mode := range []string{"transient", "rediscovery", "initial", "initial-process", "changed", "exited", "late-exit", "disappeared", "ambiguous", "final", "history"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "123", "task", "17"), 0700); err != nil {
				t.Fatal(err)
			}
			clock := &testClock{now: time.Now()}
			m := testManager(&faultWriter{})
			m.historyUnknown = mode == "history"
			var threadErr, processErr error
			start, raw, threadCount := uint64(42), uint64(10), 2
			stat := func(pid int, comm string, born, guest uint64) []byte {
				fields := make([]string, 41)
				for i := range fields {
					fields[i] = "0"
				}
				fields[0], fields[14-3], fields[15-3] = "S", "1000", "10"
				fields[20-3] = strconv.Itoa(threadCount)
				fields[22-3], fields[43-3] = strconv.FormatUint(born, 10), strconv.FormatUint(guest, 10)
				return []byte(fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(fields, " ")))
			}
			p := &procReader{root: root, boot: "boot", hertz: 100, vcpuCount: 1,
				threads: make(map[int]vcpuThread), readDir: os.ReadDir}
			p.read = func(path string, _ int64) ([]byte, error) {
				switch {
				case strings.HasSuffix(path, "/task/18/stat"):
					return stat(18, "vcpu0", 45, 1), nil
				case strings.HasSuffix(path, "/task/17/stat"):
					return stat(17, "vcpu0", start, raw), threadErr
				case strings.HasSuffix(path, "/123/stat"):
					return stat(123, "cloud-hypervisor", 20, 100), processErr
				case strings.HasSuffix(path, "/status"):
					return []byte("RssAnon: 1 kB\nRssFile: 2 kB\n"), nil
				default:
					return nil, os.ErrNotExist
				}
			}
			s := &Sampler{m: m, proc: p, clock: clock, start: clock.now, epoch: "run", sources: make(map[string]string)}
			counter := func(name string) Counter {
				for _, c := range m.View().Live.Counters {
					if c.Name == name {
						return c
					}
				}
				t.Fatalf("missing counter %s", name)
				return Counter{}
			}
			if mode != "initial" && mode != "initial-process" {
				s.readProcess(123, "ch", 1, false)()
			}
			threadErr = syscall.EIO
			if mode == "rediscovery" {
				threadCount++ // An unrelated thread topology change forces discovery.
			}
			if mode == "initial-process" {
				processErr = syscall.EIO
			}
			if mode == "changed" {
				threadErr, start = nil, 43
			}
			if mode == "exited" || mode == "late-exit" {
				threadErr = os.ErrNotExist
			}
			if mode == "late-exit" {
				// A deadline/epoch rejection drops apply, but the proc worker has
				// already retired its cache entry. The next round must still
				// account for that source's unknown terminal segment.
				_ = s.readProcess(123, "ch", 2, false)
			}
			if mode == "disappeared" {
				if err := os.Remove(filepath.Join(root, "123", "task", "17")); err != nil {
					t.Fatal(err)
				}
				threadCount--
			}
			if mode == "ambiguous" {
				if err := os.Mkdir(filepath.Join(root, "123", "task", "18"), 0700); err != nil {
					t.Fatal(err)
				}
				threadCount++
			}
			s.readProcess(123, "ch", 2, mode == "final")()
			wantComplete := mode == "transient" || mode == "rediscovery"
			if c := counter("guest.vcpu.0"); c.Complete != wantComplete || c.Status != Missing {
				t.Errorf("failed read: %+v; want complete=%v", c, wantComplete)
			}
			if wantComplete {
				// Repeated failure must not lose the validated cached source.
				s.readProcess(123, "ch", 3, false)()
				if !counter("guest.vcpu.0").Complete {
					t.Error("repeated transient read lost completeness")
				}
			}
			threadErr, processErr, raw = nil, nil, 25
			if mode == "disappeared" {
				if err := os.Mkdir(filepath.Join(root, "123", "task", "17"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "ambiguous" {
				if err := os.Remove(filepath.Join(root, "123", "task", "18")); err != nil {
					t.Fatal(err)
				}
			}
			s.readProcess(123, "ch", 4, false)()
			wantTicks := uint64(25)
			if mode == "changed" {
				wantTicks += 10 // New thread's creation baseline, old known total retained.
			}
			if c := counter("guest.vcpu.0"); c.Complete != wantComplete || c.KnownTotal != (Uint128{Lo: wantTicks * 10000000}) || c.Status != OK {
				t.Errorf("recovered counter: %+v; want ticks=%d complete=%v", c, wantTicks, wantComplete)
			}
			if c := counter("guest.cpu"); c.Complete != (mode != "history") || c.KnownTotal != (Uint128{Lo: 1000000000}) {
				t.Errorf("vCPU failure contaminated available total: %+v", c)
			}
		})
	}
}

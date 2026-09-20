package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNewSamplerFailureReleasesFiles(t *testing.T) {
	for _, mode := range []string{"count", "proc"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			procErr := errors.New("injected proc bootstrap failure")
			// Every iteration must release both descriptors and its flock; no
			// background writer is allowed to change even an empty usage file.
			for i := 0; i < 64; i++ {
				m, err := Open(dir, "test", "new", time.Now(), time.Second, 5*time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(m.closeFiles)
				file := m.reader.(*os.File)
				closed, closeFiles := 0, m.closeFn
				m.closeFn = func() { closed++; closeFiles() }
				cpus := 1
				if mode == "count" {
					cpus = 0
				}
				s, err := newSampler(m, cpus, 1, nil, nil, nil, func(int) (*procReader, error) {
					if mode == "count" {
						t.Fatal("proc bootstrap ran for an invalid resource count")
					}
					return nil, procErr
				})
				if err == nil || s != nil || (mode == "proc" && !errors.Is(err, procErr)) {
					t.Fatalf("failed construction: %v %v", s, err)
				}
				if closed != 1 {
					t.Fatalf("closed descriptors %d times", closed)
				}
				if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("file still open: %v", err)
				}
				if v := m.View(); v.Saving || v.Saved != nil || v.Live.Closed {
					t.Fatalf("constructor sealed an uninitialized run: %+v", v)
				}
			}
			if data, err := os.ReadFile(filepath.Join(dir, "test.usage")); err != nil || len(data) != 0 {
				t.Fatalf("constructor wrote file: %d bytes, %v", len(data), err)
			}
		})
	}
	if _, err := NewSampler(nil, 1, 1, nil, nil, nil); err == nil {
		t.Fatal("nil manager accepted")
	}
}

func TestNewSamplerSuccessRetainsOwnershipUntilStop(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir, "test", "new", time.Now(), time.Second, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSampler(m, 1, 1, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.interval != time.Second || s.flush != 5*time.Minute {
		t.Fatalf("sampler intervals = %s/%s, want 1s/5m", s.interval, s.flush)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer s.Stop(ctx)
	file := m.reader.(*os.File)
	if _, err := file.Stat(); err != nil {
		t.Fatalf("successful construction closed the file: %v", err)
	}
	probe, err := os.Open(filepath.Join(dir, "test.usage"))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("successful sampler did not retain writer ownership: %v", err)
	}
	// Also covers successful construction followed by a business setup error
	// before Start. That path still uses the ordinary bounded Stop, not abort.
	s.Stop(ctx)
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Stop did not close the file: %v", err)
	}
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatalf("Stop retained writer ownership: %v", err)
	}
	v := m.View()
	if v.Saved == nil || !v.Saved.Snapshot.Closed || len(v.Saved.Snapshot.Counters) != 1 || v.Saved.Snapshot.Counters[0].Name != "sandbox_ctl.cpu" {
		t.Fatalf("successful sampler did not save its final self observation: %+v", v)
	}
}

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUsageWaitChild(t *testing.T) {
	if os.Getenv("KUASAR_USAGE_WAIT_CHILD") != "1" {
		return
	}
	_, _ = os.Stdout.WriteString(strings.Repeat("output", 32768))
	os.Exit(0)
}

func TestObserveCHExitRetainsZombieAndDrainsOutput(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsageWaitChild$")
	cmd.Env = append(os.Environ(), "KUASAR_USAGE_WAIT_CHILD=1")
	var output bytes.Buffer
	cmd.Stdout = &output // os/exec's copy goroutine must be joined by cmd.Wait.
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	calls, finalCalls := 0, 0
	err := observeCHExit(func() error {
		calls++
		if calls <= 2 {
			return fmt.Errorf("interrupted: %w", unix.EINTR)
		}
		var info unix.Siginfo
		return unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
	}, func() {}, func(ctx context.Context) {
		finalCalls++
		if ctx.Err() != nil {
			t.Errorf("successful wait canceled final read: %v", ctx.Err())
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
		if err != nil || !strings.HasPrefix(string(b[strings.LastIndexByte(string(b), ')')+1:]), " Z ") {
			t.Errorf("WNOWAIT did not retain zombie: %q %v", b, err)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 250*time.Millisecond {
			t.Error("unbounded final read")
		}
	})
	if waitErr := cmd.Wait(); waitErr != nil {
		t.Fatal(waitErr)
	}
	if err != nil || calls != 3 || finalCalls != 1 || output.Len() != 6*32768 {
		t.Fatalf("err=%v waitID=%d final=%d output=%d", err, calls, finalCalls, output.Len())
	}
}

func TestObserveCHExitErrorStillAllowsSoleWait(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsageWaitChild$")
	cmd.Env = append(os.Environ(), "KUASAR_USAGE_WAIT_CHILD=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	called := 0
	err := observeCHExit(func() error { return unix.EINVAL }, func() { t.Error("failed waitid marked exit observed") }, func(ctx context.Context) {
		called++
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Error("failed retention must mark final source unknown")
		}
	})
	if waitErr := cmd.Wait(); waitErr != nil {
		t.Fatal(waitErr)
	}
	if !errors.Is(err, unix.EINVAL) || called != 1 || output.Len() != 6*32768 {
		t.Fatalf("err=%v final=%d output=%d", err, called, output.Len())
	}
}

func TestResourceStatsEndsAtObservedExitBeforeFinalUsage(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsageWaitChild$")
	cmd.Env = append(os.Environ(), "KUASAR_USAGE_WAIT_CHILD=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	dir := t.TempDir()
	statsFile(t, dir, "memory.current", "123")
	statsFile(t, dir, "cpu.stat", "usage_usec 456\n")
	var exited atomic.Bool
	server := &ctl.Server{Path: filepath.Join(t.TempDir(), "ctl.sock"), SnapshotHandler: func(ctl.Request) (ctl.Response, error) {
		t.Error("resource read called snapshot")
		return ctl.Response{}, errors.New("unexpected snapshot")
	}, ResourceStatsHandler: func(ctl.Request) (ctl.Response, error) {
		stats, err := readCurrentResourceStats("sid", statsConfig(), dir, func() bool { return !exited.Load() })
		return ctl.Response{ResourceStats: &stats}, err
	}}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() { _ = server.Serve(ctx); close(served) }()
	defer func() { cancel(); server.Stop(); <-served }()
	err := observeCHExit(func() error {
		var info unix.Siginfo
		return unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
	}, func() { exited.Store(true) }, func(finalCtx context.Context) {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
		if err != nil || !strings.HasPrefix(string(b[strings.LastIndexByte(string(b), ')')+1:]), " Z ") {
			t.Fatal("final native endpoint was not retained", err)
		}
		// Query the real ctl transport while the final callback is still active,
		// before cmd.Wait and chExited. Only effective specification is live.
		stats, err := ctl.ReadResourceStats(finalCtx, server.Path, "sid")
		if err != nil || stats.MemoryUsed != nil || stats.CPUUsageUsec != nil || stats.TimestampUnix != nil || stats.MemoryCapacity != 1<<20 {
			t.Fatal("exited process was published as live", stats, err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 6*32768 {
		t.Fatal("final endpoint or output drain changed", output.Len())
	}
}

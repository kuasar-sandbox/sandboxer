package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	}, func(ctx context.Context) {
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
	err := observeCHExit(func() error { return unix.EINVAL }, func(ctx context.Context) {
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

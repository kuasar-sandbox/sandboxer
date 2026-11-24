package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
)

// A normal shutdown has no memory holder. Its executable must still join the
// detached final reply after runtime cleanup, including errors/disconnection.
func TestWaitRunJoinsShutdownResponseWithoutCapture(t *testing.T) {
	for _, mode := range []string{"success", "error", "disconnected"} {
		t.Run(mode, func(t *testing.T) {
			directory, err := os.MkdirTemp("", "ks-stop-reply-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(directory)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShutdownResponseProcess$")
			cmd.Env = append(os.Environ(), "KUASAR_SHUTDOWN_RESPONSE_DIR="+directory, "KUASAR_SHUTDOWN_RESPONSE_MODE="+mode)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill() }()
			output := bufio.NewReader(stdout)
			line, err := output.ReadString('\n')
			if err != nil || line != "control-ready\n" {
				_ = cmd.Wait()
				t.Fatalf("child control: line=%q err=%v stderr=%s", line, err, stderr.String())
			}
			requestCtx, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			replied := make(chan error, 1)
			go func() {
				replied <- ctl.Shutdown(requestCtx, filepath.Join(directory, "ctl.sock"), ctl.ShutdownTarget{SandboxID: "sandbox", RunID: "run"})
			}()
			line, err = output.ReadString('\n')
			if err != nil || line != "response-pending\n" {
				_ = cmd.Wait()
				t.Fatalf("child lost shutdown reply: line=%q err=%v stderr=%s", line, err, stderr.String())
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			select {
			case err := <-waited:
				t.Fatalf("owner exited before final shutdown reply: %v; %s", err, stderr.String())
			case <-time.After(75 * time.Millisecond):
			}
			if mode == "disconnected" {
				cancelRequest()
				// Cancellation schedules socket closure asynchronously. Observe the
				// disconnect before allowing a complete reply to win that race.
				select {
				case err = <-replied:
				case <-ctx.Done():
					t.Fatal("shutdown client did not observe cancellation")
				}
			}
			if _, err := stdin.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			if mode != "disconnected" {
				err = <-replied
			}
			switch mode {
			case "success":
				if err != nil {
					t.Fatalf("shutdown reply: %v", err)
				}
			case "error":
				if err == nil || !strings.Contains(err.Error(), "shutdown fixture failure") {
					t.Fatalf("lost shutdown error: %v", err)
				}
			case "disconnected":
				if err == nil {
					t.Fatal("disconnected shutdown client reported success")
				}
			}
			select {
			case err := <-waited:
				if err != nil {
					t.Fatalf("owner exit: %v; %s", err, stderr.String())
				}
			case <-ctx.Done():
				t.Fatal("completed shutdown response retained executable")
			}
		})
	}
}

func TestShutdownResponseProcess(t *testing.T) {
	directory := os.Getenv("KUASAR_SHUTDOWN_RESPONSE_DIR")
	if directory == "" {
		return
	}
	fail := func(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(2) }
	r := &Runtime{state: runtimeRunning, exitDone: make(chan struct{})}
	life, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	server := &ctl.Server{Path: filepath.Join(directory, "ctl.sock")}
	server.ShutdownHandler = func(ctl.ShutdownRequest) (ctl.ShutdownOperation, error) {
		return ctl.ShutdownOperation{
			Begin: func() (int64, error) { return ctl.MonotonicDeadline(time.Now().Add(5 * time.Second)) },
			Wait: func() error {
				cancel()
				if err := <-served; err != nil {
					fail(err)
				}
				r.mu.Lock()
				r.state = runtimeClosed
				r.mu.Unlock()
				fmt.Fprintln(os.Stdout, "response-pending")
				close(r.exitDone)
				var release [1]byte
				if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
					fail(err)
				}
				if os.Getenv("KUASAR_SHUTDOWN_RESPONSE_MODE") == "error" {
					return errors.New("shutdown fixture failure")
				}
				return nil
			},
		}, nil
	}
	if err := server.Listen(); err != nil {
		fail(err)
	}
	r.captureResponsesDone = server.ResponsesDone()
	go func() { served <- server.Serve(life) }()
	fmt.Fprintln(os.Stdout, "control-ready")
	code, err := WaitRun(context.Background(), r)
	if err != nil {
		fail(err)
	}
	os.Exit(code)
}

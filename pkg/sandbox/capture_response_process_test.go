package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
)

// The child uses the executable's actual WaitRun/WaitMemorySave boundary and
// exits immediately when it returns. Its real holder finishes before a final
// ctl response is allowed to write, making the former exit race deterministic.
func TestWaitRunJoinsFrozenCaptureResponse(t *testing.T) {
	for _, mode := range []string{"success", "committed-error", "disconnected"} {
		t.Run(mode, func(t *testing.T) {
			directory, err := os.MkdirTemp("", "ks-capture-reply-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(directory)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFrozenCaptureResponseProcess$")
			cmd.Env = append(os.Environ(), "KUASAR_CAPTURE_RESPONSE_DIR="+directory, "KUASAR_CAPTURE_RESPONSE_MODE="+mode)
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
			conn, err := net.Dial("unix", filepath.Join(directory, "ctl.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			if err := ctl.WriteMessage(conn, ctl.Request{Type: ctl.TypeCheckpointSnapshotRequest, CheckpointID: "fast"}); err != nil {
				t.Fatal(err)
			}
			line, err = output.ReadString('\n')
			if err != nil || line != "response-pending\n" {
				_ = cmd.Wait()
				t.Fatalf("child exited before final response: line=%q err=%v stderr=%s", line, err, stderr.String())
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			select {
			case err := <-waited:
				t.Fatalf("executable exited with a detached response pending: %v; %s", err, stderr.String())
			case <-time.After(75 * time.Millisecond):
			}
			if mode == "disconnected" {
				_ = conn.Close()
			}
			if _, err := stdin.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			if mode != "disconnected" {
				var response ctl.Response
				if err := ctl.ReadMessage(conn, &response); err != nil {
					t.Fatalf("final capture frame: %v", err)
				}
				if mode == "success" && (response.Type != ctl.TypeSnapshotDone || response.SnapshotRef != "fixed") {
					t.Fatalf("success response=%+v", response)
				}
				if mode == "committed-error" && (response.Type != ctl.TypeError || response.Msg != "committed capture error") {
					t.Fatalf("error response=%+v", response)
				}
			}
			select {
			case err := <-waited:
				if err != nil {
					t.Fatalf("child shutdown: %v; %s", err, stderr.String())
				}
			case <-ctx.Done():
				t.Fatalf("response completion retained executable: %v", ctx.Err())
			}
		})
	}
}

func TestFrozenCaptureResponseProcess(t *testing.T) {
	directory := os.Getenv("KUASAR_CAPTURE_RESPONSE_DIR")
	if directory == "" {
		return
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	record := memorySaveRecord(t, directory)
	m, err := memory.Create("capture-response", 4096)
	if err != nil {
		fail(err)
	}
	fd, err := memorycheckpoint.ReadOnlyMemfd(m.FD())
	if err != nil {
		fail(err)
	}
	holder, err := memorycheckpoint.NewHolder(record, fd, 4096, nil)
	if err != nil {
		fail(err)
	}
	if err := m.Close(); err != nil {
		fail(err)
	}
	r := &Runtime{state: runtimeRunning, exitDone: make(chan struct{}), memorySave: &checkpointMemorySave{holder: holder}}
	life, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	server := &ctl.Server{Path: filepath.Join(directory, "ctl.sock")}
	server.SnapshotHandler = func(ctl.Request) (ctl.Response, error) {
		response := ctl.Response{SnapshotRef: "fixed", BeforeWrite: func() error {
			cancel()
			if err := <-served; err != nil {
				fail(err)
			}
			if err := record.Update(func(v *memorycheckpoint.CaptureRecord) { v.RuntimeRetired = true }); err != nil {
				fail(err)
			}
			if err := holder.Start(func(context.Context, io.ReaderAt) (string, error) { return "payload", nil }, nil); err != nil {
				fail(err)
			}
			r.mu.Lock()
			r.state = runtimeClosed
			r.exit = ExitResult{Code: 0}
			r.mu.Unlock()
			close(r.exitDone)
			if err := holder.Wait(context.Background()); err != nil {
				fail(err)
			}
			// A cancelled SDK join must return without discarding the process's
			// still-pending response obligation. WaitRun uses its own live join.
			cancelled, stop := context.WithCancel(context.Background())
			stop()
			if err := r.WaitMemorySave(cancelled); !errors.Is(err, context.Canceled) {
				fail(fmt.Errorf("cancelled response join: %v", err))
			}
			fmt.Fprintln(os.Stdout, "response-pending")
			var release [1]byte
			if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
				fail(err)
			}
			return nil
		}}
		if os.Getenv("KUASAR_CAPTURE_RESPONSE_MODE") == "committed-error" {
			return response, errors.New("committed capture error")
		}
		return response, nil
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

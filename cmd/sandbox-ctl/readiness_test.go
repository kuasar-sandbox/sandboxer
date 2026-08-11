package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"golang.org/x/sys/unix"
)

// transferReadinessFD models the inherited --ready-fd ownership contract. The
// production writer adopts and closes the numeric descriptor, so the original
// *os.File must be closed (and its finalizer disarmed) before that descriptor can
// be reused by another test.
func transferReadinessFD(t *testing.T, f *os.File) int {
	t.Helper()
	fd, err := unix.Dup(int(f.Fd()))
	if err != nil {
		_ = f.Close()
		t.Fatalf("duplicate readiness fd: %v", err)
	}
	if err := f.Close(); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("close readiness fd source: %v", err)
	}
	return fd
}

func TestRunCmdRejectsReadyFDBelowThree(t *testing.T) {
	rc, stderr := captureStderr(t, func() int {
		return runCmd([]string{"--ready-fd=2"})
	})
	if rc != 2 {
		t.Fatalf("runCmd return code = %d, want 2", rc)
	}
	if !strings.Contains(stderr, "must be at least 3") {
		t.Fatalf("stderr = %q, want ready-fd lower-bound error", stderr)
	}
}

func TestRunCmdRejectsInvalidReadyFD(t *testing.T) {
	rc, stderr := captureStderr(t, func() int {
		return runCmd([]string{"--ready-fd=1048575"})
	})
	if rc != 2 {
		t.Fatalf("runCmd return code = %d, want 2", rc)
	}
	if !strings.Contains(stderr, "is invalid") {
		t.Fatalf("stderr = %q, want invalid ready-fd error", stderr)
	}
}

func TestRunCmdEarlyFailureClosesReadyFD(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	readyFD := transferReadinessFD(t, w)

	rc, _ := captureStderr(t, func() int {
		return runCmd([]string{
			"--ready-fd=" + strconv.Itoa(readyFD),
			"--ch-binary=/nonexistent/cloud-hypervisor",
			"--config=/nonexistent/sandbox.yaml",
		})
	})
	if rc != 1 {
		t.Fatalf("runCmd return code = %d, want 1", rc)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Fatalf("readiness wire = %q, want EOF without events", body)
	}
}

func TestReadinessFDWriterSetsCLOEXEC(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	readyFD := transferReadinessFD(t, w)
	n, err := newReadinessFDWriter(readyFD, nil)
	if err != nil {
		_ = unix.Close(readyFD)
		t.Fatal(err)
	}
	defer n.Close()
	flags, err := unix.FcntlInt(uintptr(readyFD), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("ready fd is missing FD_CLOEXEC")
	}
}

func TestReadinessFDWriterExactSequenceAndDuplicates(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	readyFD := transferReadinessFD(t, w)
	n, err := newReadinessFDWriter(readyFD, nil)
	if err != nil {
		_ = unix.Close(readyFD)
		t.Fatal(err)
	}
	n.Notify(sandbox.ReadinessControlReady)
	n.Notify(sandbox.ReadinessControlReady)
	n.Notify(sandbox.ReadinessReady)
	n.Notify(sandbox.ReadinessReady)
	if err := n.Close(); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "control_ready\nready\n"; got != want {
		t.Fatalf("readiness wire = %q, want %q", got, want)
	}
}

func TestReadinessFDWriterReadyBeforeControlFailsClosed(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var logs bytes.Buffer
	readyFD := transferReadinessFD(t, w)
	n, err := newReadinessFDWriter(readyFD, func(format string, args ...any) {
		fmt.Fprintf(&logs, format+"\n", args...)
	})
	if err != nil {
		_ = unix.Close(readyFD)
		t.Fatal(err)
	}
	n.Notify(sandbox.ReadinessReady)
	n.Notify(sandbox.ReadinessControlReady)
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Fatalf("readiness wire = %q, want empty EOF", body)
	}
	if got := strings.Count(logs.String(), "invariant violation"); got != 1 {
		t.Fatalf("invariant log count = %d, logs=%q", got, logs.String())
	}
}

func TestReadinessFDWriterEPIPEDisablesWithoutFailure(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	readyFD := transferReadinessFD(t, w)
	n, err := newReadinessFDWriter(readyFD, func(format string, args ...any) {
		fmt.Fprintf(&logs, format+"\n", args...)
	})
	if err != nil {
		_ = unix.Close(readyFD)
		t.Fatal(err)
	}
	n.Notify(sandbox.ReadinessControlReady)
	n.Notify(sandbox.ReadinessReady)
	if got := strings.Count(logs.String(), "reader closed early"); got != 1 {
		t.Fatalf("EPIPE log count = %d, logs=%q", got, logs.String())
	}
	if err := n.Close(); err != nil {
		t.Fatalf("Close after EPIPE: %v", err)
	}
}

func TestReadinessFDWriterConcurrentNotifyAndClose(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readyFD := transferReadinessFD(t, w)
	n, err := newReadinessFDWriter(readyFD, nil)
	if err != nil {
		_ = unix.Close(readyFD)
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); n.Notify(sandbox.ReadinessControlReady) }()
		go func() { defer wg.Done(); n.Notify(sandbox.ReadinessReady) }()
		go func() { defer wg.Done(); _ = n.Close() }()
	}
	wg.Wait()
	_ = n.Close()
	body, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "" && got != "control_ready\n" && got != "control_ready\nready\n" {
		t.Fatalf("invalid concurrent readiness wire %q", got)
	}
}

type shortWriteCloser struct {
	bytes.Buffer
	max int
}

func (w *shortWriteCloser) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.Buffer.Write(p)
}

func (*shortWriteCloser) Close() error { return nil }

func TestReadinessFDWriterUsesFullWriteLoop(t *testing.T) {
	w := &shortWriteCloser{max: 2}
	n := &readinessFDWriter{w: w, logf: func(string, ...any) {}}
	n.Notify(sandbox.ReadinessControlReady)
	n.Notify(sandbox.ReadinessReady)
	if got, want := w.String(), "control_ready\nready\n"; got != want {
		t.Fatalf("readiness wire = %q, want %q", got, want)
	}
}

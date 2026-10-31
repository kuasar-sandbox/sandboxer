package main

import (
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

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

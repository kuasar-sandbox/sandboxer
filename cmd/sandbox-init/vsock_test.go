package main

import (
	"errors"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRetryInterruptedIO(t *testing.T) {
	calls := 0
	op := func(_ int, b []byte) (int, error) {
		calls++
		if calls == 1 {
			return -1, syscall.EINTR
		}
		if calls == 2 {
			return 0, syscall.EINTR
		}
		return len(b), nil
	}

	n, err := retryInterruptedIO(op, 42, make([]byte, 7))
	if err != nil {
		t.Fatalf("retryInterruptedIO: %v", err)
	}
	if n != 7 {
		t.Fatalf("bytes = %d, want 7", n)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRetryInterruptedIOStopsOnOtherError(t *testing.T) {
	wantErr := syscall.EIO
	calls := 0
	op := func(_ int, _ []byte) (int, error) {
		calls++
		return 0, wantErr
	}

	n, err := retryInterruptedIO(op, 42, nil)
	if n != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("got (%d, %v), want (0, %v)", n, err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestRetryInterruptedIOPreservesPartialResult(t *testing.T) {
	calls := 0
	op := func(_ int, _ []byte) (int, error) {
		calls++
		return 3, syscall.EINTR
	}

	n, err := retryInterruptedIO(op, 42, make([]byte, 7))
	if n != 3 || !errors.Is(err, syscall.EINTR) {
		t.Fatalf("got (%d, %v), want (3, EINTR)", n, err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestVsockConnCloseDoesNotCloseReusedFD(t *testing.T) {
	source, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer unix.Close(source)

	owned, err := unix.FcntlInt(uintptr(source), unix.F_DUPFD_CLOEXEC, 10_000)
	if err != nil {
		t.Fatalf("duplicate fd: %v", err)
	}
	c := &vsockConn{fd: owned}
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	if err := unix.Dup3(source, owned, unix.O_CLOEXEC); err != nil {
		t.Fatalf("reuse fd %d: %v", owned, err)
	}
	defer unix.Close(owned)

	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := unix.FcntlInt(uintptr(owned), unix.F_GETFD, 0); err != nil {
		t.Fatalf("reused fd was closed: %v", err)
	}
}

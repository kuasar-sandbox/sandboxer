package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

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

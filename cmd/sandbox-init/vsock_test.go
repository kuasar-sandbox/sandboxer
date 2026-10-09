package main

import (
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

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
	for name, op := range map[string]func() error{
		"read":        func() error { _, err := c.Read(make([]byte, 1)); return err },
		"write":       func() error { _, err := c.Write([]byte{1}); return err },
		"usage read":  func() error { _, err := (usageIO{conn: c}).Read(make([]byte, 1)); return err },
		"usage write": func() error { _, err := (usageIO{conn: c}).Write([]byte{1}); return err },
		"deadline":    func() error { return c.SetDeadline(time.Time{}) },
		"linger":      func() error { return c.SetLinger(0) },
	} {
		if err := op(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("%s touched reused fd: %v", name, err)
		}
	}
	if _, err := unix.FcntlInt(uintptr(owned), unix.F_GETFD, 0); err != nil {
		t.Fatalf("reused fd was closed: %v", err)
	}
}

// The gate is inside an admitted raw operation. Close must shutdown the
// socket while that lease is held, but cannot release its fd until it exits.
// EINTR after shutdown must not retry through the retired descriptor.
func TestVsockCloseJoinsRawLeaseBeforeReleasingFD(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := &vsockConn{fd: fds[0]}
	defer c.Close()
	defer unix.Close(fds[1])
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	result := make(chan error, 1)
	calls := 0
	go func() {
		_, err := c.io(func(fd int, b []byte) (int, error) {
			calls++
			close(entered)
			<-release
			return -1, syscall.EINTR
		}, nil)
		result <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	// EOF at the peer proves Close has performed SHUT_RDWR, without sleeps.
	if err := unix.SetsockoptTimeval(fds[1], unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 2}); err != nil {
		t.Fatal(err)
	}
	var buf [1]byte
	if n, err := unix.Read(fds[1], buf[:]); n != 0 || err != nil {
		t.Fatalf("close did not shutdown before joining: %d, %v", n, err)
	}
	if _, err := unix.FcntlInt(uintptr(c.fd), unix.F_GETFD, 0); err != nil {
		t.Fatalf("fd released with live lease: %v", err)
	}
	select {
	case <-closed:
		t.Fatal("Close returned before raw operation exited")
	default:
	}
	once.Do(func() { close(release) })
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) || calls != 1 {
			t.Fatalf("retry after close: calls=%d err=%v", calls, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("raw operation did not finish")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not join")
	}
}

func TestVsockCloseInterruptsRawIO(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "write"}[write], func(t *testing.T) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			c := &vsockConn{fd: fds[0]}
			defer c.Close()
			defer unix.Close(fds[1])
			if write {
				// Fill the socket without changing its blocking mode.
				for {
					_, err := unix.SendmsgN(c.fd, make([]byte, 4096), nil, nil, unix.MSG_DONTWAIT)
					if errors.Is(err, unix.EAGAIN) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			entered := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = c.rawIO(func(fd int, b []byte) (int, error) {
					close(entered)
					if write {
						return unix.Write(fd, b)
					}
					return unix.Read(fd, b)
				}, []byte{1})
			}()
			<-entered
			closed := make(chan struct{})
			go func() { _ = c.Close(); close(closed) }()
			for _, ch := range []<-chan struct{}{done, closed} {
				select {
				case <-ch:
				case <-time.After(2 * time.Second):
					t.Fatal("Close failed to interrupt raw I/O")
				}
			}
		})
	}
}

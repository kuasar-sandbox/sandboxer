package tapfd

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Payload-parse unit tests live with the canonical parser in
// connector/pkg/tapfd (payload_test.go); this package delegates to it
// and is covered by the RecvFd / Acquire tests below.

// socketPair returns two connected *net.UnixConn (a sender, b receiver).
func socketPair(t *testing.T) (a, b *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	mk := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "sp")
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("fileconn: %v", err)
		}
		return c.(*net.UnixConn)
	}
	return mk(fds[0]), mk(fds[1])
}

func sendMsg(t *testing.T, c *net.UnixConn, payload string, fds ...int) {
	t.Helper()
	var oob []byte
	if len(fds) > 0 {
		oob = unix.UnixRights(fds...)
	}
	if _, _, err := c.WriteMsgUnix([]byte(payload), oob, nil); err != nil {
		t.Fatalf("writemsg: %v", err)
	}
}

func TestRecvFd_OK(t *testing.T) {
	a, b := socketPair(t)
	defer a.Close()
	defer b.Close()
	dn, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer dn.Close()

	sendMsg(t, a, "mac=02:00:00:00:80:01 mtu=1450 ip=169.254.4.1 fd=1\x00", int(dn.Fd()))
	files, netnsFile, meta, err := RecvFd(b)
	if err != nil {
		t.Fatalf("RecvFd: %v", err)
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if netnsFile != nil {
		netnsFile.Close()
		t.Fatalf("got netns fd without netns_fd= in payload")
	}
	if meta.MAC != "02:00:00:00:80:01" || meta.IP != "169.254.4.1" || meta.MTU != 1450 {
		t.Fatalf("meta = %+v", meta)
	}
}

// TestRecvFd_Netns: payload declares netns_fd=1, so the trailing fd is split
// off as the netns reference (docs/tapfd.md §2.5 / §2.4 step 5). Any two fds
// stand in for [tap, netns]; the split is positional, not content-based.
func TestRecvFd_Netns(t *testing.T) {
	a, b := socketPair(t)
	defer a.Close()
	defer b.Close()
	tapFD, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer tapFD.Close()
	netnsFD, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer netnsFD.Close()

	sendMsg(t, a, "mac=02:00:00:00:80:01 fd=1 netns_fd=1\x00", int(tapFD.Fd()), int(netnsFD.Fd()))
	files, netnsFile, _, err := RecvFd(b)
	if err != nil {
		t.Fatalf("RecvFd: %v", err)
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
		if netnsFile != nil {
			netnsFile.Close()
		}
	}()
	if len(files) != 1 {
		t.Fatalf("got %d tap files, want 1", len(files))
	}
	if netnsFile == nil {
		t.Fatal("netns_fd=1 declared but no netns file returned")
	}
}

func TestRecvFd_CountMismatch(t *testing.T) {
	a, b := socketPair(t)
	defer a.Close()
	defer b.Close()
	dn, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer dn.Close()

	sendMsg(t, a, "fd=2\x00", int(dn.Fd())) // declares 2, sends 1
	files, _, _, err := RecvFd(b)
	if err == nil || !strings.Contains(err.Error(), "fd=2") {
		for _, f := range files {
			f.Close()
		}
		t.Fatalf("want fd-count mismatch error, got %v", err)
	}
}

func TestAcquire_Errors(t *testing.T) {
	if _, _, _, err := Acquire(context.Background(), nil, time.Second); err == nil {
		t.Fatal("empty argv: want error")
	}
	// Helper exits non-zero without handing over a fd (§3.1: must fail).
	if _, _, _, err := Acquire(context.Background(), []string{"sh", "-c", "exit 3"}, 2*time.Second); err == nil {
		t.Fatal("non-zero helper: want error")
	}
}

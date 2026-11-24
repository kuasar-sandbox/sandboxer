package tapfd

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	ctapfd "github.com/kuasar-sandbox/connector/pkg/tapfd"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
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
	if meta.MAC != "02:00:00:00:80:01" || meta.IP != "169.254.4.1" {
		t.Fatalf("meta = %+v", meta)
	}
}

// TestRecvFd_Netns: payload declares netns_fd=1, so the trailing fd is split
// off as the netns reference (https://github.com/kuasar-sandbox/connector/blob/main/docs/tapfd.md#optional-netns-descriptor / https://github.com/kuasar-sandbox/connector/blob/main/docs/tapfd.md#receiver-algorithm-reference step 5). Any two fds
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
	// Helper exits non-zero without handing over a fd (https://github.com/kuasar-sandbox/connector/blob/main/docs/tapfd.md#runtime-responsibilities-consumer-side: must fail).
	if _, _, _, err := Acquire(context.Background(), []string{"sh", "-c", "exit 3"}, 2*time.Second); err == nil {
		t.Fatal("non-zero helper: want error")
	}
}

func TestAcquireSocketOK(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "tapfd.sock")
	reqCh, errCh := serveOneTapFDSocket(t, sock, func(conn *net.UnixConn, line string) error {
		payload, err := (&ctapfd.PortMetadata{
			Port:    1,
			MAC:     "02:00:00:00:80:01",
			InnerIP: "169.254.4.1",
			FDCount: 1,
		}).Marshal()
		if err != nil {
			return err
		}
		payload, err = ctapfd.BuildOKResponse(payload)
		if err != nil {
			return err
		}
		dn, err := os.Open(os.DevNull)
		if err != nil {
			return err
		}
		defer dn.Close()
		return ctapfd.SendFd(conn, payload, dn.Fd())
	})

	f, netnsFile, meta, err := AcquireSocket(context.Background(), sock, "VSWITCH=sw0 PORT=1", time.Second)
	if err != nil {
		t.Fatalf("AcquireSocket: %v", err)
	}
	defer f.Close()
	if netnsFile != nil {
		netnsFile.Close()
		t.Fatalf("got netns fd without netns_fd= in payload")
	}
	if got, want := <-reqCh, "TAPFD/1 OPEN want_netns=1 VSWITCH=sw0 PORT=1\n"; got != want {
		t.Fatalf("request = %q, want %q", got, want)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("provider: %v", err)
	}
	if meta.MAC != "02:00:00:00:80:01" || meta.IP != "169.254.4.1" {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestAcquireSocketProviderError(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "tapfd.sock")
	_, errCh := serveOneTapFDSocket(t, sock, func(conn *net.UnixConn, line string) error {
		_, err := conn.Write(ctapfd.BuildErrorResponse(ctapfd.ErrorCodePortUnavailable, "port_not_attached"))
		return err
	})

	_, _, _, err := AcquireSocket(context.Background(), sock, "VSWITCH=sw0 PORT=1", time.Second)
	if err == nil || !strings.Contains(err.Error(), ctapfd.ErrorCodePortUnavailable) {
		t.Fatalf("AcquireSocket error = %v, want provider error", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("provider: %v", err)
	}
}

func TestAcquireConfigSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "tapfd.sock")
	_, errCh := serveOneTapFDSocket(t, sock, func(conn *net.UnixConn, line string) error {
		_, err := conn.Write(ctapfd.BuildErrorResponse(ctapfd.ErrorCodePortUnavailable, "port_not_attached"))
		return err
	})

	_, _, _, err := AcquireConfig(context.Background(), &config.TapFDConfig{
		Socket:  sock,
		Request: "VSWITCH=sw0 PORT=1",
		Timeout: "1s",
	})
	if err == nil || !strings.Contains(err.Error(), ctapfd.ErrorCodePortUnavailable) {
		t.Fatalf("AcquireConfig error = %v, want provider error", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("provider: %v", err)
	}
}

func serveOneTapFDSocket(t *testing.T, sock string, respond func(conn *net.UnixConn, line string) error) (<-chan string, <-chan error) {
	t.Helper()
	addr, err := net.ResolveUnixAddr("unix", sock)
	if err != nil {
		t.Fatalf("resolve unix: %v", err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(sock)
	})

	reqCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.AcceptUnix()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		reqCh <- line
		errCh <- respond(conn, line)
	}()
	return reqCh, errCh
}

func TestRetryBusyBoundAndScope(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failures    int
		err         error
		wantCalls   int
		wantTimeout bool
	}{
		{"transient", 2, unix.EBUSY, 3, false},
		{"not-attached", 100, unix.ENOENT, 1, false},
		{"busy-deadline", 100, unix.EBUSY, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			f, _, _, err := retryBusy(context.Background(), 65*time.Millisecond, func(_ context.Context, remaining time.Duration) (*os.File, *os.File, Metadata, error) {
				calls++
				if remaining <= 0 {
					t.Fatal("attempt after deadline")
				}
				if calls <= tc.failures {
					return nil, nil, Metadata{}, tc.err
				}
				f, e := os.Open(os.DevNull)
				return f, nil, Metadata{}, e
			})
			if f != nil {
				defer f.Close()
			}
			if tc.wantTimeout {
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, unix.EBUSY) || calls < 2 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			} else {
				if calls != tc.wantCalls {
					t.Fatalf("calls=%d", calls)
				}
				if tc.failures < tc.wantCalls && err != nil {
					t.Fatal(err)
				}
				if tc.name == "not-attached" && !errors.Is(err, unix.ENOENT) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestBusyProviderErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    bool
	}{
		{"tapfd provider error PROVIDER_INTERNAL: open_tap_sw0-t1:_ioctl_TUNSETIFF_sw0-t1:_device_or_resource_busy", true},
		{"tapfd provider error PORT_UNAVAILABLE: device_or_resource_busy", false},
		{"tapfd provider error PROVIDER_INTERNAL: open_tap_sw0-t1:_ioctl_TUNSETIFF_sw0-t2:_device_or_resource_busy", false},
		{"tapfd provider error PROVIDER_INTERNAL: open_tap_sw0-t1:_ioctl_TUNSETIFF_sw0-t1:_operation_not_permitted", false},
	} {
		if got := socketTapBusy(errors.New(tc.message)); got != tc.want {
			t.Fatalf("%s: %t", tc.message, got)
		}
	}
	if !helperTapBusy("open tap sw0-t1: ioctl TUNSETIFF sw0-t1: device or resource busy\n") {
		t.Fatal("expected exact exec helper busy")
	}
	if helperTapBusy("unrelated device or resource busy") {
		t.Fatal("unrelated error classified as TAP busy")
	}
}

func TestRetryBusyCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, _, _, err := retryBusy(ctx, time.Second, func(context.Context, time.Duration) (*os.File, *os.File, Metadata, error) {
		calls++
		cancel()
		return nil, nil, Metadata{}, unix.EBUSY
	})
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

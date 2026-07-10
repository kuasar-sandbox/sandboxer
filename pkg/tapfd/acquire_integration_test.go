//go:build integration

package tapfd

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const tunDev = "/dev/net/tun"

// ifReq mirrors struct ifreq for TUNSETIFF/TUNGETIFF (name + flags, padded to 40).
type ifReq struct {
	Name  [16]byte
	Flags uint16
	_     [22]byte
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// makePersistentTap creates a persistent tap (no vnet_hdr, like a provider's
// `provision`) and releases its create-queue, leaving it attachable. Cleanup
// removes the device.
func makePersistentTap(t *testing.T, name string) {
	t.Helper()
	fd, err := unix.Open(tunDev, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open %s: %v", tunDev, err)
	}
	var req ifReq
	copy(req.Name[:], name)
	req.Flags = unix.IFF_TAP | unix.IFF_NO_PI
	if err := ioctl(uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&req))); err != nil {
		unix.Close(fd)
		t.Fatalf("TUNSETIFF create %s: %v", name, err)
	}
	if err := ioctl(uintptr(fd), uintptr(unix.TUNSETPERSIST), 1); err != nil {
		unix.Close(fd)
		t.Fatalf("TUNSETPERSIST(1): %v", err)
	}
	unix.Close(fd) // release create-queue; persistent device remains

	t.Cleanup(func() {
		cfd, err := unix.Open(tunDev, unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			return
		}
		defer unix.Close(cfd)
		var r ifReq
		copy(r.Name[:], name)
		r.Flags = unix.IFF_TAP | unix.IFF_NO_PI
		if ioctl(uintptr(cfd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&r))) == nil {
			_ = ioctl(uintptr(cfd), uintptr(unix.TUNSETPERSIST), 0)
		}
	})
}

func queueFlags(t *testing.T, f *os.File) uint16 {
	t.Helper()
	var req ifReq
	if err := ioctl(f.Fd(), uintptr(unix.TUNGETIFF), uintptr(unsafe.Pointer(&req))); err != nil {
		t.Fatalf("TUNGETIFF: %v", err)
	}
	return req.Flags
}

// TestHelperProcess is re-exec'd by TestAcquireHandoffIT as the §5 provider
// helper. Guarded by an env var so it is a no-op in the normal test run.
// It mirrors a real provider: read TAPFD_SOCKET, open the tap with vnet_hdr,
// and SCM_RIGHTS-send the queue fd + metadata, then exit 0.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_TAPFD_HELPER") != "1" {
		return
	}
	// The tap name is the arg after "--".
	var tapName string
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			tapName = os.Args[i+1]
		}
	}
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, format, a...)
		os.Exit(2)
	}

	spec := os.Getenv("TAPFD_SOCKET") // "fd=N"
	n, err := strconv.Atoi(strings.TrimPrefix(spec, "fd="))
	if err != nil {
		fail("helper: bad TAPFD_SOCKET %q", spec)
	}
	conn, err := net.FileConn(os.NewFile(uintptr(n), "sock"))
	if err != nil {
		fail("helper: fileconn: %v", err)
	}
	uconn := conn.(*net.UnixConn)

	tap, err := openTapVnetHdr(tapName)
	if err != nil {
		fail("helper: open tap: %v", err)
	}
	payload := []byte("port=1 mac=02:00:00:00:80:01 ip=169.254.4.1 fd=1\x00")
	if _, _, err := uconn.WriteMsgUnix(payload, unix.UnixRights(int(tap.Fd())), nil); err != nil {
		fail("helper: send fd: %v", err)
	}
	_ = tap.Close()
	os.Exit(0)
}

// openTapVnetHdr attaches a queue fd (IFF_TAP|IFF_NO_PI|IFF_VNET_HDR) to an
// existing persistent tap — what a real provider hands off.
func openTapVnetHdr(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var req ifReq
	copy(req.Name[:], name)
	req.Flags = unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR
	if err := ioctl(uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&req))); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// TestAcquireHandoffIT drives the full §5 exec-helper handoff against a real
// subprocess: Acquire execs the helper, which hands over a vnet_hdr tap fd, and
// Acquire returns it with parsed metadata. Verifies the fd carries IFF_VNET_HDR.
func TestAcquireHandoffIT(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root (creates a tap device)")
	}
	const name = "tapfd-acq-it"
	makePersistentTap(t, name) // from open_integration_test.go

	t.Setenv("GO_TAPFD_HELPER", "1") // inherited by the re-exec'd helper child
	argv := []string{os.Args[0], "-test.run=TestHelperProcess", "--", name}

	f, _, meta, err := Acquire(context.Background(), argv, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer f.Close()

	if meta.MAC != "02:00:00:00:80:01" || meta.IP != "169.254.4.1" {
		t.Errorf("meta = %+v", meta)
	}
	if fl := queueFlags(t, f); fl&unix.IFF_VNET_HDR == 0 {
		t.Errorf("handed-off fd lacks IFF_VNET_HDR (flags %#x)", fl)
	}
}

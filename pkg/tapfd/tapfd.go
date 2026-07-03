// Package tapfd is sandbox-ctl's consumer side of the tapfd handoff
// protocol (docs/tapfd.md): it execs a provider helper and receives a tap
// queue fd (with virtio-net header) plus metadata over a unix socket via
// SCM_RIGHTS, so CH can drive virtio-net off a fd the network provider owns.
//
// The wire receive + payload parse are delegated to the canonical
// connector/pkg/tapfd; this package adds only the exec-helper
// orchestration and projects the port metadata onto the subset sandbox-ctl
// needs.
package tapfd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	vsw "github.com/kuasar-sandbox/connector/pkg/tapfd"
)

// DefaultTimeout bounds the whole handoff (exec helper → recv fd → helper exit).
const DefaultTimeout = 5 * time.Second

// Metadata is the recognized subset of the handoff payload (docs/tapfd.md
// §2.3) sandbox-ctl acts on. Unknown keys are ignored per the protocol's
// forward-compat rule (handled by the vswitch parser).
type Metadata struct {
	MAC string // provider-assigned MAC the VMM must mirror onto virtio-net
	IP  string // interface L3 address (may be bare, no mask)
	MTU int    // interface MTU (0 = absent)
}

// Acquire runs the §3 exec-helper handoff: it creates a connected socketpair,
// execs argv with TAPFD_SOCKET=fd=3 (the helper's inherited end) and
// TAPFD_WANT_NETNS=1 (§3.4 — request the tap's netns fd), receives exactly
// one tap fd + metadata + an optional netns fd, and requires the helper to exit
// 0. The whole exchange is bounded by timeout (<=0 → DefaultTimeout). On success
// the caller owns the returned files: tap (hand to CH via cmd.ExtraFiles) and,
// when the provider's tap is netns-isolated, netns (nil otherwise — used to
// launch CH inside the tap's network namespace, §2.5). Close both after the run.
func Acquire(ctx context.Context, argv []string, timeout time.Duration) (tap *os.File, netns *os.File, meta Metadata, err error) {
	if len(argv) == 0 {
		return nil, nil, Metadata{}, errors.New("tapfd: empty exec argv")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: socketpair: %w", err)
	}
	recvFile := os.NewFile(uintptr(sp[0]), "tapfd-recv")
	helperFile := os.NewFile(uintptr(sp[1]), "tapfd-helper")

	// net.FileConn dups recvFile into its own fd, so close our copy after.
	conn, err := net.FileConn(recvFile)
	_ = recvFile.Close()
	if err != nil {
		_ = helperFile.Close()
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: fileconn: %w", err)
	}
	uconn := conn.(*net.UnixConn)
	defer uconn.Close()

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	// helperFile becomes the child's fd 3 (cmd.ExtraFiles[0]); the helper dials
	// it because we point TAPFD_SOCKET at it (docs/tapfd.md §3.3).
	// TAPFD_WANT_NETNS=1 requests the tap's netns fd (§3.4) so we can launch
	// CH inside it; a provider whose tap isn't netns-isolated simply omits it.
	cmd.Env = append(os.Environ(), "TAPFD_SOCKET=fd=3", "TAPFD_WANT_NETNS=1")
	cmd.ExtraFiles = []*os.File{helperFile}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		_ = helperFile.Close()
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: exec %s: %w", argv[0], err)
	}
	_ = helperFile.Close() // the child holds its own copy now

	_ = uconn.SetDeadline(time.Now().Add(timeout))
	tapFiles, netnsFile, meta, rerr := RecvFd(uconn)
	werr := cmd.Wait() // helper sends one message then exits (§3.5)

	if rerr != nil {
		closeAll(tapFiles)
		closeFile(netnsFile)
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: recv from helper %s: %w (helper exit: %v, stderr=%q)",
			argv[0], rerr, werr, strings.TrimSpace(stderr.String()))
	}
	if werr != nil { // §3.1: non-zero exit or timeout ⇒ failure, do not use the nic
		closeAll(tapFiles)
		closeFile(netnsFile)
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: helper %s exited non-zero: %w (stderr=%q)",
			argv[0], werr, strings.TrimSpace(stderr.String()))
	}
	if len(tapFiles) != 1 { // single-queue v1
		closeAll(tapFiles)
		closeFile(netnsFile)
		return nil, nil, Metadata{}, fmt.Errorf("tapfd: expected 1 tap fd, received %d", len(tapFiles))
	}
	return tapFiles[0], netnsFile, meta, nil
}

// RecvFd performs the §2.4 receive by delegating to the canonical
// connector tapfd library (recvmsg, SCM_RIGHTS fd collection, payload
// parse, fd-count cross-check, and the positional tap/netns split), then
// projects the resulting PortMetadata onto the MAC/IP/MTU subset sandbox-ctl
// acts on. netnsFile is nil when the provider attached none. Any error closes
// all received fds.
func RecvFd(conn *net.UnixConn) (tapFiles []*os.File, netnsFile *os.File, meta Metadata, err error) {
	taps, netnsF, pm, rerr := vsw.RecvFdsWithNetns(conn)
	if rerr != nil {
		return nil, nil, Metadata{}, rerr
	}
	return taps, netnsF, Metadata{MAC: pm.MAC, IP: pm.InnerIP, MTU: int(pm.MTU)}, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

func closeFile(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

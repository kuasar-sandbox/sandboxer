package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

// No parallelism: this test deliberately reclaims one retired descriptor.
func TestRetiredMUXWriterDoesNotReachReusedFD(t *testing.T) {
	const budget = 2 * time.Second
	wait := func(name string, done <-chan struct{}) bool {
		t.Helper()
		timer := time.NewTimer(budget)
		defer timer.Stop()
		select {
		case <-done:
			return true
		case <-timer.C:
			t.Errorf("timed out joining %s", name)
			return false
		}
	}
	pair := func() [2]int {
		t.Helper()
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		return fds
	}
	// F_DUPFD_CLOEXEC reserves an unused high fd atomically. Unlike dup3,
	// it cannot overwrite an unrelated live owner, even on setup failure.
	duplicate := func(fd, minimum int) int {
		t.Helper()
		n, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, minimum)
		if err != nil {
			t.Fatalf("reserve descriptor >= %d: %v", minimum, err)
		}
		return n
	}
	old := pair()
	// These low descriptors stay owned until both high reservations exist.
	// Their cleanup runs after session cleanup on every failure path.
	t.Cleanup(func() { _ = unix.Close(old[0]); _ = unix.Close(old[1]) })
	guest := &vsockConn{fd: duplicate(old[0], 10_000)}
	t.Cleanup(func() { _ = guest.Close() })
	host := &vsockConn{fd: duplicate(old[1], 10_000)}
	t.Cleanup(func() { _ = host.Close() })
	for _, c := range []*vsockConn{guest, host} {
		if err := c.SetDeadline(time.Now().Add(budget)); err != nil {
			t.Fatal(err)
		}
	}
	guestMUX := mux.NewSession(guest, mux.PipeStreams(false, true, false), mux.Options{})
	hostMUX := mux.NewSession(host, mux.PipeStreams(false, true, false), mux.Options{})
	var handshakeDone <-chan struct{}
	t.Cleanup(func() {
		// shutdown wakes raw syscall readers before joining them. These
		// methods honor closed state, so cleanup cannot touch the reused fd.
		_ = guest.shutdown()
		_ = host.shutdown()
		wait("guest reader", guestMUX.Done())
		wait("host reader", hostMUX.Done())
		if handshakeDone != nil {
			wait("close handshake", handshakeDone)
		}
		_ = guest.Close()
		_ = host.Close()
	})
	retained := guestMUX.Stream(mux.StreamStdout)
	// Retaining this handle and invoking it later fixes the pump's schedule
	// immediately before CloseWrite without modifying production code.
	done := make(chan struct{})
	handshakeDone = done
	var handshakeErr error
	go func() {
		defer close(done)
		handshakeErr = guestMUX.InitMuxClose()
	}()
	if !wait("close handshake", done) || !wait("guest reader", guestMUX.Done()) || !wait("host reader", hostMUX.Done()) {
		t.FailNow()
	}
	if handshakeErr != nil || guestMUX.Err() != nil || hostMUX.Err() != nil {
		t.Fatalf("MUX_CLOSE: handshake=%v guest=%v host=%v", handshakeErr, guestMUX.Err(), hostMUX.Err())
	}
	select {
	case <-hostMUX.PeerClosed():
	default:
		t.Fatal("host never received MUX_CLOSE")
	}
	if err := guest.Close(); err != nil {
		t.Fatal(err)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	// Release the original socketpair aliases too: no old transport survives.
	for i, fd := range old {
		if err := unix.Close(fd); err != nil {
			t.Fatal(err)
		}
		old[i] = -1
	}

	replacement := pair()
	t.Cleanup(func() { _ = unix.Close(replacement[0]); _ = unix.Close(replacement[1]) })
	// If socketpair itself acquired the target, use that endpoint as guest.
	// Never dup an fd onto itself or close an endpoint still in use.
	if replacement[1] == guest.fd {
		replacement[0], replacement[1] = replacement[1], replacement[0]
	}
	if replacement[0] != guest.fd {
		fd := duplicate(replacement[0], guest.fd)
		if fd != guest.fd {
			_ = unix.Close(fd)
			t.Fatalf("retired fd %d acquired by another owner; refusing to overwrite it", guest.fd)
		}
		_ = unix.Close(replacement[0])
		replacement[0] = fd
	}
	newGuest, newHost := &vsockConn{fd: replacement[0]}, &vsockConn{fd: replacement[1]}
	if err := newGuest.SetDeadline(time.Now().Add(budget)); err != nil {
		t.Fatal(err)
	}
	// Synchronous production call: its return is the writer join. No sleep,
	// scheduler race, fabricated frame writer, or background pump is needed.
	closeErr := retained.CloseWrite()
	var peek [16]byte
	n, _, err := unix.Recvfrom(newHost.fd, peek[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
	stale := n > 0
	if stale {
		t.Errorf("retired session wrote % x into replacement socket; want zero bytes (CloseWrite=%v)", peek[:n], closeErr)
		if !bytes.Equal(peek[:n], []byte{0x02, 0x01, 0x00, 0x00}) {
			t.Fatalf("unexpected retired-session prefix: % x", peek[:n])
		}
	} else if !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("replacement peer: n=%d err=%v; want EAGAIN with zero bytes", n, err)
	}

	// Same typed response as usageReattachFixture, with fixed durations.
	response := &proto.Message{Type: proto.TypeUsageResponse, UsageResponse: &proto.UsageResponse{
		RunEpoch: "old-host", RequestID: 101,
		Memory: proto.UsageMemory{
			UsageReadState: proto.UsageReadState{Status: proto.UsageOK, DurationNS: 100},
			Domain:         "0/Normal", PresentPages: usageUint(100), BuddyFreePages: usageUint(10),
			PCPFreePages: usageUint(2), PageSize: usageUint(4096),
		},
		Filesystems: []proto.UsageFilesystem{{
			UsageReadState: proto.UsageReadState{Status: proto.UsageOK, DurationNS: 100},
			Disk:           "root", Incarnation: "root/1", Blocks: usageUint(100), BFree: usageUint(40),
			BlockSize: usageUint(4096), FragmentSize: usageUint(4096), Type: usageUint(unix.EXT4_SUPER_MAGIC),
		}},
	}}
	var wire bytes.Buffer
	if err := proto.WriteMessage(&wire, response); err != nil {
		t.Fatal(err)
	}
	if wire.Len() != 4+425 || !bytes.Equal(wire.Bytes()[:4], []byte{0xa9, 0x01, 0x00, 0x00}) {
		t.Fatalf("usage frame: length=%d header=% x; want 425-byte JSON", wire.Len(), wire.Bytes()[:4])
	}
	if _, err := proto.ReadMessage(bytes.NewReader(wire.Bytes())); err != nil {
		t.Fatalf("genuine response is invalid: %v", err)
	}
	// Keep the framing demonstration meaningful even after the lifetime fix.
	prefix := []byte{0x02, 0x01, 0x00, 0x00}
	if binary.LittleEndian.Uint32(prefix) != 258 || wire.Bytes()[0] != 0xa9 {
		t.Fatal("unexpected corrupt framing")
	}
	assertCorrupt := func(err error) {
		t.Helper()
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) || syntax.Offset != 1 || !strings.Contains(err.Error(), "invalid character '©' looking for beginning of value") {
			t.Fatalf("want invalid JSON beginning with byte 0xa9, got %v", err)
		}
	}
	_, corruptErr := proto.ReadMessage(bytes.NewReader(append(prefix, wire.Bytes()...)))
	assertCorrupt(corruptErr)
	// Send the genuine response after the completed stale writer. MSG_PEEK
	// left its bytes queued, so this exercises the actual contaminated socket.
	if err := proto.WriteMessage(usageIO{newGuest, time.Now().Add(budget)}, response); err != nil {
		t.Fatal(err)
	}
	got, readErr := proto.ReadMessage(usageIO{newHost, time.Now().Add(budget)})
	if stale {
		assertCorrupt(readErr)
	} else if readErr != nil || got.UsageResponse == nil || got.UsageResponse.RequestID != 101 {
		t.Fatalf("replacement response: %+v, %v", got, readErr)
	}
}

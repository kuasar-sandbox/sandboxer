package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/fwd"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestConnRegistry(t *testing.T) {
	r := newConnRegistry()
	s1, s2 := &connSession{}, &connSession{}
	if !r.add(s1) || !r.add(s2) {
		t.Fatal("add should succeed before quiesce")
	}
	if r.isQuiescing() {
		t.Fatal("not quiescing yet")
	}
	live := r.beginQuiesce()
	if len(live) != 2 {
		t.Fatalf("beginQuiesce returned %d sessions, want 2", len(live))
	}
	if !r.isQuiescing() {
		t.Fatal("should be quiescing after beginQuiesce")
	}
	if r.add(&connSession{}) {
		t.Fatal("add must be rejected while quiescing")
	}
	r.endQuiesce()
	if r.isQuiescing() {
		t.Fatal("endQuiesce should clear quiescing")
	}
	if !r.add(&connSession{}) {
		t.Fatal("add should succeed again after endQuiesce")
	}
	r.remove(s1)
	r.remove(s2)
}

// TestRunConnectSession drives a full guest connect session: a socketpair
// stands in for the reverse-channel vsock conn, the target is a loopback
// echo server. It checks the connect_ack handshake, DATA relay both ways,
// and that a host half-close (EOF frame) propagates to the target and the
// target's resulting EOF comes back as an EOF frame.
func TestRunConnectSession(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(conn, conn) // echo until the client half-closes
		_ = conn.Close()
	}()

	hostFD, guestFD := socketpairFDs(t)
	hostC := &vsockConn{fd: hostFD}
	guestC := &vsockConn{fd: guestFD}
	defer hostC.Close()

	sup := &supervisorState{connReg: newConnRegistry()}
	go runConnectSession(guestC, &proto.Message{
		Type:    proto.TypeConnect,
		Connect: &proto.ConnectSpec{Address: ln.Addr().String()},
	}, sup)

	_ = hostC.SetDeadline(time.Now().Add(3 * time.Second))

	ack, err := proto.ReadMessage(hostC)
	if err != nil {
		t.Fatalf("read connect_ack: %v", err)
	}
	if ack.Type != proto.TypeConnectAck {
		t.Fatalf("got %q want connect_ack (msg=%q)", ack.Type, ack.Msg)
	}

	// DATA round-trips through the echo target.
	if err := fwd.WriteFrame(hostC, fwd.Frame{Type: fwd.FrameData, Payload: []byte("hi")}); err != nil {
		t.Fatalf("write DATA: %v", err)
	}
	f, err := fwd.ReadFrame(hostC)
	if err != nil {
		t.Fatalf("read echoed DATA: %v", err)
	}
	if f.Type != fwd.FrameData || !bytes.Equal(f.Payload, []byte("hi")) {
		t.Fatalf("echoed frame: type=%d payload=%q", f.Type, f.Payload)
	}

	// Host half-closes → target sees EOF → echo server closes → EOF frame back.
	if err := fwd.WriteFrame(hostC, fwd.Frame{Type: fwd.FrameEOF}); err != nil {
		t.Fatalf("write EOF: %v", err)
	}
	f, err = fwd.ReadFrame(hostC)
	if err != nil {
		t.Fatalf("read EOF frame: %v", err)
	}
	if f.Type != fwd.FrameEOF {
		t.Fatalf("got frame type %d want EOF", f.Type)
	}
}

// TestRunConnectSessionDialFail verifies a dial failure is reported as an
// error response (not a connect_ack) and the conn is closed.
func TestRunConnectSessionDialFail(t *testing.T) {
	hostFD, guestFD := socketpairFDs(t)
	hostC := &vsockConn{fd: hostFD}
	guestC := &vsockConn{fd: guestFD}
	defer hostC.Close()

	sup := &supervisorState{connReg: newConnRegistry()}
	go runConnectSession(guestC, &proto.Message{
		Type:    proto.TypeConnect,
		Connect: &proto.ConnectSpec{Address: "127.0.0.1:1"}, // nothing listening
	}, sup)

	_ = hostC.SetDeadline(time.Now().Add(3 * time.Second))
	resp, err := proto.ReadMessage(hostC)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Type != proto.TypeError {
		t.Fatalf("got %q want error", resp.Type)
	}
}

func socketpairFDs(t *testing.T) (int, int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	return fds[0], fds[1]
}

// TestRunAcceptSession drives a full guest accept-mode (`LOCAL::TARGET`)
// session: a socketpair stands in for the reverse-channel vsock conn. The
// guest Listen+Accepts on a unix path created lazily; a goroutine acting as
// the guest-side client dials that path and echoes. It checks the connect_ack
// is sent only after the client connects (the accept returns), DATA relays
// both ways, and a host half-close propagates and comes back as an EOF frame.
func TestRunAcceptSession(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "up.sock")

	hostFD, guestFD := socketpairFDs(t)
	hostC := &vsockConn{fd: hostFD}
	guestC := &vsockConn{fd: guestFD}
	defer hostC.Close()

	sup := &supervisorState{connReg: newConnRegistry(), acceptLn: newAcceptListeners()}
	go runConnectSession(guestC, &proto.Message{
		Type:    proto.TypeConnect,
		Connect: &proto.ConnectSpec{Network: "unix", Address: udsPath, Accept: true},
	}, sup)

	// Guest-side client: dial the lazily-created listener (retry until up) and
	// echo until half-close. The guest's Accept pairs this conn with hostC.
	clientReady := make(chan net.Conn, 1)
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			c, err := net.Dial("unix", udsPath)
			if err == nil {
				clientReady <- c
				_, _ = io.Copy(c, c)
				_ = c.Close()
				return
			}
			if time.Now().After(deadline) {
				clientReady <- nil
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// connect_ack arrives only after the guest's Accept returns (client dialed).
	_ = hostC.SetDeadline(time.Now().Add(3 * time.Second))
	ack, err := proto.ReadMessage(hostC)
	if err != nil {
		t.Fatalf("read connect_ack: %v", err)
	}
	if ack.Type != proto.TypeConnectAck {
		t.Fatalf("got %q want connect_ack (msg=%q)", ack.Type, ack.Msg)
	}
	if c := <-clientReady; c == nil {
		t.Fatal("guest-side client failed to dial the accept listener")
	}

	// DATA round-trips through the echo client.
	if err := fwd.WriteFrame(hostC, fwd.Frame{Type: fwd.FrameData, Payload: []byte("hi")}); err != nil {
		t.Fatalf("write DATA: %v", err)
	}
	f, err := fwd.ReadFrame(hostC)
	if err != nil {
		t.Fatalf("read echoed DATA: %v", err)
	}
	if f.Type != fwd.FrameData || !bytes.Equal(f.Payload, []byte("hi")) {
		t.Fatalf("echoed frame: type=%d payload=%q", f.Type, f.Payload)
	}

	// Host half-closes → client sees EOF → echo client closes → EOF frame back.
	if err := fwd.WriteFrame(hostC, fwd.Frame{Type: fwd.FrameEOF}); err != nil {
		t.Fatalf("write EOF: %v", err)
	}
	f, err = fwd.ReadFrame(hostC)
	if err != nil {
		t.Fatalf("read EOF frame: %v", err)
	}
	if f.Type != fwd.FrameEOF {
		t.Fatalf("got frame type %d want EOF", f.Type)
	}
}

// TestRunAcceptSessionQuiesce verifies a quiesce starting while an accept
// session is parked in Accept (no guest-side client yet) cancels it: the
// cached listener is closed (unblocking Accept) and the reverse conn is
// lingered shut, so the host's read ends without a connect_ack and the
// session goroutine returns.
func TestRunAcceptSessionQuiesce(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "up.sock")

	hostFD, guestFD := socketpairFDs(t)
	hostC := &vsockConn{fd: hostFD}
	guestC := &vsockConn{fd: guestFD}
	defer hostC.Close()

	sup := &supervisorState{connReg: newConnRegistry(), acceptLn: newAcceptListeners()}
	done := make(chan struct{})
	go func() {
		runConnectSession(guestC, &proto.Message{
			Type:    proto.TypeConnect,
			Connect: &proto.ConnectSpec{Network: "unix", Address: udsPath, Accept: true},
		}, sup)
		close(done)
	}()

	// Wait until the listener exists: runAcceptSession registers the session
	// (reg.add) before getOrCreate binds it, so a visible socket node implies
	// the session is registered and beginQuiesce will tear it down.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(udsPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accept listener never came up")
		}
		time.Sleep(5 * time.Millisecond)
	}

	closeConnectSessions(sup.connReg, sup.acceptLn)

	// The parked session's reverse conn was lingered shut by the teardown, so
	// the peer (hostC) is at EOF. vsockConn.Read is a raw syscall.Read, so EOF
	// surfaces as a single (0, nil) read (not io.EOF) — assert that directly
	// rather than via proto.ReadMessage (whose io.ReadFull would spin on it).
	if n, _ := hostC.Read(make([]byte, 1)); n != 0 {
		t.Errorf("expected reverse conn at EOF after quiesce, read %d bytes", n)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("runConnectSession did not return after quiesce")
	}
}

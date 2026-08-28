package sandbox

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestParseForwardSpec(t *testing.T) {
	ok := []struct {
		in      string
		udsPath string
		fd      int
		network string
		addr    string
		accept  bool
	}{
		// dial mode, tcp target (LOCAL:host:port)
		{"/run/envd.sock:127.0.0.1:49983", "/run/envd.sock", 0, "tcp", "127.0.0.1:49983", false},
		{"fd=3:127.0.0.1:49983", "", 3, "tcp", "127.0.0.1:49983", false},
		{"@envd:[::1]:8080", "@envd", 0, "tcp", "[::1]:8080", false},
		{"fd=7:localhost:80", "", 7, "tcp", "localhost:80", false},
		{"./rel.sock:10.0.0.1:5432", "./rel.sock", 0, "tcp", "10.0.0.1:5432", false},
		// dial mode, unix target (①): '/' or '@' prefix ⇒ unix
		{"/run/db.sock:/var/run/pg.sock", "/run/db.sock", 0, "unix", "/var/run/pg.sock", false},
		{"/run/db.sock:@pg", "/run/db.sock", 0, "unix", "@pg", false},
		// accept mode, tcp target (②): LOCAL::host:port
		{"/run/api.sock::0.0.0.0:8080", "/run/api.sock", 0, "tcp", "0.0.0.0:8080", true},
		{"/run/api.sock::[::1]:8080", "/run/api.sock", 0, "tcp", "[::1]:8080", true},
		{"fd=3::0.0.0.0:8080", "", 3, "tcp", "0.0.0.0:8080", true}, // fd= local is valid in accept mode
		// accept mode, unix target (③): LOCAL::/path or ::@abstract
		{"/run/api.sock::/run/up.sock", "/run/api.sock", 0, "unix", "/run/up.sock", true},
		{"@api::@up", "@api", 0, "unix", "@up", true},
	}
	for _, tc := range ok {
		got, err := ParseForwardSpec(tc.in)
		if err != nil {
			t.Errorf("ParseForwardSpec(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got.UDSPath != tc.udsPath || got.ListenFD != tc.fd || got.Network != tc.network ||
			got.Address != tc.addr || got.Accept != tc.accept {
			t.Errorf("ParseForwardSpec(%q) = {uds:%q fd:%d net:%q addr:%q accept:%v}, want {uds:%q fd:%d net:%q addr:%q accept:%v}",
				tc.in, got.UDSPath, got.ListenFD, got.Network, got.Address, got.Accept,
				tc.udsPath, tc.fd, tc.network, tc.addr, tc.accept)
		}
	}

	bad := []string{
		"",                      // empty
		"nocolons",              // no colon at all
		"/p:127.0.0.1",          // target missing port
		"/p:127.0.0.1:",         // empty port
		"/p:127.0.0.1:notaport", // non-numeric port
		":127.0.0.1:80",         // empty local endpoint
		"fd=0:127.0.0.1:80",     // fd must be > 0
		"fd=x:127.0.0.1:80",     // non-numeric fd
		"fd=-1:127.0.0.1:80",    // negative fd
		"/p:",                   // empty target (dial)
		"/p::",                  // empty target (accept)
		"/p:rel/sock",           // relative unix target (not '/' or '@') parses as bad host:port
		"/p::rel.sock",          // relative unix target in accept mode
	}
	for _, in := range bad {
		if _, err := ParseForwardSpec(in); err == nil {
			t.Errorf("ParseForwardSpec(%q): expected error, got nil", in)
		}
	}
}

// TestForwarderAcceptAndClose drives the host accept path: a UDS listener
// accepts a local connection, serve() tries to reach the (absent) guest
// via a bogus vsock base and therefore closes the local conn; after Close
// the listener is gone.
func TestForwarderAcceptAndClose(t *testing.T) {
	dir := t.TempDir()
	udsPath := filepath.Join(dir, "fwd.sock")
	spec, err := ParseForwardSpec(udsPath + ":127.0.0.1:49983")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	f := NewForwarder(filepath.Join(dir, "nonexistent-vsock.sock"), func(string, ...any) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.Start(ctx, []ForwardSpec{spec}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	c, err := net.DialTimeout("unix", udsPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial listener: %v", err)
	}
	// serve() can't reach the guest (bogus vsock base) → it closes the conn.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Error("expected local conn to be closed by serve (guest unreachable)")
	}
	_ = c.Close()

	f.Close()
	if _, err := net.DialTimeout("unix", udsPath, 500*time.Millisecond); err == nil {
		t.Error("expected dial to fail after Close (listener removed)")
	}
}

func TestForwarderPauseThenDrainClosesDialHandshake(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")
	ln, err := net.Listen("unix", base)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	handshake := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		line := make([]byte, len(proto.HostConnectLine))
		if _, err := io.ReadFull(c, line); err != nil {
			serverDone <- err
			return
		}
		if string(line) != string(proto.HostConnectLine) {
			serverDone <- fmt.Errorf("CONNECT line = %q", line)
			return
		}
		if _, err := c.Write([]byte("OK 1\n")); err != nil {
			serverDone <- err
			return
		}
		req, err := proto.ReadMessage(c)
		if err != nil {
			serverDone <- err
			return
		}
		if req.Type != proto.TypeConnect {
			serverDone <- fmt.Errorf("request type = %q", req.Type)
			return
		}
		close(handshake)
		if _, err := c.Read(make([]byte, 1)); err == nil {
			serverDone <- fmt.Errorf("handshake connection remained open")
			return
		}
		serverDone <- nil
	}()

	f := NewForwarder(base, func(string, ...any) {})
	local, peer := net.Pipe()
	defer peer.Close()
	go f.serve(local, ForwardSpec{Raw: "test", Network: "tcp", Address: "127.0.0.1:1"})

	select {
	case <-handshake:
	case err := <-serverDone:
		t.Fatalf("handshake setup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("dial handshake did not start")
	}

	drained := make(chan struct{})
	go func() {
		f.Pause()
		f.Drain()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("Pause then Drain did not join the admitted handshake")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestForwarderExecAdmissionFollowsCaptureGate(t *testing.T) {
	f := NewForwarder("", func(string, ...any) {})
	if !f.ExecAllowed() {
		t.Fatal("new forwarder rejected exec admission")
	}
	f.Pause()
	f.Drain()
	if f.ExecAllowed() {
		t.Fatal("paused forwarder admitted exec")
	}
	f.Resume()
	if !f.ExecAllowed() {
		t.Fatal("resumed forwarder rejected exec admission")
	}
	f.Close()
	if f.ExecAllowed() {
		t.Fatal("closed forwarder admitted exec")
	}
}

func TestForwarderPauseThenGuestQuiesceClosesAndJoinsExec(t *testing.T) {
	f := NewForwarder("", func(string, ...any) {})
	handler, peer := net.Pipe()
	defer peer.Close()
	_, admitted := f.beginExec(context.Background(), handler)
	if !admitted {
		t.Fatal("exec admission rejected before capture")
	}

	firstRead := make(chan struct{})
	connClosed := make(chan struct{})
	releaseHandler := make(chan struct{})
	go func() {
		defer f.endExec(handler)
		_, _ = handler.Read(make([]byte, 1))
		close(firstRead)
		_, _ = handler.Read(make([]byte, 1))
		close(connClosed)
		<-releaseHandler
	}()

	f.Pause()
	if err := peer.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write([]byte{1}); err != nil {
		t.Fatalf("Pause closed the exec transport before guest quiesce: %v", err)
	}
	if err := peer.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstRead:
	case <-time.After(3 * time.Second):
		t.Fatal("admitted exec transport stopped carrying data after Pause")
	}

	select {
	case <-connClosed:
		t.Fatal("Pause closed the exec transport before guest quiesce")
	default:
	}
	drained := make(chan struct{})
	go func() {
		f.Drain()
		close(drained)
	}()
	select {
	case <-connClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("post-quiesce Drain did not close the admitted exec connection")
	}
	select {
	case <-drained:
		t.Fatal("Drain returned before the exec handler exited")
	default:
	}
	if _, admitted := f.beginExec(context.Background(), peer); admitted {
		t.Fatal("capture gate admitted a new exec while draining")
	}
	close(releaseHandler)
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("Drain did not join the admitted exec handler")
	}
}

func TestForwarderDrainAfterGuestQuiesceCancelsResidualExecContext(t *testing.T) {
	f := NewForwarder("", func(string, ...any) {})
	handler, peer := net.Pipe()
	defer handler.Close()
	defer peer.Close()
	execCtx, admitted := f.beginExec(context.Background(), handler)
	if !admitted {
		t.Fatal("exec admission rejected before capture")
	}

	handlerDone := make(chan struct{})
	go func() {
		defer f.endExec(handler)
		<-execCtx.Done()
		close(handlerDone)
	}()
	f.Pause()
	select {
	case <-execCtx.Done():
		t.Fatal("capture canceled the guest transport before guest quiesce")
	default:
	}
	drained := make(chan struct{})
	go func() {
		f.Drain()
		close(drained)
	}()
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("post-quiesce Drain did not cancel the residual exec transport")
	}
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("Drain did not join the canceled residual exec handler")
	}
}

func TestForwarderAbortAndDrainCancelsAdmittedExecContext(t *testing.T) {
	f := NewForwarder("", func(string, ...any) {})
	handler, peer := net.Pipe()
	defer handler.Close()
	defer peer.Close()
	execCtx, admitted := f.beginExec(context.Background(), handler)
	if !admitted {
		t.Fatal("exec admission rejected before capture")
	}

	handlerDone := make(chan struct{})
	go func() {
		defer f.endExec(handler)
		<-execCtx.Done()
		close(handlerDone)
	}()
	f.Pause()
	f.AbortAndDrain()
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("abort did not cancel and join the admitted exec handler")
	}
}

package guestlink

import (
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// fakeCHProxy stands in for cloud-hypervisor's hybrid vsock proxy on the
// host side: listens on the base UDS, reads "CONNECT 5000\n" byte-by-byte
// (no bufio so read cursor stays at the next byte after \n), then hands
// the conn to a guest handler that does proto.ReadMessage / WriteMessage.
type fakeCHProxy struct {
	listener net.Listener
	guestFn  func(net.Conn)
	wg       sync.WaitGroup
}

func newFakeCHProxy(t *testing.T, basePath string, guestFn func(net.Conn)) *fakeCHProxy {
	t.Helper()
	l, err := net.Listen("unix", basePath)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeCHProxy{listener: l, guestFn: guestFn}
	p.wg.Add(1)
	go p.accept()
	return p
}

func (p *fakeCHProxy) accept() {
	defer p.wg.Done()
	for {
		c, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handleConn(c)
	}
}

func (p *fakeCHProxy) handleConn(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	// Read CONNECT line one byte at a time; do NOT use bufio (would
	// consume bytes past \n that belong to the proto message).
	one := make([]byte, 1)
	var line []byte
	for {
		if _, err := c.Read(one); err != nil {
			return
		}
		line = append(line, one[0])
		if one[0] == '\n' {
			break
		}
		if len(line) > 64 {
			return
		}
	}
	if string(line) != "CONNECT 5000\n" {
		return
	}
	// Mirror real CH hybrid vsock: reply "OK <localPort>\n" before
	// forwarding bytes between host and guest. Our HostClient drains
	// this line; production code does the same.
	if _, err := c.Write([]byte("OK 1\n")); err != nil {
		return
	}
	p.guestFn(c)
}

func (p *fakeCHProxy) close() {
	_ = p.listener.Close()
	p.wg.Wait()
}

func TestHostClient_PingPong(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		req, err := proto.ReadMessage(c)
		if err != nil {
			t.Errorf("guest read: %v", err)
			return
		}
		if req.Type != proto.TypePing {
			t.Errorf("guest got %s, want ping", req.Type)
			return
		}
		_ = proto.WriteMessage(c, &proto.Message{
			Type:    proto.TypePong,
			ID:      req.ID,
			TSendNs: req.TSendNs,
		})
	})
	defer proxy.close()

	c := &HostClient{BasePath: base}
	resp, err := c.RoundTrip(&proto.Message{
		Type:    proto.TypePing,
		ID:      1,
		TSendNs: time.Now().UnixNano(),
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.Type != proto.TypePong || resp.ID != 1 {
		t.Errorf("got %+v", resp)
	}
}

func TestHostClient_RestoreEpoch(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		req, _ := proto.ReadMessage(c)
		if req.Type != proto.TypeRestore || req.Epoch != 7 {
			t.Errorf("guest got %+v", req)
		}
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeRestoreAck})
	})
	defer proxy.close()

	c := &HostClient{BasePath: base}
	resp, err := c.RoundTrip(&proto.Message{Type: proto.TypeRestore, Epoch: 7}, 2*time.Second)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.Type != proto.TypeRestoreAck {
		t.Errorf("got %+v", resp)
	}
}

func TestHostClient_DialError(t *testing.T) {
	c := &HostClient{BasePath: "/nonexistent/path/vsock.sock"}
	_, err := c.RoundTrip(&proto.Message{Type: proto.TypePing, ID: 1}, 200*time.Millisecond)
	if err == nil {
		t.Error("expected dial error")
	}
}

func TestPingStats_RTTPercentiles(t *testing.T) {
	var s PingStats
	for i := 1; i <= 100; i++ {
		s.Attempts.Add(1)
		s.Success.Add(1)
		s.observeRTT(time.Duration(i) * time.Microsecond)
	}
	snap := s.Snapshot()
	if snap.Success != 100 {
		t.Errorf("Success=%d", snap.Success)
	}
	// p50 of 1..100 µs ≈ 50 µs (= 50_000 ns; index = 50*99/100 = 49 → 50µs).
	if snap.RTTP50Ns < 40_000 || snap.RTTP50Ns > 60_000 {
		t.Errorf("p50=%d ns, want ≈50000", snap.RTTP50Ns)
	}
	if snap.RTTMaxNs != 100_000 {
		t.Errorf("max=%d, want 100000", snap.RTTMaxNs)
	}
}

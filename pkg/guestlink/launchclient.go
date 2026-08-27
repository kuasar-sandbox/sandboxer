package guestlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// HostClient sends host→guest messages on the launch channel. It dials
// the cloud-hypervisor hybrid vsock base UDS, writes the
// "CONNECT 5000\n" preface CH expects, then exchanges one length-
// prefixed JSON request + response. Each call is one short-lived
// connection.
type HostClient struct {
	BasePath string // /run/<sid>/vsock.sock (NOT the _5000 suffixed one)
	Logf     func(string, ...any)
}

// RoundTrip performs one request/response on a fresh short connection.
// SetDeadline covers dial+CONNECT+write+read globally — we deliberately
// do not retry inside; callers (ping ticker, restore notifier) have
// their own budget logic.
//
// Wire sequence on a host→guest short conn:
//
//	host →  "CONNECT 5000\n"           (CH proxy directive)
//	host ←  "OK <localPort>\n"         (CH proxy ack — must be drained!)
//	host →  [4-byte LE length][JSON]   (proto request)
//	host ←  [4-byte LE length][JSON]   (proto response)
//
// The OK line comes from the CH proxy itself, not the guest, so it
// must be consumed before proto.ReadMessage runs — otherwise the JSON
// length prefix gets parsed as the trailing bytes of "OK <port>\n"
// (we observed payload-size=824200015 = 0x31204B4F = "OK 1" in tests
// before this drain was added).
func (c *HostClient) RoundTrip(req *proto.Message, deadline time.Duration) (*proto.Message, error) {
	return c.RoundTripContext(context.Background(), req, deadline)
}

// RoundTripContext is RoundTrip with cancellation spanning CONNECT/OK and the
// request/response exchange. It is used by the pinger so capture can cancel and
// join an admitted probe before asking the guest to quiesce.
func (c *HostClient) RoundTripContext(ctx context.Context, req *proto.Message, deadline time.Duration) (*proto.Message, error) {
	return c.roundTripContext(ctx, req, deadline, false)
}

// roundTripUntilEOFContext additionally waits for the guest side to close the
// management connection after its response. Quiesce uses this as a transport
// teardown barrier: the next operation may pause the VM, so retaining a
// half-closed vsock 4-tuple in the snapshot is unsafe.
func (c *HostClient) roundTripUntilEOFContext(ctx context.Context, req *proto.Message, deadline time.Duration) (*proto.Message, error) {
	return c.roundTripContext(ctx, req, deadline, true)
}

func (c *HostClient) roundTripContext(ctx context.Context, req *proto.Message, deadline time.Duration, waitEOF bool) (*proto.Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := c.DialRawContext(ctx, deadline)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	finish := func(err error) error {
		_ = stopCancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	if err := proto.WriteMessage(conn, req); err != nil {
		return nil, finish(fmt.Errorf("launchclient: write %s: %w", req.Type, err))
	}
	resp, err := proto.ReadMessage(conn)
	if err != nil {
		return nil, finish(fmt.Errorf("launchclient: read response: %w", err))
	}
	if waitEOF {
		var trailing [1]byte
		n, readErr := conn.Read(trailing[:])
		if n != 0 {
			return nil, finish(fmt.Errorf("launchclient: unexpected trailing byte after %s response", req.Type))
		}
		if !errors.Is(readErr, io.EOF) {
			return nil, finish(fmt.Errorf("launchclient: wait for %s connection close: %w", req.Type, readErr))
		}
	}
	if err := finish(nil); err != nil {
		return nil, err
	}
	return resp, nil
}

// DialRaw dials the CH hybrid vsock UDS, writes the "CONNECT 5000\n"
// preface, and drains CH's "OK <localPort>\n" reply, returning the live
// connection with `deadline` still set on it (covering the immediately-
// following request/response). The caller owns the conn — after its
// handshake it should clear the deadline (conn.SetDeadline(time.Time{}))
// and may keep the conn open (e.g. wrap it in a mux.Session). On any
// error the conn is closed.
func (c *HostClient) DialRaw(deadline time.Duration) (net.Conn, error) {
	return c.DialRawContext(context.Background(), deadline)
}

// DialRawContext is DialRaw with cancellation for lifecycle barriers. The
// returned connection still carries the original deadline; cancellation is
// observed only until the CONNECT/OK handshake has completed.
func (c *HostClient) DialRawContext(ctx context.Context, deadline time.Duration) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	end := time.Now().Add(deadline)
	remaining := time.Until(end)
	if remaining <= 0 {
		return nil, fmt.Errorf("launchclient: deadline already exceeded")
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, remaining)
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", c.BasePath)
	cancelDial()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("launchclient: dial %s: %w", c.BasePath, ctxErr)
		}
		return nil, fmt.Errorf("launchclient: dial %s: %w", c.BasePath, err)
	}
	if err := conn.SetDeadline(end); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("launchclient: set deadline: %w", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = conn.Close()
		return nil, ctxErr
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	fail := func(err error) (net.Conn, error) {
		_ = stopCancel()
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if _, err := conn.Write(proto.HostConnectLine); err != nil {
		return fail(fmt.Errorf("launchclient: write CONNECT: %w", err))
	}
	if err := drainLine(conn); err != nil {
		return fail(fmt.Errorf("launchclient: drain OK line: %w", err))
	}
	if !stopCancel() {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, context.Canceled
	}
	return conn, nil
}

// drainLine reads bytes from conn until '\n' or 64 bytes (CH's OK reply
// is "OK <port>\n" — well under the cap). Used to skip the CH hybrid
// vsock's CONNECT acknowledgement line.
func drainLine(conn net.Conn) error {
	one := make([]byte, 1)
	for i := 0; i < 64; i++ {
		if _, err := conn.Read(one); err != nil {
			return err
		}
		if one[0] == '\n' {
			return nil
		}
	}
	return fmt.Errorf("OK line not terminated within 64 bytes")
}

// PingStats accumulates ping ticker counters over one sandbox lifetime.
// All fields are read/written atomically so Snapshot can be called from
// any goroutine while the ticker is still firing.
type PingStats struct {
	Attempts  atomic.Uint64
	Success   atomic.Uint64
	Timeout   atomic.Uint64 // ping write/read deadline exceeded
	DialError atomic.Uint64 // CH proxy unreachable / CONNECT rejected

	// RTT histogram is kept compact: count + sum + max, plus the last
	// RTTSampleN samples retained for percentile estimation in tests.
	RTTSumNs atomic.Int64
	RTTMaxNs atomic.Int64

	mu      sync.Mutex
	samples []int64
}

const RTTSampleN = 256

// observeRTT records one successful sample.
func (p *PingStats) observeRTT(d time.Duration) {
	ns := d.Nanoseconds()
	p.RTTSumNs.Add(ns)
	for {
		old := p.RTTMaxNs.Load()
		if ns <= old {
			break
		}
		if p.RTTMaxNs.CompareAndSwap(old, ns) {
			break
		}
	}
	p.mu.Lock()
	if len(p.samples) >= RTTSampleN {
		// Reservoir-style: overwrite oldest. Simple ring without index.
		p.samples = p.samples[1:]
	}
	p.samples = append(p.samples, ns)
	p.mu.Unlock()
}

// Snapshot returns a copy safe for serialization.
type PingSnapshot struct {
	Attempts  uint64 `json:"attempts"`
	Success   uint64 `json:"success"`
	Timeout   uint64 `json:"timeout"`
	DialError uint64 `json:"dial_error"`
	RTTAvgNs  int64  `json:"rtt_avg_ns"`
	RTTMaxNs  int64  `json:"rtt_max_ns"`
	RTTP50Ns  int64  `json:"rtt_p50_ns"`
	RTTP95Ns  int64  `json:"rtt_p95_ns"`
	RTTP99Ns  int64  `json:"rtt_p99_ns"`
}

// Snapshot returns a serializable view of accumulated stats.
func (p *PingStats) Snapshot() PingSnapshot {
	att := p.Attempts.Load()
	suc := p.Success.Load()
	sum := p.RTTSumNs.Load()
	mx := p.RTTMaxNs.Load()
	var avg int64
	if suc > 0 {
		avg = sum / int64(suc)
	}

	p.mu.Lock()
	cp := append([]int64(nil), p.samples...)
	p.mu.Unlock()
	p50, p95, p99 := percentile(cp, 50), percentile(cp, 95), percentile(cp, 99)

	return PingSnapshot{
		Attempts:  att,
		Success:   suc,
		Timeout:   p.Timeout.Load(),
		DialError: p.DialError.Load(),
		RTTAvgNs:  avg,
		RTTMaxNs:  mx,
		RTTP50Ns:  p50,
		RTTP95Ns:  p95,
		RTTP99Ns:  p99,
	}
}

func percentile(s []int64, p int) int64 {
	n := len(s)
	if n == 0 {
		return 0
	}
	// Sort copy for percentile.
	cp := append([]int64(nil), s...)
	// simple insertion sort — N is bounded by RTTSampleN (256).
	for i := 1; i < len(cp); i++ {
		v := cp[i]
		j := i - 1
		for j >= 0 && cp[j] > v {
			cp[j+1] = cp[j]
			j--
		}
		cp[j+1] = v
	}
	idx := (p * (n - 1)) / 100
	return cp[idx]
}

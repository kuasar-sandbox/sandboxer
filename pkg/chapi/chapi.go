// Package chapi is the single HTTP/1.1 client over a cloud-hypervisor
// api-socket (Unix Domain Socket). It is intentionally a leaf package (only
// stdlib deps) so every caller — restore, lifecycle shutdown, snapshot — uses
// the same request/response + timeout policy.
//
// Lifecycle operations retain their existing small HTTP client. Restore
// readiness separately reads a complete framed vm.info response in ready.go.
package chapi

import (
	"context"
	"fmt"
	"net"
	"time"
)

// dialTimeout bounds the connect to the api-socket. Always applied (even when
// the response deadline is "no forced"), so a missing/dead socket fails fast
// while a slow RESPONSE can still be waited out per RespDeadline.
const dialTimeout = 5 * time.Second

// Client issues management calls against one CH api-socket. The zero value is
// usable except for Sock. RespDeadline bounds the response read; 0 = no forced
// deadline (a wedged CH then unblocks only via teardown closing the socket).
// Callers pass the resolved timeouts.ch_api as RespDeadline so every CH call
// shares one timeout policy.
type Client struct {
	Sock         string
	RespDeadline time.Duration
}

// Pause issues PUT /api/v1/vm.pause.
func (c Client) Pause() error { return c.do("PUT", "/api/v1/vm.pause", "") }

// Resume issues PUT /api/v1/vm.resume.
func (c Client) Resume() error { return c.do("PUT", "/api/v1/vm.resume", "") }

// ResumeContext is Resume with cancellation for the restore post-spawn
// barrier. Cancelling interrupts both dialing and a stalled CH response.
func (c Client) ResumeContext(ctx context.Context) error {
	return c.doContext(ctx, "PUT", "/api/v1/vm.resume", "")
}

// Snapshot issues PUT /api/v1/vm.snapshot with destination_url=destURL. CH
// writes config.json + state.json there; with our patches memory-ranges is
// skipped for fd-backed user-managed zones.
func (c Client) Snapshot(destURL string) error {
	return c.do("PUT", "/api/v1/vm.snapshot", fmt.Sprintf(`{"destination_url":"%s"}`, destURL))
}

// ShutdownVMM issues PUT /api/v1/vmm.shutdown — an ordered VMM teardown (stop
// vCPU → destroy devices → release memory zones → exit), preferred over a host
// SIGTERM whose unordered exit leaves Linux to unmap large zones via the slow
// reaper path. The 2xx is the ACK; the actual exit is awaited by the caller.
func (c Client) ShutdownVMM() error { return c.do("PUT", "/api/v1/vmm.shutdown", "") }

func (c Client) do(method, path, body string) error {
	return c.doContext(context.Background(), method, path, body)
}

func (c Client) doContext(ctx context.Context, method, path, body string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", c.Sock)
	cancelDial()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("ch api dial %s: %w", c.Sock, ctxErr)
		}
		return fmt.Errorf("ch api dial %s: %w", c.Sock, err)
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopCancel()
	if c.RespDeadline > 0 {
		_ = conn.SetDeadline(time.Now().Add(c.RespDeadline))
	}

	req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: ch\r\n", method, path)
	if body != "" {
		req += fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	req += "Connection: close\r\n\r\n" + body
	if _, err := conn.Write([]byte(req)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("ch api %s %s write: %w", method, path, ctxErr)
		}
		return fmt.Errorf("ch api %s %s write: %w", method, path, err)
	}
	buf := make([]byte, 4096)
	n, readErr := conn.Read(buf)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("ch api %s %s read: %w", method, path, ctxErr)
	}
	if readErr != nil && n == 0 {
		return fmt.Errorf("ch api %s %s read: %w", method, path, readErr)
	}
	resp := string(buf[:n])
	if len(resp) < 12 {
		return fmt.Errorf("ch api %s %s short response: %q", method, path, resp)
	}
	if resp[9] != '2' { // "HTTP/1.1 2xx" — first status digit
		return fmt.Errorf("ch api %s %s non-2xx: %q", method, path, resp)
	}
	return nil
}

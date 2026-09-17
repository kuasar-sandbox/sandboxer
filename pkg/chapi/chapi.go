// Package chapi is the single HTTP/1.1 client over a cloud-hypervisor
// api-socket (Unix Domain Socket). It is intentionally a leaf package (only
// stdlib deps) so every caller — restore, lifecycle shutdown, snapshot — uses
// the same request/response + timeout policy.
//
// CH speaks plain HTTP/1.1 on the UDS exposed by --api-socket. The call rate is
// low (pause / resume / snapshot / shutdown), so a hand-rolled request keeps the
// dependency surface minimal compared to net/http.
package chapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// dialTimeout bounds the connect to the api-socket. Always applied (even when
// the response deadline is "no forced"), so a missing/dead socket fails fast
// while a slow RESPONSE can still be waited out per RespDeadline.
const dialTimeout = 5 * time.Second

const (
	waitReadyDialTimeout = 50 * time.Millisecond
	waitReadyInitialPoll = 1 * time.Millisecond
	waitReadyMaxPoll     = 20 * time.Millisecond
	maxReadyResponseBody = 1 << 20
)

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

// WaitReady polls vm.info until CH reports the restored VM in Paused state.
// Merely connecting to the api-socket is insufficient: CH v51.1 starts its
// HTTP server before its CLI has submitted VmRestore. deadline <= 0 polls until
// ctx is cancelled; a positive deadline covers dial, request, and the complete
// response read. Probes are sequential, so a slow response cannot accumulate
// requests in CH's single API event loop.
func WaitReady(ctx context.Context, sock string, deadline time.Duration) error {
	var end time.Time
	if deadline > 0 {
		end = time.Now().Add(deadline)
	}
	poll := waitReadyInitialPoll
	var lastErr error
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("ch api socket not ready: %w", ctx.Err())
		}
		if !end.IsZero() && !time.Now().Before(end) {
			if lastErr != nil {
				return fmt.Errorf("ch api socket not ready before deadline: %w", lastErr)
			}
			return fmt.Errorf("ch api socket not ready before deadline")
		}

		dialCtx := ctx
		cancel := func() {}
		timeout := waitReadyDialTimeout
		if !end.IsZero() {
			remaining := time.Until(end)
			if remaining <= 0 {
				continue
			}
			if remaining < timeout {
				timeout = remaining
			}
		}
		if timeout > 0 {
			dialCtx, cancel = context.WithTimeout(ctx, timeout)
		}

		c, err := (&net.Dialer{}).DialContext(dialCtx, "unix", sock)
		cancel()
		if err == nil {
			ready, pending, probeErr := probePaused(ctx, c, end)
			_ = c.Close()
			if ready {
				return nil
			}
			if !pending {
				if !end.IsZero() && !time.Now().Before(end) {
					return fmt.Errorf("ch api VM not ready before deadline: %w", probeErr)
				}
				return fmt.Errorf("ch api VM not ready: %w", probeErr)
			}
			lastErr = probeErr
		} else if errorsIsSocketPending(err) {
			lastErr = err
		} else {
			return fmt.Errorf("ch api socket: %w", err)
		}

		sleep := poll
		if !end.IsZero() {
			remaining := time.Until(end)
			if remaining <= 0 {
				continue
			}
			if remaining < sleep {
				sleep = remaining
			}
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return fmt.Errorf("ch api socket not ready: %w", ctx.Err())
		case <-timer.C:
		}
		if poll < waitReadyMaxPoll {
			poll *= 2
			if poll > waitReadyMaxPoll {
				poll = waitReadyMaxPoll
			}
		}
	}
}

func errorsIsSocketPending(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func probePaused(ctx context.Context, conn net.Conn, end time.Time) (ready, pending bool, err error) {
	// Install the deadline before cancellation so it cannot overwrite cancellation.
	if !end.IsZero() {
		if err := conn.SetDeadline(end); err != nil {
			return false, false, err
		}
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if _, err := io.WriteString(conn, "GET /api/v1/vm.info HTTP/1.1\r\nHost: ch\r\nConnection: close\r\n\r\n"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, false, ctxErr
		}
		return false, false, fmt.Errorf("vm.info write: %w", err)
	}
	// Bound the entire wire response, including headers and chunk framing. A body
	// close must not drain an unbounded response after reaching the size limit.
	wire := &io.LimitedReader{R: conn, N: maxReadyResponseBody + 1}
	resp, err := http.ReadResponse(bufio.NewReader(wire), &http.Request{Method: http.MethodGet})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, false, ctxErr
		}
		return false, false, fmt.Errorf("vm.info response: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReadyResponseBody+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, false, ctxErr
		}
		return false, false, fmt.Errorf("vm.info body: %w", err)
	}
	if wire.N == 0 || len(body) > maxReadyResponseBody {
		return false, false, fmt.Errorf("vm.info response exceeds %d bytes", maxReadyResponseBody)
	}
	if resp.StatusCode == http.StatusInternalServerError && isVMNotCreated(body) {
		return false, true, fmt.Errorf("VM is not created")
	}
	if resp.StatusCode != http.StatusOK {
		return false, false, fmt.Errorf("vm.info HTTP %d: %q", resp.StatusCode, body)
	}
	var info struct {
		State string `json:"state"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&info); err != nil {
		return false, false, fmt.Errorf("vm.info JSON: %w", err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return false, false, fmt.Errorf("vm.info JSON: %w", err)
	}
	if info.State != "Paused" {
		return false, false, fmt.Errorf("vm.info unexpected state %q", info.State)
	}
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	return true, false, nil
}

func isVMNotCreated(body []byte) bool {
	var messages []string
	if err := json.Unmarshal(body, &messages); err != nil {
		return false
	}
	want := []string{"Error from API", "The VM info is not available", "VM is not created"}
	if len(messages) != len(want) {
		return false
	}
	for i := range want {
		if messages[i] != want[i] {
			return false
		}
	}
	return true
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

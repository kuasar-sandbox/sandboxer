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
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	conn, err := net.DialTimeout("unix", c.Sock, dialTimeout)
	if err != nil {
		return fmt.Errorf("ch api dial %s: %w", c.Sock, err)
	}
	defer conn.Close()
	if c.RespDeadline > 0 {
		_ = conn.SetDeadline(time.Now().Add(c.RespDeadline))
	}

	req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: ch\r\n", method, path)
	if body != "" {
		req += fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	req += "Connection: close\r\n\r\n" + body
	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("ch api %s %s write: %w", method, path, err)
	}
	buf := make([]byte, 4096)
	n, _ := conn.Read(buf)
	resp := string(buf[:n])
	if len(resp) < 12 {
		return fmt.Errorf("ch api %s %s short response: %q", method, path, resp)
	}
	if resp[9] != '2' { // "HTTP/1.1 2xx" — first status digit
		return fmt.Errorf("ch api %s %s non-2xx: %q", method, path, resp)
	}
	return nil
}

// WaitRestored reads CH's --event-monitor event stream until it sees the
// vm/restored event that Vm::restore emits once device setup and restored
// vCPU start are complete. CH's monitor thread writes each event as a JSON
// object (whitespace-separated); this decodes them structurally rather than
// scanning the serialization, so it is immune to pretty- vs compact-print
// changes. Only Vm::restore emits an event named "restored" (source "vm").
//
// r is the read-end of the fd passed to CH as --event-monitor fd=<n>. The
// decode goroutine blocks on r; on timeout or ctx cancellation WaitRestored
// closes r (the EOF unblocks the goroutine) and drains it, so no blocked
// reader is ever leaked. EOF before the event is an error. This is the
// restore+resume completion barrier for the resume=true path: the old external
// /vm.resume gave it implicitly (it serialized behind VmRestore in CH's api
// loop), but resume=true drops that call.
func WaitRestored(ctx context.Context, r io.ReadCloser, deadline time.Duration) error {
	done := make(chan error, 1)
	go func() {
		dec := json.NewDecoder(r)
		for {
			var ev struct {
				Source string `json:"source"`
				Event  string `json:"event"`
			}
			if err := dec.Decode(&ev); err != nil {
				done <- fmt.Errorf("vm/restored not seen before stream end: %w", err)
				return
			}
			if ev.Source == "vm" && ev.Event == "restored" {
				done <- nil
				return
			}
		}
	}()
	var tc <-chan time.Time
	if deadline > 0 {
		t := time.NewTimer(deadline)
		defer t.Stop()
		tc = t.C
	}
	shutdown := func(err error) error {
		_ = r.Close() // EOF unblocks the decode goroutine…
		<-done        // …then reap it before returning
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return shutdown(fmt.Errorf("vm/restored: %w", ctx.Err()))
	case <-tc:
		return shutdown(fmt.Errorf("vm/restored not seen before deadline"))
	}
}

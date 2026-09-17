package chapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

const readyResponseLimit = 1 << 20

// WaitReady waits for a restored, Paused VM, not merely a listening API socket.
// Its deadline covers connection setup, framed response reads and polling.
// A nonpositive deadline leaves cancellation to ctx. The caller retains the
// separate vm.resume and post-resume guest-acknowledgement deadlines.
func WaitReady(ctx context.Context, sock string, deadline time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives:      true,
		DisableCompression:    true,
		MaxResponseHeaderBytes: 64 << 10,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("unexpected readiness redirect")
		},
	}
	poll := time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("ch restored VM not ready: %w", err)
		}
		ready, err := probePaused(ctx, client)
		if e := ctx.Err(); e != nil {
			return fmt.Errorf("ch restored VM not ready: %w", e)
		}
		if err != nil {
			return fmt.Errorf("ch restored VM readiness: %w", err)
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("ch restored VM not ready: %w", ctx.Err())
		case <-timer.C:
		}
		if poll < 20*time.Millisecond {
			poll *= 2
			if poll > 20*time.Millisecond {
				poll = 20 * time.Millisecond
			}
		}
	}
}

func probePaused(ctx context.Context, client *http.Client) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cloud-hypervisor/api/v1/vm.info", nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// Only normal pre-listen states are pending. Permission errors, resets,
		// malformed HTTP and unrelated failures are not startup success.
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return false, nil
		}
		return false, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > readyResponseLimit {
		return false, errors.New("vm.info response exceeds size limit")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, readyResponseLimit+1))
	if err != nil {
		return false, fmt.Errorf("read vm.info: %w", err)
	}
	if len(body) > readyResponseLimit {
		return false, errors.New("vm.info response exceeds size limit")
	}
	if resp.StatusCode == http.StatusInternalServerError {
		// CH v51.1's complete structured pre-create response. Never retry an
		// arbitrary HTTP 500 or match only a substring of the error chain.
		var chain []string
		if json.Unmarshal(body, &chain) == nil && len(chain) == 3 &&
			chain[0] == "Error from API" && chain[1] == "The VM info is not available" && chain[2] == "VM is not created" {
			return false, nil
		}
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("vm.info HTTP status %d", resp.StatusCode)
	}
	var info struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return false, fmt.Errorf("decode vm.info: %w", err)
	}
	if info.State != "Paused" {
		return false, fmt.Errorf("vm.info state %q, expected Paused after restore", info.State)
	}
	return true, nil
}

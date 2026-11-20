package resctl

import (
	"context"
	"fmt"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

// Static SDKs have no node memory ledger. Dynamic clients require a positive
// capability echo before changing the existing synchronous cleanup behavior.
func (h *ControllerHooks) SupportsRetainedMemory() bool {
	if h == nil || !h.Enabled() {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.retainedSupported
}

func (h *ControllerHooks) retentionCallLocked(ctx context.Context, typ string, capture resource.CaptureRetention) (*resource.RetainedMemory, error) {
	if !h.Enabled() {
		return &resource.RetainedMemory{MemoryID: capture.MemoryID, Epoch: capture.Epoch}, nil
	}
	if !h.client.Connected() {
		if err := h.client.ConnectContext(ctx); err != nil {
			return nil, err
		}
	}
	r, err := h.client.RetainedMemoryContext(ctx, typ, h.opts.SandboxID, capture)
	if resource.IsTransportError(err) {
		// Retention operations are exactly replayable and do not reconstruct
		// a retired active Budget through the ordinary StateSync path.
		if reconnectErr := h.client.ConnectContext(ctx); reconnectErr == nil {
			r, err = h.client.RetainedMemoryContext(ctx, typ, h.opts.SandboxID, capture)
		}
	}
	return r, err
}

// PrepareRetained protects the current B before capture commit. A transport
// error is an unknown result: Release must still preserve the lease and B until
// an explicit Abort against the uncommitted capture record succeeds.
func (h *ControllerHooks) PrepareRetained(ctx context.Context, capture resource.CaptureRetention) (resource.RetainedMemory, error) {
	if err := capture.Validate(); err != nil {
		return resource.RetainedMemory{}, err
	}
	if h == nil {
		return resource.RetainedMemory{MemoryID: capture.MemoryID, Epoch: capture.Epoch}, nil
	}
	if !h.SupportsRetainedMemory() {
		return resource.RetainedMemory{}, fmt.Errorf("controller does not support checkpoint memory handoff")
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	h.mu.Lock()
	previous := h.retention
	h.mu.Unlock()
	if previous != nil && *previous != capture {
		// Data ready may precede its node ACK. Settle the one previous R
		// before creating another capture; never accumulate generations.
		if _, err := h.retentionCallLocked(ctx, resource.TypeReleaseRetained, *previous); err != nil {
			// A prior preflight may have durably aborted while its resource
			// ACK was lost. The exact Abort is legal only for that still
			// uncommitted record and leaves its original active B intact.
			if _, abortErr := h.retentionCallLocked(ctx, resource.TypeAbortRetained, *previous); abortErr != nil {
				return resource.RetainedMemory{}, err
			}
		}
	}
	h.mu.Lock()
	copy := capture
	h.retention, h.retentionPrepared = &copy, true
	h.mu.Unlock()
	r, err := h.retentionCallLocked(ctx, resource.TypePrepareRetained, capture)
	if err != nil {
		return resource.RetainedMemory{}, err
	}
	if r == nil {
		return resource.RetainedMemory{}, fmt.Errorf("controller omitted retained memory identity")
	}
	return *r, nil
}

func (h *ControllerHooks) CommitRetained(ctx context.Context, capture resource.CaptureRetention) error {
	if h == nil {
		return nil
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	if _, err := h.retentionCallLocked(ctx, resource.TypeCommitRetained, capture); err != nil {
		return err
	}
	h.mu.Lock()
	copy := capture
	h.retention = &copy
	h.retentionPrepared, h.retentionHolder = false, true
	h.admitted, h.connected = false, false
	h.reservationBytes, h.cacheReservation = 0, 0 // bookkeeping only, never a local control target
	h.mu.Unlock()
	return nil
}

func (h *ControllerHooks) AbortRetained(ctx context.Context, capture resource.CaptureRetention) error {
	if h == nil {
		return nil
	}
	h.sessionMu.Lock()
	if _, err := h.retentionCallLocked(ctx, resource.TypeAbortRetained, capture); err != nil {
		h.mu.Lock()
		released := h.released
		h.mu.Unlock()
		if released && h.client != nil {
			_ = h.client.Close()
		}
		h.sessionMu.Unlock()
		h.retryAbortRetained(capture)
		return err
	}
	h.mu.Lock()
	matched := false
	if h.retention != nil && *h.retention == capture {
		h.retention, h.retentionPrepared = nil, false
		matched = true
	}
	released, releaseDone := h.released, h.releaseDone
	h.mu.Unlock()
	if released && h.client != nil {
		// A late idempotent replay may reconnect after ordinary Release. It
		// must not leave a new idle control connection owned by a closed run.
		_ = h.client.Close()
	}
	h.sessionMu.Unlock()
	if matched && released {
		// Release may still be draining StateSync. Never release its B/lease
		// before that barrier, and never wait while holding sessionMu.
		<-releaseDone
		h.sessionMu.Lock()
		h.mu.Lock()
		ordinary := h.retention == nil && !h.retentionPrepared && !h.retentionHolder
		h.mu.Unlock()
		if ordinary {
			h.releaseOrdinaryLocked("checkpoint preparation aborted after runtime release")
		}
		h.sessionMu.Unlock()
	}
	return nil
}

// A failed Abort is an owned metadata/accounting responsibility even after
// Runtime.Close. This continuation retains no memfd, RAM pin or runtime owner.
func (h *ControllerHooks) retryAbortRetained(capture resource.CaptureRetention) {
	h.mu.Lock()
	if h.retention == nil || *h.retention != capture || !h.retentionPrepared {
		h.mu.Unlock()
		return
	}
	if h.abortRetries == nil {
		h.abortRetries = make(map[resource.CaptureRetention]bool)
	}
	if h.abortRetries[capture] {
		h.mu.Unlock()
		return
	}
	h.abortRetries[capture] = true
	h.mu.Unlock()
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.abortRetries, capture)
			h.mu.Unlock()
		}()
		for {
			time.Sleep(time.Second)
			h.mu.Lock()
			pending := h.retention != nil && *h.retention == capture && h.retentionPrepared
			h.mu.Unlock()
			if !pending {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := h.AbortRetained(ctx, capture)
			cancel()
			if err == nil {
				return
			}
			h.opts.Logf("checkpoint resource rollback retained for retry: %v", err)
		}
	}()
}

func (h *ControllerHooks) ReleaseRetained(ctx context.Context, capture resource.CaptureRetention) error {
	if h == nil {
		return nil
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	if _, err := h.retentionCallLocked(ctx, resource.TypeReleaseRetained, capture); err != nil {
		return err
	}
	h.mu.Lock()
	if h.retention != nil && *h.retention == capture {
		h.retention = nil
	}
	h.mu.Unlock()
	return nil
}

// CloseRetained drops the holder's original lease after its RAM responsibility
// ends. A replaced canonical inode belongs to the successor and is preserved.
// Accounting release requires ReleaseRetained or inventory's positive proof;
// closing a transport or lease alone never clears R.
func (h *ControllerHooks) CloseRetained() error {
	if h == nil {
		return nil
	}
	h.Release("checkpoint memory holder closed")
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	if h.client != nil {
		_ = h.client.Close()
	}
	if h.lease != nil {
		return h.lease.Close()
	}
	return nil
}

package resctl

import (
	"errors"
	"fmt"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

// CacheSupported is negotiated during Admit. Older nodes preserve the
// original fixed-size cache behavior.
func (h *ControllerHooks) CacheSupported() bool {
	if h == nil || !h.Enabled() {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cacheSupported
}

func (h *ControllerHooks) CacheWake() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.cacheWake
}

func (h *ControllerHooks) CacheTarget() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cacheReclaimTarget
}

func (h *ControllerHooks) CacheRequest() (uint64, uint64) {
	if h == nil {
		return 0, 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cacheReclaimTarget, h.cacheReclaimSeq
}

func (h *ControllerHooks) setCacheTarget(target uint64, seq ...uint64) {
	generation := uint64(0)
	if len(seq) > 0 {
		generation = seq[0]
	}
	h.mu.Lock()
	if target != h.cacheReclaimTarget || generation != h.cacheReclaimSeq {
		h.cacheReclaimTarget = target
		h.cacheReclaimSeq = generation
		select {
		case h.cacheWake <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *ControllerHooks) CacheReservation() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cacheReservation
}

// RequestCacheBudget serializes with Guest budget and StateSync RPCs. For an
// ambiguous grow, only already confirmed bytes remain available locally. For
// ambiguous shrink, the caller has already discarded those host pages, so the
// smaller baseline remains safe at reconnect.
func (h *ControllerHooks) RequestCacheBudget(current, requested uint64) (uint64, uint64, uint64, time.Duration, error) {
	return h.requestCacheBudget(current, requested, 0)
}

func (h *ControllerHooks) requestCacheBudget(current, requested, doneSeq uint64) (uint64, uint64, uint64, time.Duration, error) {
	if h == nil || !h.Enabled() || !h.CacheSupported() {
		return 0, 0, 0, 0, errors.New("elastic cache budget unsupported")
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	state := h.localState()
	if state.released || !state.admitted || !state.cacheSupported {
		return 0, state.cacheReservation, state.cacheTarget, 0, errors.New("cache budget session unavailable")
	}
	if current > state.cacheReservation || current > state.cacheMaximum ||
		requested > state.cacheMaximum-current {
		return 0, state.cacheReservation, state.cacheTarget, 0, fmt.Errorf("invalid cache request baseline")
	}
	// The caller has already released physical pages when it reports a
	// smaller absolute baseline. Remember this even if the transport is
	// currently disconnected, so StateSync does not resurrect stale charge.
	// Increases are still accepted only after a successful node reply.
	if current < state.cacheReservation {
		h.mu.Lock()
		h.cacheReservation = current
		h.mu.Unlock()
	}
	if !state.connected {
		h.notifyReconnect()
		return 0, current, state.cacheTarget, 0,
			&resource.TransportError{Err: errors.New("cache budget controller disconnected")}
	}
	result, err := h.client.RequestCacheBudgetWithAckContext(h.lifetimeCtx, current, requested, state.cacheMaximum, doneSeq)
	if err != nil {
		h.markDisconnectedLocked(err)
		return 0, current, state.cacheTarget, 0, err
	}
	expected := current + result.Granted
	if expected < current || result.Granted > requested ||
		result.Reservation != expected || result.Reservation > state.cacheMaximum {
		err := fmt.Errorf("invalid cache grant response")
		h.markDisconnectedLocked(err)
		return 0, current, state.cacheTarget, 0, err
	}
	h.mu.Lock()
	h.cacheReservation = result.Reservation
	h.mu.Unlock()
	h.setCacheTarget(result.ReclaimTarget, result.ReclaimSeq)
	return result.Granted, result.Reservation, result.ReclaimTarget,
		time.Duration(result.CooldownMs) * time.Millisecond, nil
}

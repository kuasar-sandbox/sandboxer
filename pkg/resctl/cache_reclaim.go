package resctl

import (
	"errors"
	"fmt"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func (h *ControllerHooks) CacheCoordinated() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cacheCoordinated
}

func (h *ControllerHooks) cacheReclaimState() *resource.CacheReclaimState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return resource.CloneCacheReclaimState(&h.cacheReclaim)
}

// confirmCacheReclaimCurrent is called by the cache worker only after physical
// allocation or safe rollback finishes, never just because a grant arrived.
func (h *ControllerHooks) confirmCacheReclaimCurrent(current uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cacheCoordinated && h.cacheReclaim.Task == nil && h.cacheReclaim.Done == nil && current <= h.cacheReservation {
		h.cacheReclaim.Current = current
	}
}

// An unacknowledged completion is an immutable outbox, including its next
// intent. Keep it until the node explicitly acknowledges the completed Seq.
func (h *ControllerHooks) setCacheReclaimIntent(intent *resource.CacheReclaimIntent, lastSeq uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cacheReclaim.Done != nil || h.cacheReclaim.Task != nil || h.cacheReclaim.LastSeq != lastSeq {
		return false
	}
	h.cacheReclaim.Intent = resource.CloneCacheReclaimIntent(intent)
	return true
}

func (h *ControllerHooks) acceptCacheReclaimResponse(task *resource.CacheReclaimTask, ack uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	state := &h.cacheReclaim
	changed := false
	if ack > state.LastSeq {
		return fmt.Errorf("cache reclaim ACK %d exceeds accepted sequence %d", ack, state.LastSeq)
	}
	if ack > h.cacheReclaimAck && (state.Done == nil || ack != state.Done.Task.Seq) {
		return fmt.Errorf("cache reclaim ACK has no matching completion")
	}
	if task != nil {
		if task.RunID != state.RunID || task.Seq == 0 || task.MaxBytes == 0 ||
			task.MaxBytes%4096 != 0 || task.MaxBytes > cacheMaxBatch || task.MaxBytes > h.cacheMaximum ||
			task.Target == 0 || task.Target%4096 != 0 || task.Target > h.cacheMaximum || task.MaxBytes > h.cacheMaximum-task.Target ||
			(task.Source != resource.CacheReclaimAutonomous && task.Source != resource.CacheReclaimYield) ||
			(task.Source == resource.CacheReclaimAutonomous && task.IntentID == 0) ||
			(task.Source == resource.CacheReclaimYield && task.IntentID != 0) {
			return fmt.Errorf("invalid cache reclaim task")
		}
		if task.Seq > state.LastSeq && task.Target+task.MaxBytes != state.Current {
			return fmt.Errorf("cache reclaim task differs from confirmed physical capacity")
		}
		if state.Task != nil && task.Seq == state.Task.Seq && *task != *state.Task {
			return fmt.Errorf("cache reclaim task changed after acceptance")
		}
		if state.Done != nil && task.Seq == state.Done.Task.Seq && *task != state.Done.Task {
			return fmt.Errorf("completed cache reclaim task changed")
		}
		if task.Seq > state.LastSeq && (state.Task != nil || state.Done != nil && ack < state.Done.Task.Seq) {
			return fmt.Errorf("cache reclaim task overlaps unresolved execution")
		}
		if task.Seq > state.LastSeq && state.Current != h.cacheReservation {
			return fmt.Errorf("cache reclaim task issued before physical growth confirmation")
		}
	}
	if state.Done != nil && ack >= state.Done.Task.Seq {
		state.Done = nil
		changed = true
	}
	h.cacheReclaimAck = max(h.cacheReclaimAck, ack)
	if task != nil && task.Seq > state.LastSeq {
		copyTask := *task
		state.Task = &copyTask
		state.LastSeq = task.Seq
		changed = true
		if intent := state.Intent; intent != nil && task.IntentID == intent.ID {
			if task.MaxBytes >= intent.Bytes {
				state.Intent = nil
			} else {
				intent.Bytes -= task.MaxBytes
			}
		}
	}
	if changed {
		select {
		case h.cacheWake <- struct{}{}:
		default:
		}
	}
	return nil
}

// completeCacheReclaim records a safe physical return before any network call.
// StateSync can therefore never resurrect charge after a disconnected return.
func (h *ControllerHooks) completeCacheReclaim(task resource.CacheReclaimTask, current uint64, intent *resource.CacheReclaimIntent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	state := &h.cacheReclaim
	if state.Task == nil || *state.Task != task || state.Done != nil || current > h.cacheReservation ||
		current > state.Current || state.Current-current > task.MaxBytes {
		return fmt.Errorf("cache reclaim completion does not match accepted task")
	}
	state.Task = nil
	state.Intent = resource.CloneCacheReclaimIntent(intent)
	state.Done = &resource.CacheReclaimDone{Task: task, Current: current, Intent: resource.CloneCacheReclaimIntent(intent)}
	state.Current = current
	h.cacheReservation = current
	return nil
}

// syncCacheReclaim is separate from cache-growth requests: reporting intent or
// completing a task must not withdraw a pending growth demand on the node.
func (h *ControllerHooks) syncCacheReclaim() error {
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	state := h.localState()
	if state.released || !state.admitted || !state.cacheCoordinated {
		return errors.New("cache coordination session unavailable")
	}
	if !state.connected {
		h.notifyReconnect()
		return &resource.TransportError{Err: errors.New("cache controller disconnected")}
	}
	result, err := h.client.UpdateCacheReclaimContext(h.lifetimeCtx, *state.cacheReclaim)
	if err == nil && result.Reservation != state.cacheReservation {
		err = fmt.Errorf("cache reclaim reservation mismatch node=%d local=%d", result.Reservation, state.cacheReservation)
	}
	if err == nil {
		err = h.acceptCacheReclaimResponse(result.Task, result.Ack)
	}
	if err != nil {
		h.markDisconnectedLocked(err)
	}
	return err
}

package sandbox

import (
	"errors"
	"fmt"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// An environment report describes the actual namespace PID 1 Wait. It never
// changes the primary's initial-launch fact or invents an app exit status.
func (r *Runtime) recordEnvironmentExit(fact proto.EnvironmentExit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.environmentActive {
		return errors.New("unexpected environment exit report for direct supervision")
	}
	if r.state == runtimeClosed {
		return errors.New("environment exit report arrived after retirement")
	}
	if previous := r.environmentExit; previous != nil {
		if *previous == fact {
			return nil
		}
		r.environmentProtocolError = errors.New("conflicting Guest environment exit reports")
		return r.environmentProtocolError
	}
	copy := fact
	r.environmentExit = &copy
	return nil
}

// Called with mu held after the real VMM reaper and resource cleanup. Missing
// reports are an observation error even when Host requested normal shutdown:
// knowing stop intent cannot prove an unreported manager result.
func (r *Runtime) environmentExitErrorLocked() error {
	if !r.environmentActive {
		return nil
	}
	var result error
	if fact := r.environmentExit; fact != nil {
		if fact.State == proto.EnvironmentFailed {
			result = fmt.Errorf("Guest environment failed: %s (PID 1 code=%d signal=%d)", fact.Reason, fact.Code, fact.TermSignal)
		}
	} else if !r.physicalRetirement {
		result = errors.New("Guest systemd environment exited without a terminal environment report")
	}
	return errors.Join(result, r.environmentProtocolError)
}

// Explicit physical retirement has no Guest execution phase (in particular a
// committed frozen capture must stay frozen). It only waives a missing report;
// a failure already reported by the Guest always remains a failure.
func (r *Runtime) markPhysicalRetirement() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == runtimeClosed {
		return
	}
	select {
	case <-r.reaped:
		return
	default:
	}
	r.physicalRetirement = true
}

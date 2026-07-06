package main

import (
	"syscall"
	"time"
)

// Shared restart machinery for the user app (launch.restart) and companion
// plugins (launch.plugin[]). Both use the same exponential backoff and the
// same restart-policy decision (docs/sandbox-init.md §3.2).

const (
	backoffMin   = 10 * time.Millisecond
	backoffMax   = 60 * time.Second
	backoffReset = 60 * time.Second // uptime ≥ this on exit ⇒ reset the delay
)

// backoff produces restart delays: starts at backoffMin, doubles up to
// backoffMax, and resets to backoffMin when the process stayed up at least
// backoffReset before exiting ("启动后不退出则 60s 后重置退避计时"). One per
// supervised process; mutated only by that process's restart path.
type backoff struct {
	delay     time.Duration
	startedAt time.Time
}

// onStart records a (re)launch instant; next() measures uptime against it.
func (b *backoff) onStart(now time.Time) { b.startedAt = now }

// next returns the delay to wait before the next relaunch, given the exit
// instant, advancing (or resetting) the backoff state.
func (b *backoff) next(exitedAt time.Time) time.Duration {
	if !b.startedAt.IsZero() && exitedAt.Sub(b.startedAt) >= backoffReset {
		b.delay = backoffMin
		return b.delay
	}
	if b.delay <= 0 {
		b.delay = backoffMin
	} else {
		b.delay *= 2
		if b.delay > backoffMax {
			b.delay = backoffMax
		}
	}
	return b.delay
}

// wantRestart decides whether a process should be restarted given its policy
// and exit status:
//
//	always     → yes
//	on-failure → yes iff it exited non-zero or was signalled
//	never / "" → no
func wantRestart(policy string, status syscall.WaitStatus) bool {
	switch policy {
	case "always":
		return true
	case "on-failure":
		return status.Signaled() || status.ExitStatus() != 0
	default: // "never" or unset
		return false
	}
}

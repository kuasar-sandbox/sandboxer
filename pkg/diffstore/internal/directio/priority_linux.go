package directio

import (
	"log/slog"
	"runtime"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

const (
	ioPriorityWhoProcess = 1
	ioPriorityClassShift = 13
	ioPriorityClassMask  = 7
	ioPriorityLevelMask  = 7
	ioPriorityBE0        = 2 << ioPriorityClassShift
	schedResetOnFork     = 0x40000000
)

// prioritySyscalls is a per-call test seam. Production calls never replace
// process-wide syscall functions or change another thread's I/O priority.
type prioritySyscalls struct {
	get    func() (int, error)
	set    func(int) error
	policy func() (int, error)
}

func getThreadPriority() (int, error) {
	value, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, ioPriorityWhoProcess, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(value), nil
}

func setThreadPriority(value int) error {
	_, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioPriorityWhoProcess, 0, uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}

func getThreadPolicy() (int, error) {
	value, _, errno := unix.Syscall(unix.SYS_SCHED_GETSCHEDULER, 0, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(value), nil
}

func nativePrioritySyscalls() prioritySyscalls {
	return prioritySyscalls{getThreadPriority, setThreadPriority, getThreadPolicy}
}

type priorityStage uint8

const (
	priorityCallerGet priorityStage = iota
	priorityCallerPolicy
	priorityWorkerGet
	priorityWorkerPolicy
	prioritySet
	priorityRestore
	priorityStageCount
)

func (stage priorityStage) String() string {
	return [...]string{"caller_get", "caller_policy", "worker_get", "worker_policy", "set", "restore"}[stage]
}

type priorityDiagnostic struct {
	stage priorityStage
	err   error
}

// One warning per stage bounds diagnostics even when every I/O is denied by a
// seccomp/LSM policy. CAS avoids making other callers wait for a logger.
type priorityWarnings struct {
	seen [priorityStageCount]atomic.Bool
}

func (warnings *priorityWarnings) first(stage priorityStage) bool {
	return warnings.seen[stage].CompareAndSwap(false, true)
}

var ioPriorityWarnings priorityWarnings

func reportPriorityDiagnostic(diagnostic priorityDiagnostic) {
	if ioPriorityWarnings.first(diagnostic.stage) {
		slog.Warn("diffstore: I/O priority unavailable; preserving the I/O result",
			"stage", diagnostic.stage.String(), "err", diagnostic.err,
			"thread_retired", diagnostic.stage == priorityRestore)
	}
}

// priorityAtLeastBE0 preserves stronger inherited settings. NONE follows the
// CPU scheduler: FIFO, RR and DEADLINE imply an RT I/O class. Moving such an
// operation to a normal runtime thread would lower its effective priority and
// reproducing RT there could require capabilities the process no longer has.
func priorityAtLeastBE0(raw int, policy func() (int, error)) (bool, error) {
	switch raw >> ioPriorityClassShift & ioPriorityClassMask {
	case 1: // RT
		return true, nil
	case 2: // BE
		return raw&ioPriorityLevelMask == 0, nil
	case 0: // NONE
		value, err := policy()
		if err != nil {
			return false, err
		}
		value &^= schedResetOnFork
		return value == unix.SCHED_FIFO || value == unix.SCHED_RR || value == unix.SCHED_DEADLINE, nil
	default:
		return false, nil
	}
}

type priorityResult struct {
	n          int
	err        error
	diagnostic *priorityDiagnostic
	panicked   bool
	panicValue any
}

func withIOPriority(op func() (int, error)) (int, error) {
	return withPrioritySyscalls(op, nativePrioritySyscalls(), reportPriorityDiagnostic)
}

// withPrioritySyscalls is best effort: priority errors are diagnostics, never
// I/O errors. It preserves the actual transfer count, error and retry policy.
// op is a synchronous internal syscall operation, not a user callback; it must
// not call runtime.Goexit. The caller always waits until op has relinquished
// its buffers, including after cancellation elsewhere in the owning runtime.
func withPrioritySyscalls(op func() (int, error), calls prioritySyscalls, report func(priorityDiagnostic)) (int, error) {
	runtime.LockOSThread()
	original, err := calls.get()
	if err != nil {
		defer runtime.UnlockOSThread()
		if report != nil {
			report(priorityDiagnostic{priorityCallerGet, err})
		}
		return op()
	}
	highEnough, err := priorityAtLeastBE0(original, calls.policy)
	if err != nil || highEnough {
		defer runtime.UnlockOSThread()
		if err != nil && report != nil {
			report(priorityDiagnostic{priorityCallerPolicy, err})
		}
		return op()
	}
	runtime.UnlockOSThread()

	// The helper owns the complete lifetime of this goroutine. Restoring a
	// reused Go thread can fail, and an arbitrary caller (export/main included)
	// cannot be required to exit or to preserve this helper's thread lock.
	done := make(chan priorityResult)
	go func() {
		runtime.LockOSThread()
		var result priorityResult
		var saved int
		changed, returned := false, false
		defer func() {
			if !returned {
				result.panicked = true
				result.panicValue = recover()
			}
			retire := false
			if changed {
				if err := calls.set(saved); err != nil {
					retire = true
					result.diagnostic = &priorityDiagnostic{priorityRestore, err}
				}
			}
			if !retire {
				runtime.UnlockOSThread()
			}
			// With the lock retained, returning retires this thread from Go's
			// scheduler. Go exits ordinary threads but permanently parks its
			// initial thread. Neither can run another goroutine. The caller's
			// own defers run on its goroutine and cannot unlock this thread.
			done <- result
		}()

		var setupErr error
		saved, setupErr = calls.get()
		if setupErr != nil {
			result.diagnostic = &priorityDiagnostic{priorityWorkerGet, setupErr}
		} else {
			highEnough, err := priorityAtLeastBE0(saved, calls.policy)
			if err != nil {
				result.diagnostic = &priorityDiagnostic{priorityWorkerPolicy, err}
			} else if !highEnough {
				if err := calls.set(ioPriorityBE0); err != nil {
					result.diagnostic = &priorityDiagnostic{prioritySet, err}
				} else {
					changed = true
				}
			}
		}
		result.n, result.err = op()
		returned = true
	}()
	result := <-done
	if result.diagnostic != nil && report != nil {
		report(*result.diagnostic)
	}
	if result.panicked {
		panic(result.panicValue)
	}
	return result.n, result.err
}

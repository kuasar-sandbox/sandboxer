package directio

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func callerAtDefault(t testing.TB) func() {
	t.Helper()
	// Probe on a disposable goroutine so even a policy that allows promotion
	// but denies restoration cannot contaminate the testing goroutine.
	probe := make(chan error)
	go func() {
		runtime.LockOSThread()
		retire := false
		defer func() {
			if !retire {
				runtime.UnlockOSThread()
			}
		}()
		old, err := getThreadPriority()
		if err == nil && old>>ioPriorityClassShift&ioPriorityClassMask == 1 {
			err = fmt.Errorf("do not change an inherited RT test thread")
		}
		if err == nil && old>>ioPriorityClassShift&ioPriorityClassMask == 0 {
			high, policyErr := priorityAtLeastBE0(old, getThreadPolicy)
			if policyErr != nil {
				err = policyErr
			} else if high {
				err = fmt.Errorf("do not change an inherited CPU RT test thread")
			}
		}
		if err == nil {
			err = setThreadPriority(ioPriorityBE0)
			if err == nil {
				err = setThreadPriority(old)
				retire = err != nil
			}
		}
		probe <- err
	}()
	if err := <-probe; err != nil {
		t.Skipf("native ioprio save/set/restore unavailable: %v", err)
	}
	runtime.LockOSThread()
	old, err := getThreadPriority()
	if err != nil {
		runtime.UnlockOSThread()
		t.Skipf("native ioprio get unavailable: %v", err)
	}
	if old>>ioPriorityClassShift&ioPriorityClassMask == 1 {
		runtime.UnlockOSThread()
		t.Skip("do not change a test runner's inherited RT priority")
	}
	if err := setThreadPriority(0); err != nil {
		runtime.UnlockOSThread()
		t.Skipf("native ioprio reset unavailable: %v", err)
	}
	return func() {
		if err := setThreadPriority(old); err != nil {
			t.Errorf("restore test caller: %v; retaining thread lock until test goroutine exits", err)
			return
		}
		runtime.UnlockOSThread()
	}
}

func collect(target *[]priorityDiagnostic) func(priorityDiagnostic) {
	return func(d priorityDiagnostic) { *target = append(*target, d) }
}

func requireDiag(t *testing.T, ds []priorityDiagnostic, stage priorityStage) {
	t.Helper()
	if len(ds) != 1 || ds[0].stage != stage || ds[0].err != unix.EPERM {
		t.Fatalf("priorityDiagnostics = %#v, want %s/EPERM", ds, stage)
	}
}

func TestIOPrioritySetAndRestore(t *testing.T) {
	defer callerAtDefault(t)()
	callerTID := unix.Gettid()
	api := nativePrioritySyscalls()
	var workerTID, before, during, after int
	var reads atomic.Int32
	api.get = func() (int, error) {
		v, err := getThreadPriority()
		if reads.Add(1) == 2 {
			before = v
		}
		return v, err
	}
	var sets atomic.Int32
	api.set = func(v int) error {
		err := setThreadPriority(v)
		if sets.Add(1) == 2 {
			after, _ = getThreadPriority()
		}
		return err
	}
	var ds []priorityDiagnostic
	n, err := withPrioritySyscalls(func() (int, error) {
		workerTID = unix.Gettid()
		during, _ = getThreadPriority()
		for range 100 {
			runtime.Gosched()
		}
		if unix.Gettid() != workerTID {
			panic("changed worker thread")
		}
		return 7, nil
	}, api, collect(&ds))
	if n != 7 || err != nil || len(ds) != 0 || during != ioPriorityBE0 || after != before || workerTID == callerTID {
		t.Fatalf("n=%d err=%v ds=%v caller=%d worker=%d before=%d during=%d after=%d", n, err, ds, callerTID, workerTID, before, during, after)
	}
	callerAfter, _ := getThreadPriority()
	if callerAfter != 0 {
		t.Fatalf("caller polluted: %d", callerAfter)
	}
	t.Logf("caller=%d worker=%d priority=%d→%d→%d", callerTID, workerTID, before, during, after)
}

func TestIOPrioritySetupFailuresPreserveResult(t *testing.T) {
	for _, stage := range []priorityStage{priorityCallerGet, priorityCallerPolicy, priorityWorkerGet, priorityWorkerPolicy, prioritySet} {
		t.Run(stage.String(), func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			api := prioritySyscalls{set: func(int) error { return nil }}
			var gets, policies atomic.Int32
			api.get = func() (int, error) {
				i := gets.Add(1)
				if stage == priorityCallerGet && i == 1 || stage == priorityWorkerGet && i == 2 {
					return 0, unix.EPERM
				}
				return 0, nil
			}
			api.policy = func() (int, error) {
				i := policies.Add(1)
				if stage == priorityCallerPolicy && i == 1 || stage == priorityWorkerPolicy && i == 2 {
					return 0, unix.EPERM
				}
				return 0, nil // SCHED_OTHER
			}
			if stage == prioritySet {
				api.set = func(int) error { return unix.EPERM }
			}
			var ds []priorityDiagnostic
			calls := 0
			ioErr := errors.New("actual IO failure")
			n, err := withPrioritySyscalls(func() (int, error) { calls++; return 13, ioErr }, api, collect(&ds))
			if n != 13 || err != ioErr || calls != 1 {
				t.Fatalf("n=%d err=%v calls=%d", n, err, calls)
			}
			requireDiag(t, ds, stage)
		})
	}
}

func TestIOPriorityRestoreFailureRetiresWorker(t *testing.T) {
	defer callerAtDefault(t)()
	callerTID := unix.Gettid()
	api := nativePrioritySyscalls()
	var sets atomic.Int32
	api.set = func(v int) error {
		if sets.Add(1) == 2 {
			return unix.EPERM
		}
		return setThreadPriority(v)
	}
	workerTID := 0
	var ds []priorityDiagnostic
	calls := 0
	n, err := withPrioritySyscalls(func() (int, error) {
		calls++
		workerTID = unix.Gettid()
		v, _ := getThreadPriority()
		if v != ioPriorityBE0 {
			panic(fmt.Sprintf("priority=%d", v))
		}
		return 4096, nil
	}, api, collect(&ds))
	if n != 4096 || err != nil || calls != 1 {
		t.Fatalf("n=%d err=%v calls=%d", n, err, calls)
	}
	requireDiag(t, ds, priorityRestore)
	if unix.Gettid() != callerTID {
		t.Fatal("caller moved despite outer LockOSThread")
	}
	if v, _ := getThreadPriority(); v != 0 {
		t.Fatalf("caller polluted: %d", v)
	}
	path := fmt.Sprintf("/proc/self/task/%d", workerTID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("poisoned worker thread %d still alive", workerTID)
		}
		runtime.Gosched()
	}
	n, err = withPrioritySyscalls(func() (int, error) { return 2, nil }, nativePrioritySyscalls(), nil)
	if n != 2 || err != nil {
		t.Fatalf("following call: %d,%v", n, err)
	}
	t.Logf("worker %d was observed absent from /proc/self/task; caller %d remained priority 0", workerTID, callerTID)
}

func TestIOPriorityRestoreFailurePreservesError(t *testing.T) {
	defer callerAtDefault(t)()
	api := nativePrioritySyscalls()
	var calls atomic.Int32
	api.set = func(v int) error {
		if calls.Add(1) == 2 {
			return unix.EPERM
		}
		return setThreadPriority(v)
	}
	var ds []priorityDiagnostic
	n, err := withPrioritySyscalls(func() (int, error) { return 8, unix.EIO }, api, collect(&ds))
	if n != 8 || err != unix.EIO {
		t.Fatalf("actual result changed: %d,%v", n, err)
	}
	requireDiag(t, ds, priorityRestore)
}

func TestIOPriorityPreservesHigherCaller(t *testing.T) {
	for _, tc := range []struct {
		name        string
		raw, policy int
	}{
		{"explicit_RT", (1 << 13) | 4, 0}, {"BE0", ioPriorityBE0, 0},
		{"NONE_FIFO", 0, 1}, {"NONE_RR", 0, 2}, {"NONE_DEADLINE", 0, 6}, {"NONE_FIFO_reset", 0, 1 | schedResetOnFork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			caller := unix.Gettid()
			api := nativePrioritySyscalls()
			api.get = func() (int, error) { return tc.raw, nil }
			api.policy = func() (int, error) { return tc.policy, nil }
			api.set = func(int) error { panic("must not change already higher priority") }
			n, err := withPrioritySyscalls(func() (int, error) {
				if unix.Gettid() != caller {
					panic("moved higher-priority caller")
				}
				return 5, nil
			}, api, nil)
			if n != 5 || err != nil {
				t.Fatalf("n=%d err=%v", n, err)
			}
		})
	}
}

func TestIOPriorityDiagnosticsAreBounded(t *testing.T) {
	var warnings priorityWarnings
	var count atomic.Int32
	var callers sync.WaitGroup
	for range 64 {
		callers.Go(func() {
			if warnings.first(priorityRestore) {
				count.Add(1)
			}
		})
	}
	callers.Wait()
	if count.Load() != 1 || !warnings.first(prioritySet) || warnings.first(prioritySet) {
		t.Fatal("diagnostics must warn once per stage, independently of concurrent callers")
	}
}

func TestIOPriorityPreservesCallerBE7(t *testing.T) {
	defer callerAtDefault(t)()
	if err := setThreadPriority(ioPriorityBE0 | 7); err != nil {
		t.Fatal(err)
	}
	n, err := withPrioritySyscalls(func() (int, error) {
		v, e := getThreadPriority()
		if e != nil {
			return 0, e
		}
		if v != ioPriorityBE0 {
			panic("not BE0")
		}
		return 1, nil
	}, nativePrioritySyscalls(), nil)
	v, _ := getThreadPriority()
	if n != 1 || err != nil || v != ioPriorityBE0|7 {
		t.Fatalf("n=%d err=%v caller=%d", n, err, v)
	}
}

func TestIOPriorityPanicReturnsToCaller(t *testing.T) {
	defer callerAtDefault(t)()
	api := nativePrioritySyscalls()
	var count atomic.Int32
	restored := false
	api.set = func(v int) error {
		err := setThreadPriority(v)
		if count.Add(1) == 2 && err == nil {
			restored = true
		}
		return err
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = withPrioritySyscalls(func() (int, error) { panic("original panic") }, api, nil)
	}()
	if recovered != "original panic" || !restored {
		t.Fatalf("panic=%v restored=%v", recovered, restored)
	}
}

func TestIOPriorityScopeIncludesEINTRLoop(t *testing.T) {
	defer callerAtDefault(t)()
	api := nativePrioritySyscalls()
	var setCount atomic.Int32
	api.set = func(v int) error { setCount.Add(1); return setThreadPriority(v) }
	calls := 0
	n, err := withPrioritySyscalls(func() (int, error) {
		op := func() (int, error) {
			calls++
			if calls == 1 {
				return 0, unix.EINTR
			}
			return 3, unix.EINTR
		}
		for {
			n, err := op()
			if n > 0 || err != unix.EINTR {
				return n, err
			}
		}
	}, api, nil)
	if n != 3 || err != unix.EINTR || calls != 2 || setCount.Load() != 2 {
		t.Fatalf("n=%d err=%v calls=%d sets=%d", n, err, calls, setCount.Load())
	}
}

func BenchmarkBareSyscall(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _, _ = unix.Syscall(unix.SYS_GETTID, 0, 0, 0)
	}
}

func BenchmarkInPlaceSuccessScope(b *testing.B) {
	defer callerAtDefault(b)()
	b.ReportAllocs()
	for b.Loop() {
		runtime.LockOSThread()
		old, err := getThreadPriority()
		if err != nil {
			b.Fatal(err)
		}
		if err := setThreadPriority(ioPriorityBE0); err != nil {
			b.Fatal(err)
		}
		_, _, _ = unix.Syscall(unix.SYS_GETTID, 0, 0, 0)
		if err := setThreadPriority(old); err != nil {
			b.Fatal(err)
		}
		runtime.UnlockOSThread()
	}
}

func BenchmarkOwnedScopeUnpinnedCaller(b *testing.B) {
	b.ReportAllocs()
	api := nativePrioritySyscalls()
	for b.Loop() {
		_, err := withPrioritySyscalls(func() (int, error) {
			v, _, e := unix.Syscall(unix.SYS_GETTID, 0, 0, 0)
			if e != 0 {
				return 0, e
			}
			return int(v), nil
		}, api, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOwnedScopePinnedCaller(b *testing.B) {
	defer callerAtDefault(b)()
	b.ReportAllocs()
	api := nativePrioritySyscalls()
	for b.Loop() {
		_, err := withPrioritySyscalls(func() (int, error) {
			v, _, e := unix.Syscall(unix.SYS_GETTID, 0, 0, 0)
			if e != 0 {
				return 0, e
			}
			return int(v), nil
		}, api, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkPread(b *testing.B, isolated, pinCaller bool) {
	if pinCaller {
		defer callerAtDefault(b)()
	}
	f, err := os.Open("/dev/zero")
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 4096)
	op := func() (int, error) { return unix.Pread(int(f.Fd()), buf, 0) }
	api := nativePrioritySyscalls()
	b.ReportAllocs()
	b.SetBytes(int64(len(buf)))
	for b.Loop() {
		var n int
		var err error
		if isolated {
			n, err = withPrioritySyscalls(op, api, nil)
		} else {
			n, err = op()
		}
		if n != len(buf) || err != nil {
			b.Fatalf("pread result: %d,%v", n, err)
		}
	}
}

// /dev/zero exercises an actual read syscall without a block device. This is a
// wrapper-overhead comparison only, not a storage-throughput benchmark.
func BenchmarkBarePread4K(b *testing.B)          { benchmarkPread(b, false, false) }
func BenchmarkOwnedPread4KUnpinned(b *testing.B) { benchmarkPread(b, true, false) }
func BenchmarkOwnedPread4KPinned(b *testing.B)   { benchmarkPread(b, true, true) }

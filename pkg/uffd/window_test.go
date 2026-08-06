package uffd

import "testing"

func TestTailWindowSequentialGrowthAndCap(t *testing.T) {
	h := newUnitHandler(t, 8*MaxTailBytes, ZeroSource{})
	faultOffset := uint64(0)
	want := []uint64{64 << 10, 128 << 10, 256 << 10, 512 << 10, 1 << 20, 1 << 20}
	for i, wantWindow := range want {
		epoch := h.beginWindowEvent(tailWindowData, StateAbsent)
		task := windowTask(tailWindowData, StateAbsent, faultOffset, epoch)
		window, track := h.chooseWindow(task)
		if !track || window != wantWindow {
			t.Fatalf("step %d window = %d track=%v, want %d/true", i, window, track, wantWindow)
		}
		h.finishWindow(task, window, window, true, track)
		faultOffset = task.start + window
	}
	if got := h.Stats()["tail_window_grows"]; got != 4 {
		t.Fatalf("window grows = %d, want 4", got)
	}
	if got := h.Stats()["tail_window_current"]; got != MaxTailBytes {
		t.Fatalf("current window = %d, want %d", got, MaxTailBytes)
	}
}

func TestTailWindowResetRules(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Handler) uint64
	}{
		{
			name: "non-contiguous",
			run: func(h *Handler) uint64 {
				fault := establishWindow(t, h, tailWindowData, StateAbsent, 0)
				epoch := h.beginWindowEvent(tailWindowData, StateAbsent)
				task := windowTask(tailWindowData, StateAbsent, fault+PageSize, epoch)
				window, _ := h.chooseWindow(task)
				return window
			},
		},
		{
			name: "task drop",
			run: func(h *Handler) uint64 {
				fault := establishWindow(t, h, tailWindowData, StateAbsent, 0)
				epoch := h.beginWindowEvent(tailWindowData, StateAbsent)
				h.dropWindowEvent(tailWindowData, StateAbsent, epoch)
				epoch = h.beginWindowEvent(tailWindowData, StateAbsent)
				task := windowTask(tailWindowData, StateAbsent, fault, epoch)
				window, _ := h.chooseWindow(task)
				return window
			},
		},
		{
			name: "no completion",
			run: func(h *Handler) uint64 {
				fault := establishWindow(t, h, tailWindowData, StateAbsent, 0)
				epoch := h.beginWindowEvent(tailWindowData, StateAbsent)
				task := windowTask(tailWindowData, StateAbsent, fault, epoch)
				window, track := h.chooseWindow(task)
				h.finishWindow(task, window, 0, false, track)
				epoch = h.beginWindowEvent(tailWindowData, StateAbsent)
				task = windowTask(tailWindowData, StateAbsent, fault, epoch)
				window, _ = h.chooseWindow(task)
				return window
			},
		},
		{
			name: "mode change",
			run: func(h *Handler) uint64 {
				fault := establishWindow(t, h, tailWindowData, StateAbsent, 0)
				_ = establishWindow(t, h, tailWindowZero, StateAbsent, 2*MaxTailBytes)
				epoch := h.beginWindowEvent(tailWindowData, StateAbsent)
				task := windowTask(tailWindowData, StateAbsent, fault, epoch)
				window, _ := h.chooseWindow(task)
				return window
			},
		},
		{
			name: "expected change",
			run: func(h *Handler) uint64 {
				fault := establishWindow(t, h, tailWindowZero, StateAbsent, 0)
				epoch := h.beginWindowEvent(tailWindowZero, StateReleased)
				task := windowTask(tailWindowZero, StateReleased, fault, epoch)
				window, _ := h.chooseWindow(task)
				return window
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newUnitHandler(t, 8*MaxTailBytes, ZeroSource{})
			if got := tt.run(h); got != InitialTailBytes {
				t.Fatalf("window after reset = %d, want %d", got, InitialTailBytes)
			}
			if h.Stats()["tail_window_resets"] == 0 {
				t.Fatal("reset metric was not incremented")
			}
		})
	}
}

func TestTailWindowBusyDropInvalidatesRunningCompletion(t *testing.T) {
	h := newUnitHandler(t, 8*MaxTailBytes, ZeroSource{})
	epoch := h.beginWindowEvent(tailWindowData, StateAbsent)
	running := windowTask(tailWindowData, StateAbsent, 0, epoch)
	window, track := h.chooseWindow(running)

	droppedEpoch := h.beginWindowEvent(tailWindowData, StateAbsent)
	h.dropWindowEvent(tailWindowData, StateAbsent, droppedEpoch)
	h.finishWindow(running, window, window, true, track)

	epoch = h.beginWindowEvent(tailWindowData, StateAbsent)
	next := windowTask(tailWindowData, StateAbsent, running.start+window, epoch)
	got, _ := h.chooseWindow(next)
	if got != InitialTailBytes {
		t.Fatalf("stale completion regrew window to %d", got)
	}
}

func establishWindow(t *testing.T, h *Handler, mode tailWindowMode, expected PageState, faultOffset uint64) uint64 {
	t.Helper()
	epoch := h.beginWindowEvent(mode, expected)
	task := windowTask(mode, expected, faultOffset, epoch)
	window, track := h.chooseWindow(task)
	if window != InitialTailBytes || !track {
		t.Fatalf("initial window = %d track=%v", window, track)
	}
	h.finishWindow(task, window, window, true, track)
	return task.start + window
}

func windowTask(mode tailWindowMode, expected PageState, faultOffset, epoch uint64) tailTask {
	kind := tailDeferredData
	if mode == tailWindowZero {
		kind = tailZero
	}
	return tailTask{
		kind:        kind,
		start:       faultOffset + PageSize,
		faultOffset: faultOffset,
		expected:    expected,
		windowMode:  mode,
		windowEpoch: epoch,
	}
}

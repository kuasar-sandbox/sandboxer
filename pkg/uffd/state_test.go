package uffd

import "testing"

func TestPageStateRunLength(t *testing.T) {
	m := NewPageStateMap(8)
	m.SetRange(2, 5, StateReleased)
	for _, tt := range []struct {
		name       string
		start, max uint64
		wantState  PageState
		want       uint64
	}{
		{name: "absent prefix", start: 0, max: 8, wantState: StateAbsent, want: 2},
		{name: "released run", start: 2, max: 8, wantState: StateReleased, want: 3},
		{name: "max bound", start: 2, max: 2, wantState: StateReleased, want: 2},
		{name: "mismatch", start: 2, max: 4, wantState: StateAbsent, want: 0},
		{name: "end clamp", start: 5, max: 99, wantState: StateAbsent, want: 3},
		{name: "past end", start: 8, max: 1, wantState: StateAbsent, want: 0},
		{name: "zero max", start: 0, max: 0, wantState: StateAbsent, want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.RunLength(tt.start, tt.max, tt.wantState); got != tt.want {
				t.Fatalf("RunLength(%d,%d,%v) = %d, want %d", tt.start, tt.max, tt.wantState, got, tt.want)
			}
		})
	}
}

func TestPageStateSetRangeIf(t *testing.T) {
	m := NewPageStateMap(6)
	m.Set(2, StateReleased)
	m.Set(4, StateLoaded)
	if changed := m.SetRangeIf(0, 9, StateAbsent, StateLoaded); changed != 4 {
		t.Fatalf("changed = %d, want 4", changed)
	}
	want := []PageState{StateLoaded, StateLoaded, StateReleased, StateLoaded, StateLoaded, StateLoaded}
	for page, state := range want {
		if got := m.Get(uint64(page)); got != state {
			t.Fatalf("page %d = %v, want %v", page, got, state)
		}
	}
	if changed := m.SetRangeIf(2, 3, StateAbsent, StateLoaded); changed != 0 {
		t.Fatalf("stale tail changed Released page: %d", changed)
	}
	if changed := m.SetRangeIf(6, 8, StateAbsent, StateLoaded); changed != 0 {
		t.Fatalf("out-of-range change count = %d", changed)
	}
}

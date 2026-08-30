package uffd

import "testing"

func TestPageStateRunBounds(t *testing.T) {
	m := NewPageStateMap(12)
	m.SetRange(0, 12, StateAbsent)
	m.Set(2, StateLoaded)
	m.Set(9, StateReleased)

	start, end := m.RunBounds(6, 1, 11, StateAbsent)
	if start != 3 || end != 9 {
		t.Fatalf("RunBounds = [%d,%d), want [3,9)", start, end)
	}
	start, end = m.RunBounds(6, 5, 8, StateAbsent)
	if start != 5 || end != 8 {
		t.Fatalf("bounded RunBounds = [%d,%d), want [5,8)", start, end)
	}
	start, end = m.RunBounds(2, 0, 12, StateAbsent)
	if start != 2 || end != 2 {
		t.Fatalf("mismatched RunBounds = [%d,%d), want empty at 2", start, end)
	}
	start, end = m.RunBounds(12, 0, 20, StateAbsent)
	if start != 12 || end != 12 {
		t.Fatalf("out-of-range RunBounds = [%d,%d), want empty at 12", start, end)
	}
}

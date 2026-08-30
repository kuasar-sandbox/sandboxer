package uffd

import "testing"

func TestAddressMapLocateIgnoresOverlappingBackendVA(t *testing.T) {
	const (
		memfdLen  = 8 * PageSize
		backendVA = 0x100000
		chVA      = backendVA + 2*PageSize
	)

	m := NewAddressMap(memfdLen)
	if err := m.RegisterVMA(ProcessBackend, backendVA, memfdLen, 0); err != nil {
		t.Fatalf("register backend: %v", err)
	}
	if err := m.RegisterVMA(ProcessCH, chVA, 2*PageSize, 4*PageSize); err != nil {
		t.Fatalf("register CH: %v", err)
	}

	got, ok := m.Locate(chVA + PageSize)
	if !ok {
		t.Fatal("Locate did not find overlapping CH VMA")
	}
	if want := uint64(5 * PageSize); got != want {
		t.Fatalf("Locate = 0x%x, want CH memfd offset 0x%x", got, want)
	}
}

func TestAddressMapLocateRejectsBackendOnlyVA(t *testing.T) {
	const (
		memfdLen  = 4 * PageSize
		backendVA = 0x100000
		chVA      = 0x200000
	)

	m := NewAddressMap(memfdLen)
	if err := m.RegisterVMA(ProcessBackend, backendVA, memfdLen, 0); err != nil {
		t.Fatalf("register backend: %v", err)
	}
	if err := m.RegisterVMA(ProcessCH, chVA, memfdLen, 0); err != nil {
		t.Fatalf("register CH: %v", err)
	}

	if got, ok := m.Locate(backendVA + PageSize); ok {
		t.Fatalf("Locate accepted backend-only VA with offset 0x%x", got)
	}
}

func TestAddressMapCHRegionBounds(t *testing.T) {
	const memfdLen = 8 * PageSize
	m := NewAddressMap(memfdLen)
	if err := m.RegisterVMA(ProcessCH, 0x200000, 3*PageSize, 2*PageSize); err != nil {
		t.Fatal(err)
	}
	start, end, ok := m.CHRegionBounds(4 * PageSize)
	if !ok || start != 2*PageSize || end != 5*PageSize {
		t.Fatalf("CHRegionBounds = [%d,%d),%v, want [%d,%d),true", start, end, ok, 2*PageSize, 5*PageSize)
	}
	if _, _, ok := m.CHRegionBounds(PageSize); ok {
		t.Fatal("CHRegionBounds found an offset outside every CH region")
	}
}

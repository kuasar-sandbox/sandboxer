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

func TestAddressMapLocatePage(t *testing.T) {
	const (
		memfdLen   = 16 * PageSize
		backendVA  = 0x100000
		lowCHVA    = 0x200000 // memfd [0, 8*PageSize)
		highCHVA   = 0x400000 // memfd [8*PageSize, 16*PageSize)
		midPageOff = 0x80     // offset inside a page
	)
	m := NewAddressMap(memfdLen)
	if err := m.RegisterVMA(ProcessBackend, backendVA, memfdLen, 0); err != nil {
		t.Fatalf("register backend: %v", err)
	}
	if err := m.RegisterVMA(ProcessCH, lowCHVA, 8*PageSize, 0); err != nil {
		t.Fatalf("register low CH region: %v", err)
	}
	if err := m.RegisterVMA(ProcessCH, highCHVA, 8*PageSize, 8*PageSize); err != nil {
		t.Fatalf("register high CH region: %v", err)
	}

	for _, tt := range []struct {
		name    string
		faultVA uint64
		wantOff uint64
		wantEnd uint64
		wantOK  bool
	}{
		{
			name:    "mid-page fault resolves to page-aligned offset",
			faultVA: lowCHVA + 3*PageSize + midPageOff,
			wantOff: 3 * PageSize,
			wantEnd: 8 * PageSize,
			wantOK:  true,
		},
		{
			name:    "last page of low region clamps at region end",
			faultVA: lowCHVA + 7*PageSize + midPageOff,
			wantOff: 7 * PageSize,
			wantEnd: 8 * PageSize,
			wantOK:  true,
		},
		{
			name:    "high region reports its own memfd window",
			faultVA: highCHVA + 2*PageSize,
			wantOff: 10 * PageSize,
			wantEnd: 16 * PageSize,
			wantOK:  true,
		},
		{
			name:    "backend VA is never resolved",
			faultVA: backendVA + PageSize,
			wantOK:  false,
		},
		{
			name:    "unregistered VA misses",
			faultVA: 0x800000,
			wantOK:  false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			off, end, ok := m.LocatePage(tt.faultVA)
			if ok != tt.wantOK {
				t.Fatalf("LocatePage ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if off != tt.wantOff {
				t.Fatalf("pageOffset = 0x%x, want 0x%x", off, tt.wantOff)
			}
			if end != tt.wantEnd {
				t.Fatalf("regionEndOff = 0x%x, want 0x%x", end, tt.wantEnd)
			}
		})
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

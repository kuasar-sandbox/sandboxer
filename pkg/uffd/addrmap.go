package uffd

import (
	"fmt"
	"sync"
)

// ProcessKind labels which process's VMA recorded an entry.
type ProcessKind uint8

const (
	// ProcessBackend = sandbox-ctl's mmap of the memfd. Single VMA
	// covering the entire memfd at offset 0.
	ProcessBackend ProcessKind = 1
	// ProcessCH = cloud-hypervisor's mmap of the same memfd. May span
	// multiple VMAs when CH splits the zone across the x86 PCI hole
	// (low region [0,3GiB), high region [4GiB,...]); each region
	// reports independently via va_report and arrives at a distinct
	// memfdOffset.
	ProcessCH ProcessKind = 2
)

// vma holds one virtual-memory-area record. memfdOffset is the byte
// offset within the underlying memfd that this VMA's start corresponds
// to; for a fault at faultVA in [start, end), the memfd offset is
// memfdOffset + (faultVA - start).
type vma struct {
	process     ProcessKind
	start       uint64
	end         uint64
	memfdOffset uint64
}

// AddressMap translates a CH userfaultfd event VA into the memfd-relative
// offset and a memfd-relative offset into the sandbox-ctl backend VMA. Both
// backendVA and chVA cover the same memfd inode, but they belong to different
// process address spaces and their numeric VA ranges may overlap. Backend is
// one VMA, CH may be multiple. Stable in steady state (RLock per fault); only
// mutated when registering a VMA at sandbox startup or when handling a
// va_report from CH.
type AddressMap struct {
	mu       sync.RWMutex
	memfdLen uint64
	vmas     []vma
}

// NewAddressMap creates an empty map sized for a memfd of memfdLen bytes.
func NewAddressMap(memfdLen uint64) *AddressMap {
	return &AddressMap{memfdLen: memfdLen}
}

// RegisterVMA records that [vaStart, vaStart+size) maps the memfd
// starting at byte offset memfdOffset. Multiple registrations are
// allowed; each is appended in arrival order.
//
// Validation: size > 0, memfdOffset + size ≤ memfdLen, and the new
// memfd range [memfdOffset, memfdOffset+size) must not overlap any
// previously-registered range owned by the same process (catches
// duplicate-region registration mistakes early).
func (m *AddressMap) RegisterVMA(p ProcessKind, vaStart, size, memfdOffset uint64) error {
	if size == 0 {
		return fmt.Errorf("uffd: AddressMap RegisterVMA size=0")
	}
	if memfdOffset+size > m.memfdLen || memfdOffset+size < memfdOffset {
		return fmt.Errorf("uffd: AddressMap RegisterVMA range [0x%x,+0x%x) exceeds memfdLen 0x%x",
			memfdOffset, size, m.memfdLen)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.vmas {
		v := &m.vmas[i]
		if v.process != p {
			continue
		}
		newEnd := memfdOffset + size
		oldEnd := v.memfdOffset + (v.end - v.start)
		if memfdOffset < oldEnd && v.memfdOffset < newEnd {
			return fmt.Errorf(
				"uffd: AddressMap RegisterVMA process=%d new memfd range [0x%x,+0x%x) overlaps existing [0x%x,+0x%x)",
				p, memfdOffset, size, v.memfdOffset, v.end-v.start)
		}
	}
	m.vmas = append(m.vmas, vma{
		process:     p,
		start:       vaStart,
		end:         vaStart + size,
		memfdOffset: memfdOffset,
	})
	return nil
}

// Locate returns (memfdOffset, true) if faultVA falls within a CH VMA, else
// (_, false). Every caller handles an event read from a CH-owned userfaultfd;
// the backend mapping is never userfaultfd-registered and is resolved only by
// BackendVAFor. Filtering by process is required because backend and CH live in
// different address spaces, so their numeric VA ranges may legitimately
// overlap.
func (m *AddressMap) Locate(faultVA uint64) (uint64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.vmas {
		v := &m.vmas[i]
		if v.process != ProcessCH {
			continue
		}
		if faultVA >= v.start && faultVA < v.end {
			return v.memfdOffset + (faultVA - v.start), true
		}
	}
	return 0, false
}

// LocatePage resolves a CH-side fault VA in a single RLock pass: the
// page-aligned memfd offset of the faulting page plus the exclusive end
// offset of the containing CH region. The region end lets the fault
// worker clamp fill ioctls to one region without re-entering the map —
// when CH splits a zone across the x86 PCI hole the regions sit at
// distinct, non-contiguous VAs, and a batch crossing the boundary
// resolves to no compatible userfaultfd VMA (ENOENT on the tail).
// Returns ok=false when no CH VMA covers faultVA.
func (m *AddressMap) LocatePage(faultVA uint64) (pageOffset, regionEndOff uint64, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.vmas {
		v := &m.vmas[i]
		if v.process != ProcessCH {
			continue
		}
		if faultVA >= v.start && faultVA < v.end {
			off := v.memfdOffset + (faultVA - v.start)
			return off &^ (PageSize - 1), v.memfdOffset + (v.end - v.start), true
		}
	}
	return 0, 0, false
}

// BackendVAFor returns the backendVA address that corresponds to the
// given memfd offset (for issuing a reciprocal madvise(DONTNEED) on
// backend mm in response to EVENT_REMOVE on a CH-side VMA). Returns
// (0, 0, false) if no backend VMA covers the offset.
//
// Returns (va, length, true) where length is the contiguous bytes of
// backendVA aligned with [memfdOffset, +reqLen) within the matching
// VMA. Backend is registered as a single full-memfd VMA in the
// current architecture, so length always equals reqLen.
func (m *AddressMap) BackendVAFor(memfdOffset, reqLen uint64) (uint64, uint64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := range m.vmas {
		v := &m.vmas[i]
		if v.process != ProcessBackend {
			continue
		}
		size := v.end - v.start
		if memfdOffset >= v.memfdOffset && memfdOffset < v.memfdOffset+size {
			vaOff := memfdOffset - v.memfdOffset
			avail := size - vaOff
			if avail > reqLen {
				avail = reqLen
			}
			return v.start + vaOff, avail, true
		}
	}
	return 0, 0, false
}

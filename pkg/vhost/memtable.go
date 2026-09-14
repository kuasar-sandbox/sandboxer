package vhost

import (
	"encoding/binary"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// MemRegion describes one chunk of guest physical memory the master
// (cloud-hypervisor) has shared with the backend via SET_MEM_TABLE.
//
// guestPhysAddr is the GPA the region starts at.
// userspaceAddr is the master's HVA (we use this only for sanity check).
// memorySize is the region size in bytes.
// mmapOffset is the offset into the underlying fd corresponding to GPA 0.
// mmapBytes is the backend-process slice covering this region, sub-sliced
// from a caller-supplied memfd-mapped slab (unified-memfd invariant,
// docs/cloud-hypervisor.md §3.1). The backend does NOT own the mmap; the slab is provided by
// pkg/memory.
type MemRegion struct {
	GuestPhysAddr uint64
	MemorySize    uint64
	UserspaceAddr uint64
	MmapOffset    uint64
	mmapBytes     []byte
}

// MemTable holds the active memory regions the backend can translate
// guest physical addresses against. Mappings are borrowed from a
// shared memfd slab; this table never owns/unmaps them.
type MemTable struct {
	regions []MemRegion
}

// SetRegions replaces the memory table atomically. The previous regions'
// mmap slices are NOT munmap'd — they're borrowed from the caller-owned
// memfd slab and outlive any individual SET_MEM_TABLE.
func (m *MemTable) SetRegions(regs []MemRegion) {
	m.regions = regs
}

// TranslateGPA converts a guest physical address + length into a backend
// HVA pointing at the same bytes. Used for descriptor.Addr translation
// (virtio spec: descriptor table contents are GPAs).
//
// The returned slice aliases the mmap; reads/writes go directly to shared
// guest memory. The slice is valid until the next SetRegions call replaces
// the underlying mmap (which only happens at SET_MEM_TABLE — well outside
// any in-flight request).
func (m *MemTable) TranslateGPA(gpa uint64, length uint64) ([]byte, error) {
	for i := range m.regions {
		r := &m.regions[i]
		if gpa >= r.GuestPhysAddr && gpa+length <= r.GuestPhysAddr+r.MemorySize {
			off := gpa - r.GuestPhysAddr
			return r.mmapBytes[off : off+length], nil
		}
	}
	return nil, fmt.Errorf("vhost: GPA 0x%x len %d not in any region", gpa, length)
}

// TranslateUVA converts a master-process user virtual address + length
// into a backend HVA pointing at the same bytes. Used for ring base
// addresses (SET_VRING_ADDR delivers master UVAs per vhost-user spec:
// desc_user_addr / used_user_addr / avail_user_addr). The translation
// uses the region's UserspaceAddr (master's HVA at the region's start)
// to compute the offset, which is independent of whether master and
// slave mmap'd the fd at the same address.
func (m *MemTable) TranslateUVA(uva uint64, length uint64) ([]byte, error) {
	for i := range m.regions {
		r := &m.regions[i]
		if uva >= r.UserspaceAddr && uva+length <= r.UserspaceAddr+r.MemorySize {
			off := uva - r.UserspaceAddr
			return r.mmapBytes[off : off+length], nil
		}
	}
	return nil, fmt.Errorf("vhost: UVA 0x%x len %d not in any region", uva, length)
}

// TranslateGPAsingleByte returns the HVA for a single byte at gpa.
// Convenience for very small reads (status byte).
func (m *MemTable) TranslateGPAsingleByte(gpa uint64) (*byte, error) {
	b, err := m.TranslateGPA(gpa, 1)
	if err != nil {
		return nil, err
	}
	return &b[0], nil
}

// vhostMemoryRegion is the on-wire layout of one region in SET_MEM_TABLE
// payload (32 bytes).
type vhostMemoryRegion struct {
	GuestPhysAddr uint64
	MemorySize    uint64
	UserspaceAddr uint64
	MmapOffset    uint64
}

// ParseSetMemTable parses a SET_MEM_TABLE payload + the attached fds
// into MemRegion entries. The on-wire payload is:
//
//	uint32 num_regions
//	uint32 padding
//	[num_regions]vhostMemoryRegion
//
// Caller is responsible for closing the fds after mmap completes.
func ParseSetMemTable(payload []byte, fds []int) ([]MemRegion, error) {
	if len(payload) < 8 {
		return nil, fmt.Errorf("vhost: SET_MEM_TABLE payload too short: %d", len(payload))
	}
	numRegions := binary.LittleEndian.Uint32(payload[0:4])
	if int(numRegions) != len(fds) {
		return nil, fmt.Errorf("vhost: SET_MEM_TABLE num_regions=%d but %d fds attached",
			numRegions, len(fds))
	}
	const regSize = 32
	expectedLen := 8 + int(numRegions)*regSize
	if len(payload) < expectedLen {
		return nil, fmt.Errorf("vhost: SET_MEM_TABLE payload size %d < expected %d",
			len(payload), expectedLen)
	}

	regs := make([]MemRegion, numRegions)
	for i := uint32(0); i < numRegions; i++ {
		off := 8 + int(i)*regSize
		raw := payload[off : off+regSize]
		regs[i] = MemRegion{
			GuestPhysAddr: binary.LittleEndian.Uint64(raw[0:8]),
			MemorySize:    binary.LittleEndian.Uint64(raw[8:16]),
			UserspaceAddr: binary.LittleEndian.Uint64(raw[16:24]),
			MmapOffset:    binary.LittleEndian.Uint64(raw[24:32]),
		}
	}
	return regs, nil
}

// BindRegions verifies each region's fd matches the expected memfd
// inode and binds the region's mmapBytes slice as a sub-range of the
// caller-supplied memfd slab (= sandbox-ctl's backendVA mmap of the
// same memfd, see pkg/memory.Memfd).
//
// Returns an error if any fd's inode differs from expectedInode (the
// unified-memfd invariant; docs/cloud-hypervisor.md §3.1) or if a region's [mmap_offset,
// mmap_offset + memory_size) exceeds the slab.
//
// The backend never opens its own mmap — the kernel page cache shares
// pages between sandbox-ctl's mapping and CH's mapping naturally.
func BindRegions(regs []MemRegion, fds []int, expectedInode uint64, slab []byte) error {
	if len(slab) == 0 {
		return fmt.Errorf("vhost: BindRegions called with empty memfd slab")
	}
	for i := range regs {
		var stat syscall.Stat_t
		if err := syscall.Fstat(fds[i], &stat); err != nil {
			return fmt.Errorf("vhost: fstat region %d fd: %w", i, err)
		}
		if stat.Ino != expectedInode {
			return fmt.Errorf("vhost: region %d inode 0x%x != expected 0x%x (unified-memfd invariant)",
				i, stat.Ino, expectedInode)
		}
		off := regs[i].MmapOffset
		size := regs[i].MemorySize
		end := off + size
		if end > uint64(len(slab)) {
			return fmt.Errorf("vhost: region %d mmap_offset=0x%x size=0x%x exceeds slab len=0x%x",
				i, off, size, len(slab))
		}
		regs[i].mmapBytes = slab[off:end:end]
	}
	_ = unix.MAP_SHARED // keep unix import live for future ranges
	return nil
}

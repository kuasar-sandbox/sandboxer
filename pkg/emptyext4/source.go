// Package emptyext4 generates deterministic empty ext4 filesystems as sparse Sources.
// A Source retains layout metadata only. It never allocates or stores encoded
// superblocks, bitmaps, inode tables, directory blocks, or image pages.
// ReadAt directly encodes the requested byte range into the caller's buffer.
package emptyext4

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync/atomic"
	"unsafe"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// Profile identifies the immutable on-disk layout contract. It is not a user option.
const Profile = "empty-ext4/v1"

const (
	BlockSize         uint64 = 4096
	MinSize           int64  = 1 << 20
	MaxSize           int64  = 1 << 40
	inodeSize         uint64 = 256
	maxBlocksPerGroup uint64 = 32768
	descriptorSize    uint64 = 64
	groupsPerFlex     uint64 = 8
)

type group struct {
	start, count                         uint64
	blockBitmap, inodeBitmap, inodeTable uint64
	freeBlocks                           uint32
	hasSuper                             bool
}

// Source is immutable after construction except for its terminal closed flag.
// Concurrent and repeated random RunAt/ReadAt calls are supported. Close is
// called after the consumer has stopped and joined outstanding operations.
type Source struct {
	size, blocks, blocksPerGroup, inodesPerGroup, tableBlocks, gdtBlocks uint64
	rootBlock, freeBlocks                                                uint64
	groups                                                               []group
	data                                                                 []sparse.Extent
	uuid                                                                 [16]byte
	created                                                              uint32
	closed                                                               atomic.Bool
}

type Info struct {
	Size                     uint64 `json:"size"`
	Blocks                   uint64 `json:"blocks"`
	BlocksPerGroup           uint64 `json:"blocks_per_group"`
	Groups                   int    `json:"groups"`
	InodesPerGroup           uint64 `json:"inodes_per_group"`
	Inodes                   uint64 `json:"inodes"`
	InodeTableBlocksPerGroup uint64 `json:"inode_table_blocks_per_group"`
	GDTBlocks                uint64 `json:"gdt_blocks"`
	DataRuns                 int    `json:"data_runs"`
	DataBytes                uint64 `json:"data_bytes"`
	PrebuiltPayloadBytes     uint64 `json:"prebuilt_payload_bytes"`
	RetainedLayoutBytes      uint64 `json:"retained_layout_bytes"`
	Profile                  string `json:"profile"`
}

func ceilDiv(n, d uint64) uint64 { return (n + d - 1) / d }
func alignUp(n, a uint64) uint64 { return ceilDiv(n, a) * a }

// New plans a fixed-size, blank ext4 filesystem for a new, zero-backed disk.
// Ext4 blocks and image capacity are both exactly 4 KiB aligned. In particular,
// no requested capacity is silently rounded down or extended.
func New(size int64) (*Source, error) {
	if err := ValidateSize(size); err != nil {
		return nil, err
	}
	// Canonical identity for empty-ext4/v1. This is UUIDv5 of the name
	// "empty-ext4/v1" in namespace UUIDv5(DNS,"kuasar-sandbox"):
	// namespace 4ca6f6eb-9d0a-5db2-8623-0786b34f6f7c,
	// image UUID aebc4e52-125c-56e0-9e34-3e0732ca282f.
	// Capacity, sandbox identity and wall clock do not participate. A template
	// is content, while diffstore owns each active disk's independent identity.
	id := [16]byte{0xae, 0xbc, 0x4e, 0x52, 0x12, 0x5c, 0x56, 0xe0, 0x9e, 0x34, 0x3e, 0x07, 0x32, 0xca, 0x28, 0x2f}
	return newWithIdentity(size, id, 0)
}

// ValidateSize checks the creation limits without allocating a layout.
func ValidateSize(size int64) error {
	if size < MinSize || size > MaxSize || size%int64(BlockSize) != 0 {
		return fmt.Errorf("emptyext4: size must be 4 KiB aligned and between %d and %d", MinSize, MaxSize)
	}
	return nil
}

// Explicit identity is kept internal for deterministic package tests.
func newWithIdentity(size int64, id [16]byte, created uint32) (*Source, error) {
	if size < MinSize || size > MaxSize || size%int64(BlockSize) != 0 {
		return nil, fmt.Errorf("emptyext4: size must be 4 KiB aligned and between %d and %d", MinSize, MaxSize)
	}
	s := &Source{size: uint64(size), blocks: uint64(size) / BlockSize, created: created, uuid: id}
	count, bpg, ipg, table, gdt, err := geometry(s.blocks)
	if err != nil {
		return nil, err
	}
	s.blocksPerGroup, s.inodesPerGroup, s.tableBlocks, s.gdtBlocks = bpg, ipg, table, gdt
	s.groups = make([]group, count)
	// Only descriptions are retained; metadata for each flex is located in its
	// first physical group. Backup superblocks/GDTs stay in their own groups.
	s.data = make([]sparse.Extent, 0, int(count)+32)
	for i := uint64(0); i < count; i++ {
		g := &s.groups[i]
		g.start = i * s.blocksPerGroup
		g.count = min(s.blocks-g.start, s.blocksPerGroup)
		g.hasSuper = hasSuper(i)
		g.freeBlocks = uint32(g.count)
		if g.hasSuper {
			g.freeBlocks -= uint32(1 + s.gdtBlocks)
		}
	}
	for first := uint64(0); first < count; first += groupsPerFlex {
		n := min(groupsPerFlex, count-first)
		base := s.groups[first].start
		if s.groups[first].hasSuper {
			base += 1 + s.gdtBlocks
		}
		end := base + 2*n + s.tableBlocks*n
		if first == 0 {
			s.rootBlock = end
			end++
		}
		if end >= s.groups[first].start+s.groups[first].count {
			return nil, fmt.Errorf("emptyext4: flex %d metadata does not fit", first/groupsPerFlex)
		}
		s.groups[first].freeBlocks -= uint32(end - base)
		for j := uint64(0); j < n; j++ {
			g := &s.groups[first+j]
			g.blockBitmap = base + j
			g.inodeBitmap = base + n + j
			g.inodeTable = base + 2*n + j*s.tableBlocks
		}
	}
	for i := uint64(0); i < count; i++ {
		g := s.groups[i]
		s.freeBlocks += uint64(g.freeBlocks)
		if g.hasSuper {
			s.addData(g.start, 1+s.gdtBlocks)
		}
		if i%groupsPerFlex != 0 {
			continue
		}
		n := min(groupsPerFlex, count-i)
		for j := uint64(0); j < n; j++ {
			member := s.groups[i+j]
			// A bitmap is entirely zero iff the represented group has no
			// allocated blocks and no out-of-range padding bits. This is a
			// layout fact, not a scan of generated bytes.
			if uint64(member.freeBlocks) != member.count || member.count < BlockSize*8 {
				s.addData(member.blockBitmap, 1)
			}
		}
		s.addData(g.inodeBitmap, n)
		if i == 0 {
			s.addData(g.inodeTable, 1)
			s.addData(s.rootBlock, 1)
		}
	}
	return s, nil
}

// Start with balanced groups, then resolve the rounding-to-eight constraint.
// At large group counts, rounding the average up by a few blocks can consume
// most of the final group. Every candidate therefore recomputes the actual
// group count and verifies the complete last group's metadata requirement.
func geometry(blocks uint64) (count, bpg, ipg, table, gdt uint64, err error) {
	initial := ceilDiv(blocks, maxBlocksPerGroup)
	for bpg = alignUp(ceilDiv(blocks, initial), 8); bpg >= 256; bpg -= 8 {
		count = ceilDiv(blocks, bpg)
		ipg = alignUp(ceilDiv(blocks/2, count), BlockSize/inodeSize)
		table = ipg * inodeSize / BlockSize
		gdt = ceilDiv(count*descriptorSize, BlockSize)
		last := blocks - (count-1)*bpg
		used := 2 + table
		if hasSuper(count - 1) {
			used += 1 + gdt
		}
		if count == 1 {
			used++
		}
		// Group zero also needs the root directory plus backup table space.
		if last > used && bpg > 2+table+1+gdt+1 {
			return
		}
	}
	err = fmt.Errorf("emptyext4: no valid block-group geometry for %d blocks", blocks)
	return
}

// Planned regions are naturally in ascending order; coalesce by layout only.
func (s *Source) addData(block, count uint64) {
	start, size := block*BlockSize, count*BlockSize
	if n := len(s.data); n > 0 && s.data[n-1].Offset+s.data[n-1].Size == start {
		s.data[n-1].Size += size
		return
	}
	s.data = append(s.data, sparse.Extent{Offset: start, Size: size})
}
func hasSuper(group uint64) bool {
	if group <= 1 {
		return true
	}
	for _, base := range [...]uint64{3, 5, 7} {
		v := group
		for v%base == 0 {
			v /= base
		}
		if v == 1 {
			return true
		}
	}
	return false
}
func (s *Source) Info() Info {
	n := uint64(0)
	for _, e := range s.data {
		n += e.Size
	}
	retained := uint64(unsafe.Sizeof(*s)) + uint64(cap(s.groups))*uint64(unsafe.Sizeof(group{})) + uint64(cap(s.data))*uint64(unsafe.Sizeof(sparse.Extent{}))
	return Info{Size: s.size, Blocks: s.blocks, BlocksPerGroup: s.blocksPerGroup, Groups: len(s.groups), InodesPerGroup: s.inodesPerGroup, Inodes: s.inodesPerGroup * uint64(len(s.groups)), InodeTableBlocksPerGroup: s.tableBlocks, GDTBlocks: s.gdtBlocks, DataRuns: len(s.data), DataBytes: n, RetainedLayoutBytes: retained, Profile: "4KiB blocks;256B inodes;8192B/inode;flex_bg=8;ext_attr,dir_index,filetype,extent,64bit,sparse_super,large_file,huge_file,dir_nlink,extra_isize;no journal,resize_inode,checksums"}
}
func (s *Source) Size() uint64 { return s.size }
func (s *Source) Close() error { s.closed.Store(true); return nil }

func (s *Source) RunAt(off, limit uint64) (sparse.Run, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("emptyext4: closed")
	}
	if off >= s.size {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, fmt.Errorf("emptyext4: zero run limit")
	}
	end := off + min(limit, s.size-off)
	index := sort.Search(len(s.data), func(i int) bool { return s.data[i].Offset+s.data[i].Size > off })
	kind := sparse.Hole
	if index < len(s.data) {
		e := s.data[index]
		if e.Offset <= off {
			kind = sparse.Data
			end = min(end, e.Offset+e.Size)
		} else {
			end = min(end, e.Offset)
		}
	}
	return run{s: s, off: off, end: end, kind: kind}, nil
}

type run struct {
	s        *Source
	off, end uint64
	kind     sparse.RunKind
}

func (r run) Offset() uint64       { return r.off }
func (r run) End() uint64          { return r.end }
func (r run) Kind() sparse.RunKind { return r.kind }
func (r run) ReadAt(ctx context.Context, p []byte, inner uint64) (int, error) {
	if inner > r.end-r.off || uint64(len(p)) > r.end-r.off-inner {
		return 0, fmt.Errorf("emptyext4: read outside run")
	}
	if r.s.closed.Load() {
		return 0, fmt.Errorf("emptyext4: closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Run's empty-at-End rule is stronger than Source's offset>=Size EOF rule.
	if len(p) == 0 {
		return 0, nil
	}
	return r.s.ReadAt(ctx, p, r.off+inner)
}

// ReadAt encodes directly into p. It supports unaligned fields and ranges that
// cross metadata, zero padding, whole sparse holes, and block-group boundaries.
func (s *Source) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	if s.closed.Load() {
		return 0, fmt.Errorf("emptyext4: closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off >= s.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := int(min(uint64(len(p)), s.size-off))
	clear(p[:n])
	end := off + uint64(n)
	// The range index lets large reads over a Hole avoid per-page work.
	index := sort.Search(len(s.data), func(i int) bool { return s.data[i].Offset+s.data[i].Size > off })
	for ; index < len(s.data) && s.data[index].Offset < end; index++ {
		e := s.data[index]
		start := max(off, e.Offset)
		stop := min(end, e.Offset+e.Size)
		for at := start; at < stop; {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			next := min(stop, (at/BlockSize+1)*BlockSize)
			s.encodeBlock(p[at-off:next-off], at, at/BlockSize)
			at = next
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// window writes overlapping scalar fields straight into the destination.
// It intentionally does not build even a temporary 4 KiB encoded page.
type window struct {
	p   []byte
	off uint64
}

func (w window) le(at, n, value uint64) {
	start := max(at, w.off)
	end := min(at+n, w.off+uint64(len(w.p)))
	for i := start; i < end; i++ {
		w.p[i-w.off] = byte(value >> ((i - at) * 8))
	}
}
func (w window) bytes(at uint64, value []byte) {
	start := max(at, w.off)
	end := min(at+uint64(len(value)), w.off+uint64(len(w.p)))
	if start < end {
		copy(w.p[start-w.off:end-w.off], value[start-at:end-at])
	}
}
func (w window) ones(bitmapStart, lo, hi uint64) {
	if hi <= lo {
		return
	}
	first := max(bitmapStart+lo/8, w.off)
	last := min(bitmapStart+ceilDiv(hi, 8), w.off+uint64(len(w.p)))
	for at := first; at < last; at++ {
		b := (at - bitmapStart) * 8
		begin := max(lo, b) - b
		finish := min(hi, b+8) - b
		w.p[at-w.off] |= byte(((uint16(1) << finish) - 1) &^ ((uint16(1) << begin) - 1))
	}
}

func (s *Source) encodeBlock(p []byte, off, block uint64) {
	gi := block / s.blocksPerGroup
	g := s.groups[gi]
	w := window{p: p, off: off}
	if g.hasSuper {
		if block == g.start {
			base := block * BlockSize
			if gi == 0 {
				base += 1024
			}
			s.superblock(w, base, gi)
			return
		}
		if block > g.start && block <= g.start+s.gdtBlocks {
			base := (g.start + 1) * BlockSize
			first := (max(off, base) - base) / descriptorSize
			last := min(uint64(len(s.groups)), ceilDiv(off+uint64(len(p))-base, descriptorSize))
			for i := first; i < last; i++ {
				s.descriptor(w, base+i*descriptorSize, i)
			}
			return
		}
	}
	first := gi / groupsPerFlex * groupsPerFlex
	n := min(groupsPerFlex, uint64(len(s.groups))-first)
	base := s.groups[first].blockBitmap
	switch {
	case block >= base && block < base+n:
		g = s.groups[first+block-base]
		base := block * BlockSize
		used := uint64(g.count) - uint64(g.freeBlocks)
		w.ones(base, 0, used)
		w.ones(base, g.count, BlockSize*8)
	case block >= base+n && block < base+2*n:
		owner := first + block - (base + n)
		base := block * BlockSize
		if owner == 0 {
			w.ones(base, 0, 10)
		}
		w.ones(base, s.inodesPerGroup, BlockSize*8)
	case block == s.groups[0].inodeTable:
		s.rootInode(w, block*BlockSize+inodeSize)
	case block == s.rootBlock:
		base := block * BlockSize
		w.le(base, 4, 2)
		w.le(base+4, 2, 12)
		w.le(base+6, 1, 1)
		w.le(base+7, 1, 2)
		w.le(base+8, 1, '.')
		w.le(base+12, 4, 2)
		w.le(base+16, 2, BlockSize-12)
		w.le(base+18, 1, 2)
		w.le(base+19, 1, 2)
		w.le(base+20, 2, uint64('.')|uint64('.')<<8)
	}
}

func (s *Source) superblock(w window, at, group uint64) {
	inodes := s.inodesPerGroup * uint64(len(s.groups))
	w.le(at, 4, inodes)
	w.le(at+4, 4, s.blocks)
	w.le(at+12, 4, s.freeBlocks)
	w.le(at+16, 4, inodes-10)
	w.le(at+0x18, 4, 2)
	w.le(at+0x1c, 4, 2)
	w.le(at+0x20, 4, s.blocksPerGroup)
	w.le(at+0x24, 4, s.blocksPerGroup)
	w.le(at+0x28, 4, s.inodesPerGroup)
	w.le(at+0x30, 4, uint64(s.created))
	w.le(at+0x36, 2, 0xffff)
	w.le(at+0x38, 2, 0xef53)
	w.le(at+0x3a, 2, 1)
	w.le(at+0x3c, 2, 1)
	w.le(at+0x40, 4, uint64(s.created))
	w.le(at+0x4c, 4, 1)
	w.le(at+0x54, 4, 11)
	w.le(at+0x58, 2, inodeSize)
	w.le(at+0x5a, 2, group)
	w.le(at+0x5c, 4, 0x28)
	w.le(at+0x60, 4, 0x2c2)
	w.le(at+0x64, 4, 0x6b)
	w.bytes(at+0x68, s.uuid[:])
	w.bytes(at+0x78, []byte("ks-empty"))
	w.bytes(at+0xec, s.uuid[:])
	w.le(at+0xfc, 1, 1)
	w.le(at+0xfe, 2, descriptorSize)
	w.le(at+0x100, 4, 0xc)
	w.le(at+0x108, 4, uint64(s.created))
	w.le(at+0x150, 4, s.blocks>>32)
	w.le(at+0x158, 4, s.freeBlocks>>32)
	w.le(at+0x15c, 2, 32)
	w.le(at+0x15e, 2, 32)
	// Fix directory hash signedness independent of the guest architecture.
	w.le(at+0x160, 4, 1)
	w.le(at+0x174, 1, 3)
	// For this non-bigalloc/no-journal/no-resize profile, kernel overhead is
	// inode tables + two bitmaps/group + every superblock/GDT copy. The one
	// root-directory block is allocated data, not filesystem overhead.
	w.le(at+0x248, 4, s.blocks-s.freeBlocks-1)
}
func (s *Source) descriptor(w window, at, i uint64) {
	g := s.groups[i]
	freeInodes := s.inodesPerGroup
	if i == 0 {
		freeInodes -= 10
		w.le(at+0x10, 2, 1)
	}
	w.le(at, 4, g.blockBitmap)
	w.le(at+4, 4, g.inodeBitmap)
	w.le(at+8, 4, g.inodeTable)
	w.le(at+12, 2, uint64(g.freeBlocks))
	w.le(at+14, 2, freeInodes)
	w.le(at+0x12, 2, 4)
	w.le(at+0x20, 4, g.blockBitmap>>32)
	w.le(at+0x24, 4, g.inodeBitmap>>32)
	w.le(at+0x28, 4, g.inodeTable>>32)
	w.le(at+0x2c, 2, uint64(g.freeBlocks)>>16)
	w.le(at+0x2e, 2, freeInodes>>16)
}
func (s *Source) rootInode(w window, at uint64) {
	w.le(at, 2, 0x41ed)
	w.le(at+4, 4, BlockSize)
	w.le(at+8, 4, uint64(s.created))
	w.le(at+12, 4, uint64(s.created))
	w.le(at+16, 4, uint64(s.created))
	w.le(at+0x1a, 2, 2)
	w.le(at+0x1c, 4, BlockSize/512)
	w.le(at+0x20, 4, 0x80000)
	w.le(at+0x28, 2, 0xf30a)
	w.le(at+0x2a, 2, 1)
	w.le(at+0x2c, 2, 4)
	w.le(at+0x38, 2, 1)
	w.le(at+0x3a, 2, s.rootBlock>>32)
	w.le(at+0x3c, 4, s.rootBlock)
	w.le(at+0x80, 2, 32)
	w.le(at+0x90, 4, uint64(s.created))
	// Inode seconds are signed; preserve the epoch bit after 2038.
	epoch := uint64((int64(s.created)-int64(int32(s.created)))>>32) & 3
	w.le(at+0x84, 4, epoch)
	w.le(at+0x88, 4, epoch)
	w.le(at+0x8c, 4, epoch)
	w.le(at+0x94, 4, epoch)
}

// Package uffd is the host-side userfaultfd handler for sandbox memory.
//
// One instance per sandbox (per memfd). The handler owns: a uffd fd
// (registered MISSING on the sandbox-ctl-side mmap of the memfd), a
// reader goroutine that drains uffd events, and a worker pool that
// services per-page faults via UFFDIO_COPY / UFFDIO_ZEROPAGE.
//
// The same handler runs in cold-start mode (snapshotReader = ZeroSource,
// every Absent fault becomes ZEROPAGE) and restore mode (snapshotReader
// = StreamSnapshotSource, Absent → COPY from snapshot bytes).
//
// See sandbox.md §8 for the full contract.
package uffd

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// userfaultfd ioctl request numbers, computed from the kernel header.
// _IOWR(0xAA, n, struct sz)  →  0xc0_<sz<<16>>_aa_n
const (
	uffdioAPI        uintptr = 0xc018_aa3f // sizeof(uffdio_api)        = 24
	uffdioRegister   uintptr = 0xc020_aa00 // sizeof(uffdio_register)   = 32
	uffdioUnregister uintptr = 0x8010_aa01 // sizeof(uffdio_range)      = 16
	uffdioWake       uintptr = 0x8010_aa02 // sizeof(uffdio_range)      = 16
	uffdioCopy       uintptr = 0xc028_aa03 // sizeof(uffdio_copy)       = 40
	uffdioZeropage   uintptr = 0xc020_aa04 // sizeof(uffdio_zeropage)   = 32
	uffdioContinue   uintptr = 0xc018_aa07 // sizeof(uffdio_continue)   = 24
)

// uffdio_api struct.
type uffdioAPIStruct struct {
	API      uint64
	Features uint64
	Ioctls   uint64
}

// uffd_msg struct (from <linux/userfaultfd.h>). Fixed 32 bytes.
type uffdMsg struct {
	Event    uint8
	Reserved [3]uint8
	ArgFlags uint32
	// Largest payload: pagefault (24 bytes after the 8-byte preamble)
	// or remove (16 bytes). We over-allocate to 24 and decode by Event.
	Arg [24]byte
}

const (
	uffdEventPagefault uint8 = 0x12
	uffdEventFork      uint8 = 0x13
	uffdEventRemap     uint8 = 0x14
	uffdEventRemove    uint8 = 0x15
	uffdEventUnmap     uint8 = 0x16
)

// uffdMsgPagefault decodes a pagefault message body.
type uffdMsgPagefault struct {
	Flags   uint64
	Address uint64
	TID     uint32
	_       uint32
}

// uffdMsgRemove decodes a EVENT_REMOVE / EVENT_UNMAP body.
type uffdMsgRemove struct {
	Start uint64
	End   uint64
}

// pagefault flag bits.
const (
	uffdPagefaultFlagWrite uint64 = 1 << 0
	uffdPagefaultFlagWP    uint64 = 1 << 1
	uffdPagefaultFlagMinor uint64 = 1 << 2
)

// uffdio_register struct.
type uffdioRegisterStruct struct {
	RangeStart uint64
	RangeLen   uint64
	Mode       uint64
	Ioctls     uint64
}

const (
	uffdRegisterMissing uint64 = 1 << 0
	uffdRegisterWP      uint64 = 1 << 1
)

// uffdio_copy struct.
type uffdioCopyStruct struct {
	Dst    uint64
	Src    uint64
	Len    uint64
	Mode   uint64
	Copied int64 // out
}

const uffdCopyModeWP uint64 = 1 << 1

// uffdio_zeropage struct.
type uffdioZeropageStruct struct {
	RangeStart uint64
	RangeLen   uint64
	Mode       uint64
	Zeropage   int64 // out
}

// uffdio_range struct (used by Wake/Unregister).
type uffdioRangeStruct struct {
	Start uint64
	Len   uint64
}

// API features. Bit positions match <linux/userfaultfd.h>.
//
// IMPORTANT: requesting UFFD_FEATURE_EVENT_FORK (1<<1) makes
// UFFDIO_API return EPERM unless the caller has CAP_SYS_PTRACE; we
// stay clear of it. THREAD_ID + MISSING_SHMEM + EVENT_REMOVE +
// EVENT_UNMAP are unprivileged on Linux 5.10+.
const (
	uffdFeaturePagefaultWP   uint64 = 1 << 0
	uffdFeatureEventFork     uint64 = 1 << 1 // requires CAP_SYS_PTRACE — DO NOT request
	uffdFeatureEventRemap    uint64 = 1 << 2
	uffdFeatureEventRemove   uint64 = 1 << 3
	uffdFeatureMissingHugetb uint64 = 1 << 4
	uffdFeatureMissingShmem  uint64 = 1 << 5
	uffdFeatureEventUnmap    uint64 = 1 << 6
	uffdFeatureSigbus        uint64 = 1 << 7
	uffdFeatureThreadID      uint64 = 1 << 8
)

// suppress unused-warning for bits we name for documentation but
// don't currently request.
var _ = uffdFeaturePagefaultWP
var _ = uffdFeatureEventFork
var _ = uffdFeatureEventRemap
var _ = uffdFeatureMissingHugetb
var _ = uffdFeatureSigbus

const uffdAPI uint64 = 0xaa

// createUffd is the userfaultfd(2) syscall.
func createUffd(flags int) (int, error) {
	r, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(flags), 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(r), nil
}

// ioctlUffdAPI runs UFFDIO_API. features is filled in by the kernel
// upon return with the actually-supported feature bitmask.
func ioctlUffdAPI(fd int, requested uint64) (uint64, error) {
	req := uffdioAPIStruct{API: uffdAPI, Features: requested}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(fd), uffdioAPI,
		uintptr(unsafe.Pointer(&req))); errno != 0 {
		return 0, errno
	}
	return req.Features, nil
}

// ioctlUffdRegister registers [start, start+len) for MISSING faults.
func ioctlUffdRegister(fd int, start, length uint64) error {
	req := uffdioRegisterStruct{
		RangeStart: start,
		RangeLen:   length,
		Mode:       uffdRegisterMissing,
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(fd), uffdioRegister,
		uintptr(unsafe.Pointer(&req))); errno != 0 {
		return errno
	}
	return nil
}

// ioctlUffdCopy invokes UFFDIO_COPY and returns the kernel-reported completed
// byte count even when ioctl returns an errno. Callers validate its range and
// page alignment before updating statistics or PageState.
func ioctlUffdCopy(fd int, dst, src, length uint64) (int64, error) {
	req := uffdioCopyStruct{
		Dst: dst,
		Src: src,
		Len: length,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(fd), uffdioCopy,
		uintptr(unsafe.Pointer(&req)))
	return normalizeUffdCompletion(req.Copied, errno)
}

// ioctlUffdZeropage invokes UFFDIO_ZEROPAGE and returns the kernel-reported
// completed byte count even when ioctl returns an errno.
func ioctlUffdZeropage(fd int, dst, length uint64) (int64, error) {
	req := uffdioZeropageStruct{
		RangeStart: dst,
		RangeLen:   length,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(fd), uffdioZeropage,
		uintptr(unsafe.Pointer(&req)))
	return normalizeUffdCompletion(req.Zeropage, errno)
}

// The kernel writes the result of mcopy_atomic/mfill_zeropage into the signed
// output field before returning from ioctl. On a conflict that means both an
// ioctl errno and a negative output value (for example -EEXIST). A negative
// value is an error code, not a byte count; never expose it as completed work.
func normalizeUffdCompletion(completed int64, errno unix.Errno) (int64, error) {
	if completed < 0 {
		if errno != 0 {
			return 0, errno
		}
		return 0, unix.Errno(-completed)
	}
	if errno != 0 {
		return completed, errno
	}
	return completed, nil
}

// ioctlUffdWake wakes any threads sleeping on faults in [start, start+len).
func ioctlUffdWake(fd int, start, length uint64) error {
	req := uffdioRangeStruct{Start: start, Len: length}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(fd), uffdioWake,
		uintptr(unsafe.Pointer(&req))); errno != 0 {
		return errno
	}
	return nil
}

// madviseDontneedRange clears the calling process's PTEs over
// [addr, addr+length) via madvise(DONTNEED). For sandbox-ctl's
// MAP_SHARED mmap of the memfd (backendVA), this releases the
// process-side residency that accumulates as the backend touches
// pages on behalf of vhost-blk DMA / snapshot bytes.
//
// Inode-level reclaim (file pages) is CH's job — its balloon path
// already issues fallocate(PUNCH_HOLE) on the same memfd. We do NOT
// duplicate that here. The split is intentional:
//
//   - fallocate(PUNCH_HOLE) is file-level reclaim → CH owns it (per
//     guest free_page_reporting, region.file_offset() path)
//   - madvise(DONTNEED) is process-level reclaim → each process must
//     do its own; CH's punch on the inode does NOT authoritatively
//     drop sandbox-ctl's PTE/RSS share. Without this call,
//     sandbox-ctl's RSS accumulates 1:1 with guest-allocated memory
//     across the sandbox lifetime, eventually OOM'ing the host even
//     though the inode side is properly reclaimed.
func madviseDontneedRange(addr, length uintptr) error {
	if length == 0 {
		return nil
	}
	// madvise(MADV_DONTNEED) requires page-aligned start AND length;
	// the kernel returns EINVAL otherwise. Callers (handleRemove via
	// uffd EVENT_REMOVE) get page-aligned ranges from the kernel by
	// construction, but check defensively so an upstream bug surfaces
	// here rather than as an unexplained EINVAL deeper in.
	const pageMask = uintptr(PageSize - 1)
	if addr&pageMask != 0 || length&pageMask != 0 {
		return unix.EINVAL
	}
	// addr is a raw MAP_SHARED memfd VA (not Go-managed memory), so pass it
	// straight to madvise(2) as a uintptr — same direct-syscall idiom as the
	// ioctls above. Avoids fabricating a Go slice header from a uintptr, which
	// is what trips go vet's unsafeptr check (a uintptr isn't GC-tracked).
	if _, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, length, uintptr(unix.MADV_DONTNEED)); errno != 0 {
		return errno
	}
	return nil
}

// Package memory wraps the sandbox-ctl-allocated memfd that backs a
// sandbox VM's RAM. The memfd is created here, sealed against
// shrink/grow, mmap'd into sandbox-ctl with MADV_NOHUGEPAGE so uffd can
// route faults at 4 KiB granularity, and handed to cloud-hypervisor as
// an inherited fd via cmd.ExtraFiles.
//
// Lifecycle: Create on sandbox start, Close on sandbox shutdown.
// Between those, sandbox-ctl uses backendVA for direct reads (snapshot
// path) and CH uses chVA after its own mmap of the same fd.
package memory

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Memfd holds the inherited-by-CH backing fd plus the sandbox-ctl-side
// mmap covering the same inode.
type Memfd struct {
	file  *os.File // owns the fd; Close() closes it
	data  []byte   // backendVA: mmap of [0, size) with MAP_SHARED
	inode uint64   // for SET_MEM_TABLE inode-match invariant (docs/cloud-hypervisor.md §3.1)
}

// Create allocates a fresh memfd of size bytes:
//  1. memfd_create(MFD_CLOEXEC | MFD_ALLOW_SEALING)
//  2. ftruncate(size)
//  3. F_ADD_SEALS (SHRINK | GROW | SEAL) — guests can't resize via API
//  4. mmap(MAP_SHARED) into sandbox-ctl process
//  5. madvise(MADV_NOHUGEPAGE) — uffd needs 4 KiB-page semantics
//
// Failures at any step roll back the prior steps cleanly.
func Create(name string, size int64) (*Memfd, error) {
	if size <= 0 {
		return nil, fmt.Errorf("memory: size must be > 0, got %d", size)
	}
	fd, err := unix.MemfdCreate(name, unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("memfd_create: %w", err)
	}
	cleanup := func() { _ = unix.Close(fd) }

	if err := unix.Ftruncate(fd, size); err != nil {
		cleanup()
		return nil, fmt.Errorf("ftruncate: %w", err)
	}

	const seals = unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_SEAL
	if _, _, errno := unix.Syscall(unix.SYS_FCNTL,
		uintptr(fd), unix.F_ADD_SEALS, uintptr(seals)); errno != 0 {
		cleanup()
		return nil, fmt.Errorf("F_ADD_SEALS: %v", errno)
	}

	data, err := unix.Mmap(fd, 0, int(size),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("mmap: %w", err)
	}

	if err := unix.Madvise(data, unix.MADV_NOHUGEPAGE); err != nil {
		_ = unix.Munmap(data)
		cleanup()
		return nil, fmt.Errorf("madvise NOHUGEPAGE: %w", err)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Munmap(data)
		cleanup()
		return nil, fmt.Errorf("fstat: %w", err)
	}

	return &Memfd{
		file:  os.NewFile(uintptr(fd), name),
		data:  data,
		inode: stat.Ino,
	}, nil
}

// File returns the *os.File wrapper. Use it in cmd.ExtraFiles to inherit
// into cloud-hypervisor (which sees fd=3 + ExtraFiles index).
func (m *Memfd) File() *os.File { return m.file }

// FD returns the integer fd (e.g. for ioctls before fork).
func (m *Memfd) FD() int { return int(m.file.Fd()) }

// Addr returns the user-virtual address of the mmap (backendVA).
// Callers using this for raw memory access must respect Size() bounds.
func (m *Memfd) Addr() uintptr {
	if len(m.data) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&m.data[0]))
}

// Size returns the byte size of the mmap.
func (m *Memfd) Size() int { return len(m.data) }

// Inode returns the memfd's tmpfs inode. SET_MEM_TABLE handlers compare
// fstat(received_fd).Ino against this to confirm CH passed back the
// same memfd we gave it (docs/cloud-hypervisor.md §3.1 inode-match invariant).
func (m *Memfd) Inode() uint64 { return m.inode }

// Bytes returns the backing slice for direct memory access. Mutations
// are immediately visible to cloud-hypervisor's own mmap of the same
// inode (MAP_SHARED).
func (m *Memfd) Bytes() []byte { return m.data }

// Close munmaps the sandbox-ctl side and closes our fd. The kernel
// keeps the inode alive as long as cloud-hypervisor still has its own
// fd / mmap; once both sides drop, the shmem inode is reaped.
func (m *Memfd) Close() error {
	var errs []error
	if m.data != nil {
		if err := unix.Munmap(m.data); err != nil {
			errs = append(errs, fmt.Errorf("munmap: %w", err))
		}
		m.data = nil
	}
	if m.file != nil {
		if err := m.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close: %w", err))
		}
		m.file = nil
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

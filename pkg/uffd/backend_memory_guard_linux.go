package uffd

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// BackendMemoryGuard prevents the backend mapping from instantiating a missing
// shmem folio. User accesses fail with SIGBUS; remote GUP and kernel uaccess can
// fail with EFAULT. Population still belongs to the normal CH-side UFFD handler.
// The guard has no reader and subscribes to no asynchronous events.
//
// This protects one VMA, not every alias or file-descriptor access to its inode.
// EnsureLoaded and normal guest descriptor ownership remain required. The guard
// does not pin pages against a concurrent guest discard after EnsureLoaded.
type BackendMemoryGuard struct {
	fd       int
	once     sync.Once
	closeErr error
}

// NewBackendMemoryGuard must run immediately after creating the private backend
// mmap, before handing the memfd or mapping to any consumer. It protects the
// complete mapping and verifies the fresh memfd is still entirely sparse. Any
// error aborts startup: continuing without this protection is not safe.
//
// Keep the guard for the mapping's complete lifetime. After all consumers and
// the CH UFFD handler have stopped, unmap the backend VMA before calling Close.
func NewBackendMemoryGuard(memfdFD int, backendVA uintptr, size int) (_ *BackendMemoryGuard, retErr error) {
	if os.Getpagesize() != PageSize {
		return nil, fmt.Errorf("uffd: backend guard requires %d-byte pages: %w", PageSize, unix.EOPNOTSUPP)
	}
	if size <= 0 || size%PageSize != 0 || backendVA == 0 || backendVA%PageSize != 0 || uintptr(size) > ^uintptr(0)-backendVA {
		return nil, fmt.Errorf("uffd: invalid backend guard range: %w", unix.EINVAL)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(memfdFD, &stat); err != nil {
		return nil, fmt.Errorf("uffd: backend guard fstat: %w", err)
	}
	if stat.Size != int64(size) || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("uffd: backend guard backing size or type mismatch: %w", unix.EINVAL)
	}
	fd, err := createBackendGuardUFFD(createUffd)
	if err != nil {
		return nil, fmt.Errorf("uffd: create backend guard: %w", err)
	}
	g := &BackendMemoryGuard{fd: fd}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, g.Close())
		}
	}()
	// SIGBUS resolves an unprepared access without a reader. REMOVE, UNMAP and
	// FORK events must not be enabled: their acknowledgement needs a reader.
	const requested = uffdFeatureMissingShmem | uffdFeatureSigbus
	features, err := ioctlUffdAPI(fd, requested)
	if err != nil {
		return nil, fmt.Errorf("uffd: backend guard API: %w", err)
	}
	if features&requested != requested {
		return nil, fmt.Errorf("uffd: backend guard missing required features: %w", unix.EOPNOTSUPP)
	}
	if err := ioctlUffdRegister(fd, uint64(backendVA), uint64(size)); err != nil {
		return nil, fmt.Errorf("uffd: register backend guard: %w", err)
	}
	// Detect and reject folios created before this registration. This check
	// does not protect other aliases or FD access. A preexisting folio may
	// already contain unintended zeros; do not silently accept it as a source
	// baseline or change the general CH seed semantics.
	data, err := unix.Seek(memfdFD, 0, unix.SEEK_DATA)
	if !errors.Is(err, unix.ENXIO) {
		if err != nil {
			return nil, fmt.Errorf("uffd: verify fresh backend memory: %w", err)
		}
		return nil, fmt.Errorf("uffd: fresh backend memory already has data at offset %#x", data)
	}
	return g, nil
}

// USER_MODE_ONLY was added after Linux 5.10. Only EINVAL from this fixed flag
// combination means the legacy flags may be tried. Preserve permission errors
// and all API/registration failures; never seek another permission path.
func createBackendGuardUFFD(create func(int) (int, error)) (int, error) {
	fd, err := create(unix.O_CLOEXEC | uffdUserModeOnly)
	if errors.Is(err, unix.EINVAL) {
		return create(unix.O_CLOEXEC)
	}
	return fd, err
}

// Close is nil-safe and idempotent. The caller must first stop consumers and
// unmap the protected backend range; closing earlier would reopen first-touch.
func (g *BackendMemoryGuard) Close() error {
	if g == nil {
		return nil
	}
	g.once.Do(func() { g.closeErr = unix.Close(g.fd) })
	return g.closeErr
}

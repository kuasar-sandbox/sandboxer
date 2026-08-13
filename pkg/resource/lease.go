package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"golang.org/x/sys/unix"
)

const LeaseVersion = 1

// Lease is the immutable, per-sandbox lifecycle inventory record. Dynamic
// resource state deliberately does not belong here; sandbox-ctl reports that
// state over StateSync after a controller restart.
type Lease struct {
	Version          int      `json:"version"`
	SandboxID        string   `json:"sandbox_id"`
	PID              int      `json:"pid"`
	ControllerSocket string   `json:"controller_socket"`
	CgroupPath       string   `json:"cgroup_path"`
	CapacityMemory   uint64   `json:"capacity_memory"`
	CapacityCPUMilli uint64   `json:"capacity_cpu_milli"`
	FloorMemory      uint64   `json:"floor_memory"`
	FloorCPUMilli    uint64   `json:"floor_cpu_milli"`
	StartupMemory    uint64   `json:"startup_memory"`
	ClientFeatures   []string `json:"client_features,omitempty"`
}

func (l Lease) Validate() error {
	if l.Version != LeaseVersion {
		return fmt.Errorf("lease version %d is unsupported", l.Version)
	}
	if l.SandboxID == "" {
		return fmt.Errorf("lease sandbox_id is required")
	}
	if l.PID <= 0 {
		return fmt.Errorf("lease pid must be positive")
	}
	if !filepath.IsAbs(l.ControllerSocket) || filepath.Clean(l.ControllerSocket) != l.ControllerSocket {
		return fmt.Errorf("lease controller_socket must be a clean absolute path")
	}
	if !filepath.IsAbs(l.CgroupPath) || filepath.Clean(l.CgroupPath) != l.CgroupPath {
		return fmt.Errorf("lease cgroup_path must be a clean absolute path")
	}
	if l.CapacityMemory == 0 || l.FloorMemory == 0 || l.FloorMemory > l.CapacityMemory {
		return fmt.Errorf("lease memory bounds are invalid")
	}
	if l.StartupMemory < l.FloorMemory || l.StartupMemory > l.CapacityMemory {
		return fmt.Errorf("lease startup memory is outside floor/capacity")
	}
	if l.CapacityCPUMilli == 0 || l.FloorCPUMilli == 0 || l.FloorCPUMilli > l.CapacityCPUMilli {
		return fmt.Errorf("lease cpu bounds are invalid")
	}
	return nil
}

func (l Lease) Supports(feature string) bool {
	return slices.Contains(l.ClientFeatures, feature)
}

// LeaseDir and LeasePath derive inventory paths solely from the controller
// socket. The SID is hashed so tenant-controlled IDs never become path input.
func LeaseDir(controllerSocket string) string { return controllerSocket + ".leases" }

func LeaseFilename(sandboxID string) string {
	sum := sha256.Sum256([]byte(sandboxID))
	return hex.EncodeToString(sum[:]) + ".json"
}

func LeasePath(controllerSocket, sandboxID string) string {
	return filepath.Join(LeaseDir(controllerSocket), LeaseFilename(sandboxID))
}

// LeaseHandle owns the POSIX record lock for one sandbox lifetime.
type LeaseHandle struct {
	path string
	file *os.File
	once sync.Once
	err  error
}

// POSIX process-associated record locks are released when the owning process
// closes any descriptor for the locked inode. Keep track of leases owned by
// this process so owner-side inspection can reuse the pinned descriptor rather
// than open and close the pathname behind LeaseHandle's back.
var localLeaseHandles = struct {
	sync.Mutex
	byPath map[string]*LeaseHandle
}{byPath: make(map[string]*LeaseHandle)}

func localLeasePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

func CreateLease(l Lease) (*LeaseHandle, error) {
	return createLease(l, nil)
}

// createLease's hook is test-only fault injection immediately before atomic
// publication. Production always calls CreateLease, which passes nil.
func createLease(l Lease, beforePublish func(string, *os.File)) (*LeaseHandle, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	dir := LeaseDir(l.ControllerSocket)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lease mkdir %s: %w", dir, err)
	}
	path := LeasePath(l.ControllerSocket, l.SandboxID)
	payload, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("lease encode: %w", err)
	}
	f, err := os.CreateTemp(dir, "."+LeaseFilename(l.SandboxID)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("lease create temporary file: %w", err)
	}
	tempPath := f.Name()
	published := false
	fail := func(cause error) (*LeaseHandle, error) {
		_ = f.Close()
		if !published {
			_ = os.Remove(tempPath)
		}
		return nil, cause
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("lease chmod temporary file: %w", err))
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lock); err != nil {
		return fail(fmt.Errorf("lease lock temporary file: %w", err))
	}
	if _, err := f.Write(append(payload, '\n')); err != nil {
		return fail(fmt.Errorf("lease write: %w", err))
	}
	if beforePublish != nil {
		beforePublish(path, f)
	}
	localPath := localLeasePath(path)
	for {
		// Serialize publication with every owner-side helper in this process.
		// Without this boundary another goroutine could open and close the newly
		// published inode before it is registered, silently releasing our lock.
		localLeaseHandles.Lock()
		if localLeaseHandles.byPath[localPath] != nil {
			localLeaseHandles.Unlock()
			return fail(fmt.Errorf("lease %s is locked by sandbox process %d", path, os.Getpid()))
		}
		renameErr := unix.Renameat2(unix.AT_FDCWD, tempPath, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE)
		if renameErr == nil {
			published = true
			handle := &LeaseHandle{path: path, file: f}
			localLeaseHandles.byPath[localPath] = handle
			localLeaseHandles.Unlock()
			return handle, nil
		}
		localLeaseHandles.Unlock()
		if !errors.Is(renameErr, unix.EEXIST) {
			return fail(fmt.Errorf("lease publish %s: %w", path, renameErr))
		}
		removed, err := RemoveUnlockedLease(path)
		if err != nil {
			return fail(fmt.Errorf("lease inspect existing %s: %w", path, err))
		}
		if removed {
			continue
		}
		owner, locked, err := LeaseLockOwner(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fail(fmt.Errorf("lease inspect existing %s: %w", path, err))
		}
		if locked {
			return fail(fmt.Errorf("lease %s is locked by sandbox process %d", path, owner))
		}
		// The pathname appeared or changed after stale cleanup. Retry until
		// that inode is either removed or shown to have a live owner.
	}
}

func (h *LeaseHandle) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// Close unlinks while the lock is still held, then closes the descriptor. A
// SIGKILL skips the unlink but still releases the kernel-owned lock.
func (h *LeaseHandle) Close() error {
	if h == nil {
		return nil
	}
	localLeaseHandles.Lock()
	defer localLeaseHandles.Unlock()
	h.once.Do(func() {
		localPath := localLeasePath(h.path)
		if localLeaseHandles.byPath[localPath] == h {
			opened, statErr := h.file.Stat()
			named, pathErr := os.Lstat(h.path)
			switch {
			case statErr != nil:
				h.err = statErr
			case pathErr != nil && !os.IsNotExist(pathErr):
				h.err = pathErr
			case pathErr == nil && os.SameFile(opened, named):
				if err := os.Remove(h.path); err != nil && !os.IsNotExist(err) {
					h.err = err
				}
			}
			delete(localLeaseHandles.byPath, localPath)
		}
		if err := h.file.Close(); h.err == nil && err != nil {
			h.err = err
		}
	})
	return h.err
}

// ReadLease reads one immutable record. Lock ownership must be checked
// separately; file existence/content alone does not establish liveness.
func ReadLease(path string) (Lease, error) {
	localLeaseHandles.Lock()
	defer localLeaseHandles.Unlock()
	if handle := localLeaseHandles.byPath[localLeasePath(path)]; handle != nil {
		return decodeOwnedLease(handle)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return Lease{}, err
	}
	f := os.NewFile(uintptr(fd), "sandbox-resource-lease-read")
	if f == nil {
		_ = unix.Close(fd)
		return Lease{}, fmt.Errorf("lease adopt read fd")
	}
	defer f.Close()
	return decodeLease(f)
}

func decodeOwnedLease(handle *LeaseHandle) (Lease, error) {
	info, err := handle.file.Stat()
	if err != nil {
		return Lease{}, err
	}
	return decodeLease(io.NewSectionReader(handle.file, 0, info.Size()))
}

func decodeLease(r io.Reader) (Lease, error) {
	var lease Lease
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lease); err != nil {
		return lease, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return lease, fmt.Errorf("lease has trailing JSON value")
		}
		return lease, fmt.Errorf("lease trailing content: %w", err)
	}
	if err := lease.Validate(); err != nil {
		return lease, err
	}
	return lease, nil
}

// InspectLease obtains lock ownership and immutable content from one open file
// description, so a pathname replacement cannot splice one process's lock
// identity onto another inode's JSON. A locked corrupt lease returns
// locked=true and its owner together with the decode error, allowing callers
// to install a full-pool unknown charge.
func InspectLease(path string) (lease Lease, ownerPID int, locked bool, err error) {
	localLeaseHandles.Lock()
	defer localLeaseHandles.Unlock()
	if handle := localLeaseHandles.byPath[localLeasePath(path)]; handle != nil {
		lease, err := decodeOwnedLease(handle)
		return lease, os.Getpid(), true, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return Lease{}, 0, false, err
	}
	f := os.NewFile(uintptr(fd), "sandbox-resource-lease-inspect")
	if f == nil {
		_ = unix.Close(fd)
		return Lease{}, 0, false, fmt.Errorf("lease adopt inspect fd")
	}
	defer f.Close()
	ownerPID, locked, err = LockOwnerFD(f.Fd())
	if err != nil || !locked {
		return Lease{}, ownerPID, locked, err
	}
	lease, err = decodeLease(f)
	return lease, ownerPID, true, err
}

// LeaseLockOwner queries the conflicting POSIX write lock with F_GETLK.
// locked=false means the file is stale. The caller should not infer liveness
// from the PID stored in JSON.
func LeaseLockOwner(path string) (ownerPID int, locked bool, err error) {
	localLeaseHandles.Lock()
	defer localLeaseHandles.Unlock()
	if localLeaseHandles.byPath[localLeasePath(path)] != nil {
		return os.Getpid(), true, nil
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, false, err
	}
	defer unix.Close(fd)
	return LockOwnerFD(uintptr(fd))
}

func LockOwnerFD(fd uintptr) (ownerPID int, locked bool, err error) {
	query := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(fd, unix.F_GETLK, &query); err != nil {
		return 0, false, err
	}
	if query.Type == unix.F_UNLCK {
		return 0, false, nil
	}
	return int(query.Pid), true, nil
}

// RemoveUnlockedLease removes a stale record only while holding the same POSIX
// write lock used by its owner. This closes the F_GETLK -> unlink race with a
// new sandbox process opening and locking the stale inode between inventory
// inspection and cleanup. removed=false means another process owns the lease.
func RemoveUnlockedLease(path string) (removed bool, err error) {
	return removeUnlockedLease(path, nil)
}

// removeUnlockedLease's hook is test-only fault injection for pathname
// replacement after open. Production always calls RemoveUnlockedLease.
func removeUnlockedLease(path string, afterOpen func(string, *os.File)) (removed bool, err error) {
	localLeaseHandles.Lock()
	defer localLeaseHandles.Unlock()
	if localLeaseHandles.byPath[localLeasePath(path)] != nil {
		return false, nil
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	f := os.NewFile(uintptr(fd), "stale-sandbox-resource-lease")
	if f == nil {
		_ = unix.Close(fd)
		return false, fmt.Errorf("lease adopt stale fd")
	}
	defer f.Close()
	if afterOpen != nil {
		afterOpen(path, f)
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lock); err != nil {
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EAGAIN) {
			return false, nil
		}
		return false, err
	}
	opened, err := f.Stat()
	if err != nil {
		return false, err
	}
	named, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !os.SameFile(opened, named) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

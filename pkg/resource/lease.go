package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

func CreateLease(l Lease) (*LeaseHandle, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	dir := LeaseDir(l.ControllerSocket)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lease mkdir %s: %w", dir, err)
	}
	path := LeasePath(l.ControllerSocket, l.SandboxID)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lease open %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), "sandbox-resource-lease")
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("lease adopt fd")
	}
	fail := func(cause error) (*LeaseHandle, error) {
		_ = f.Close()
		return nil, cause
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lock); err != nil {
		return fail(fmt.Errorf("lease %s is locked by another sandbox: %w", path, err))
	}
	payload, err := json.Marshal(l)
	if err != nil {
		return fail(fmt.Errorf("lease encode: %w", err))
	}
	if err := f.Truncate(0); err != nil {
		return fail(fmt.Errorf("lease truncate: %w", err))
	}
	if _, err := f.WriteAt(append(payload, '\n'), 0); err != nil {
		return fail(fmt.Errorf("lease write: %w", err))
	}
	return &LeaseHandle{path: path, file: f}, nil
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
	h.once.Do(func() {
		if err := os.Remove(h.path); err != nil && !os.IsNotExist(err) {
			h.err = err
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
	var lease Lease
	f, err := os.Open(path)
	if err != nil {
		return lease, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lease); err != nil {
		return lease, err
	}
	if err := lease.Validate(); err != nil {
		return lease, err
	}
	return lease, nil
}

// LeaseLockOwner queries the conflicting POSIX write lock with F_GETLK.
// locked=false means the file is stale. The caller should not infer liveness
// from the PID stored in JSON.
func LeaseLockOwner(path string) (ownerPID int, locked bool, err error) {
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

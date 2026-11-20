package resource

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// LeaseFileIdentity is persisted with an owner transfer before publication.
// PID alone is not a sufficient identity for ACK-loss replay or PID reuse.
type LeaseFileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func leaseFileIdentity(info os.FileInfo) (LeaseFileIdentity, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return LeaseFileIdentity{}, fmt.Errorf("lease is not a regular Linux file")
	}
	return LeaseFileIdentity{Device: uint64(st.Dev), Inode: st.Ino}, nil
}

// CreateLeaseCandidate prepares a fully written, locked inode without touching
// the previous live SID lease. Only the resource controller may publish it.
func CreateLeaseCandidate(l Lease) (*LeaseHandle, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	dir := LeaseDir(l.ControllerSocket)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "."+LeaseFilename(l.SandboxID)+".handoff-*")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*LeaseHandle, error) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lock); err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(f).Encode(l); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	h := &LeaseHandle{path: f.Name(), file: f, canonical: LeasePath(l.ControllerSocket, l.SandboxID)}
	localLeaseHandles.Lock()
	localLeaseHandles.byPath[localLeasePath(h.path)] = h
	localLeaseHandles.Unlock()
	return h, nil
}

// ConfirmPublished is safe even before an ACK. POSIX locks are lost when the
// owner closes any FD of the locked inode, so the pathname must only be lstat'd.
func (h *LeaseHandle) ConfirmPublished() (bool, error) {
	if h == nil {
		return false, fmt.Errorf("nil lease handle")
	}
	localLeaseHandles.Lock()
	defer localLeaseHandles.Unlock()
	if h.canonical == "" {
		return true, nil
	}
	opened, err := h.file.Stat()
	if err != nil {
		return false, err
	}
	named, err := os.Lstat(h.canonical)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !os.SameFile(opened, named) {
		return false, nil
	}
	delete(localLeaseHandles.byPath, localLeasePath(h.path))
	h.path = h.canonical
	h.canonical = ""
	localLeaseHandles.byPath[localLeasePath(h.path)] = h
	return true, nil
}

func validCandidatePath(controller, sid, path string) bool {
	return filepath.Clean(path) == path && filepath.Dir(path) == LeaseDir(controller) &&
		strings.HasPrefix(filepath.Base(path), "."+LeaseFilename(sid)+".handoff-")
}

// InspectLeaseCandidate verifies lock owner and JSON through the same opened
// inode. It is also safe in embedded deployments that share a process with the
// controller: those reuse the owner's pinned FD instead of opening another.
func InspectLeaseCandidate(controller, sid, path string) (Lease, int, LeaseFileIdentity, error) {
	if !validCandidatePath(controller, sid, path) {
		return Lease{}, 0, LeaseFileIdentity{}, fmt.Errorf("invalid candidate lease path")
	}
	return inspectLeaseIdentity(path)
}

func InspectLeaseIdentity(path string) (Lease, int, LeaseFileIdentity, error) {
	return inspectLeaseIdentity(path)
}

func inspectLeaseIdentity(path string) (Lease, int, LeaseFileIdentity, error) {
	localLeaseHandles.Lock()
	defer localLeaseHandles.Unlock()
	if h := localLeaseHandles.byPath[localLeasePath(path)]; h != nil {
		l, err := decodeOwnedLease(h)
		if err != nil {
			return l, os.Getpid(), LeaseFileIdentity{}, err
		}
		info, err := h.file.Stat()
		if err != nil {
			return l, os.Getpid(), LeaseFileIdentity{}, err
		}
		id, err := leaseFileIdentity(info)
		return l, os.Getpid(), id, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return Lease{}, 0, LeaseFileIdentity{}, err
	}
	f := os.NewFile(uintptr(fd), "checkpoint-lease-inspect")
	defer f.Close()
	pid, locked, err := LockOwnerFD(f.Fd())
	if err != nil || !locked {
		if err == nil {
			err = fmt.Errorf("lease inode is not locked")
		}
		return Lease{}, pid, LeaseFileIdentity{}, err
	}
	l, err := decodeLease(f)
	if err != nil {
		return l, pid, LeaseFileIdentity{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return l, pid, LeaseFileIdentity{}, err
	}
	id, err := leaseFileIdentity(info)
	return l, pid, id, err
}

// PublishLeaseCandidate atomically replaces the canonical path with an already
// locked inode. The caller first persists the exact transfer intent. A repeat
// after ACK loss succeeds only for that same published inode and lock owner.
func PublishLeaseCandidate(controller, sid, candidate string, id LeaseFileIdentity, pid int) error {
	if !validCandidatePath(controller, sid, candidate) || id.Inode == 0 || pid <= 0 {
		return fmt.Errorf("invalid lease publication identity")
	}
	canonical := LeasePath(controller, sid)
	l, owner, actual, err := InspectLeaseCandidate(controller, sid, candidate)
	if os.IsNotExist(err) {
		l, owner, actual, err = InspectLeaseIdentity(canonical)
		if err == nil && owner == pid && l.PID == pid && l.SandboxID == sid && actual == id {
			return nil
		}
		return fmt.Errorf("published lease identity changed")
	}
	if err != nil {
		return err
	}
	if owner != pid || l.PID != pid || l.SandboxID != sid || l.ControllerSocket != controller || actual != id {
		return fmt.Errorf("candidate lease identity changed")
	}
	if err := func() error {
		// Embedded controllers and clients share process-associated POSIX
		// locks. Serialize rename and owner registration with every local
		// lease reader so none can open/close the new inode in between.
		localLeaseHandles.Lock()
		defer localLeaseHandles.Unlock()
		if err := os.Rename(candidate, canonical); err != nil {
			return err
		}
		info, err := os.Lstat(canonical)
		if err != nil {
			return err
		}
		actual, err := leaseFileIdentity(info)
		if err != nil || actual != id {
			return fmt.Errorf("canonical lease publication changed inode")
		}
		// An embedded old holder still pins its replaced inode, but its
		// handle must no longer answer reads for the canonical pathname.
		delete(localLeaseHandles.byPath, localLeasePath(canonical))
		if h := localLeaseHandles.byPath[localLeasePath(candidate)]; h != nil {
			delete(localLeaseHandles.byPath, localLeasePath(candidate))
			h.path, h.canonical = canonical, ""
			localLeaseHandles.byPath[localLeasePath(canonical)] = h
		}
		return nil
	}(); err != nil {
		return err
	}
	dir, err := os.Open(LeaseDir(controller))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

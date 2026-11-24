//go:build linux

package memorycheckpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// VMMIdentity is local, immutable evidence for a CH whose physical exit is
// delegated to the holder. PID alone, and a zombie thread group leader, are
// insufficient to prove release of the external RAM backing.
type VMMIdentity struct {
	PID       int                     `json:"pid"`
	StartTime uint64                  `json:"start_time"`
	BootID    string                  `json:"boot_id"`
	Cgroup    HolderDirectoryIdentity `json:"cgroup"`
}

func bootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", errors.New("empty boot identity")
	}
	return s, nil
}
func (v VMMIdentity) valid() bool {
	return v.PID > 0 && v.StartTime > 0 && v.BootID != "" && filepath.IsAbs(v.Cgroup.Path) &&
		filepath.Clean(v.Cgroup.Path) == v.Cgroup.Path && v.Cgroup.Inode != 0
}
func cgroupIdentity(path string) (HolderDirectoryIdentity, error) {
	f, err := openDirectory(path)
	if err != nil {
		return HolderDirectoryIdentity{}, err
	}
	defer f.Close()
	fi, err := fileIdentity(f)
	if err != nil {
		return HolderDirectoryIdentity{}, err
	}
	return HolderDirectoryIdentity{Path: path, Device: fi.Device, Inode: fi.Inode}, nil
}
func pidfdExited(fd int) (bool, error) {
	p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(p, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		if p[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return false, errors.New("invalid VMM pidfd")
		}
		return p[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
	}
}

// IdentifyVMM is called while the original child and dedicated VMM cgroup
// are owned by Runtime. Failure disables early retirement, not capture.
func IdentifyVMM(pid int, path string) (VMMIdentity, error) {
	if pid <= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return VMMIdentity{}, errors.New("missing exact VMM cgroup")
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return VMMIdentity{}, err
	}
	defer unix.Close(fd)
	start, err := ProcessStartTime(pid)
	if err != nil {
		return VMMIdentity{}, err
	}
	boot, err := bootID()
	if err != nil {
		return VMMIdentity{}, err
	}
	cg, err := cgroupIdentity(path)
	if err != nil {
		return VMMIdentity{}, err
	}
	b, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		return VMMIdentity{}, err
	}
	found := false
	for _, s := range strings.Fields(string(b)) {
		if s == strconv.Itoa(pid) {
			found = true
		}
	}
	if !found {
		return VMMIdentity{}, errors.New("VMM is not in its source cgroup")
	}
	gone, err := pidfdExited(fd)
	if err != nil || gone {
		return VMMIdentity{}, errors.Join(errors.New("VMM already exiting"), err)
	}
	v := VMMIdentity{PID: pid, StartTime: start, BootID: boot, Cgroup: cg}
	if !v.valid() {
		return VMMIdentity{}, errors.New("invalid VMM identity")
	}
	return v, nil
}

// Exited checks the original thread group, not the state of its leader.
// A pidfd is acquired before inspecting the PID's start time. The two possible
// races (exit before open, PID reuse) either prove the old group is gone or
// retain responsibility conservatively. Never signal by an unverified PID.
func (v VMMIdentity) Exited() (bool, error) {
	if !v.valid() {
		return false, errors.New("invalid VMM identity")
	}
	boot, err := bootID()
	if err != nil {
		return false, err
	}
	if boot != v.BootID {
		return true, nil
	}
	fd, err := unix.PidfdOpen(v.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("VMM exit proof: %w", err)
	}
	defer unix.Close(fd)
	start, err := ProcessStartTime(v.PID)
	if errors.Is(err, os.ErrNotExist) {
		return pidfdExited(fd)
	}
	if err != nil {
		return false, err
	}
	if start != v.StartTime {
		return true, nil
	}
	return pidfdExited(fd)
}

// CoversCgroup permits recovery to omit a second charge only for the exact
// still-owned old VMM cgroup and its original consumer. Any foreign userspace
// member, reused identity, or uncertain read preserves the ordinary scan.
// A dedicated managed VMM subgroup can also contain KVM kernel threads;
// PID 0 denotes a kernel task outside the node's PID namespace in this
// trusted subgroup. No historical pathname alone can suppress a new owner.
func (v VMMIdentity) CoversCgroup(path string) (bool, error) {
	if !v.valid() || path != v.Cgroup.Path {
		return false, nil
	}
	boot, err := bootID()
	if err != nil || boot != v.BootID {
		return false, err
	}
	got, err := cgroupIdentity(path)
	if err != nil || got != v.Cgroup {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		return false, err
	}
	for _, s := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(s)
		if err != nil || pid < 0 {
			return false, errors.New("invalid cgroup member")
		}
		if pid == 0 {
			continue
		}
		if pid == v.PID {
			start, err := ProcessStartTime(pid)
			if err == nil && start == v.StartTime {
				continue
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
			if err == nil {
				return false, nil
			}
			// A member disappearing during enumeration is no longer a consumer.
			continue
		}
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		kernel := false
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Kthread:") && strings.TrimSpace(strings.TrimPrefix(line, "Kthread:")) == "1" {
				kernel = true
				break
			}
		}
		if !kernel {
			return false, nil
		}
	}
	// Revalidate the directory after inspecting membership.
	again, err := cgroupIdentity(path)
	return err == nil && again == v.Cgroup, err
}

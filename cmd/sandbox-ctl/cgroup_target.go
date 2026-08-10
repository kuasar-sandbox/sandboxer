package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// resolveCgroupPathArg validates the runtime-only fd=N syntax and resolves the
// descriptor's host path for protocols that carry cgroup identity across
// processes. The returned file owns the inherited descriptor and must remain
// open for the sandbox lifetime.
func resolveCgroupPathArg(value string) (string, *os.File, error) {
	if !strings.HasPrefix(value, "fd=") {
		if !filepath.IsAbs(value) {
			return "", nil, fmt.Errorf("must be an absolute path or fd=N")
		}
		return value, nil, nil
	}

	raw := strings.TrimPrefix(value, "fd=")
	u, err := strconv.ParseUint(raw, 10, 31)
	if err != nil || raw == "" {
		return "", nil, fmt.Errorf("invalid inherited descriptor %q", value)
	}
	fd := int(u)
	if fd < 3 {
		return "", nil, fmt.Errorf("inherited descriptor must be >= 3, got %d", fd)
	}

	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return "", nil, fmt.Errorf("inspect inherited descriptor %d: %w", fd, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", nil, fmt.Errorf("stat inherited descriptor %d: %w", fd, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", nil, fmt.Errorf("inherited descriptor %d is not a directory", fd)
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return "", nil, fmt.Errorf("statfs inherited descriptor %d: %w", fd, err)
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return "", nil, fmt.Errorf("inherited descriptor %d is not on cgroup v2", fd)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return "", nil, fmt.Errorf("set inherited descriptor %d close-on-exec: %w", fd, err)
	}

	f := os.NewFile(uintptr(fd), fmt.Sprintf("cgroup-fd-%d", fd))
	if f == nil {
		return "", nil, fmt.Errorf("adopt inherited descriptor %d", fd)
	}
	target, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		_ = f.Close()
		return "", nil, fmt.Errorf("resolve inherited descriptor %d: %w", fd, err)
	}
	if !filepath.IsAbs(target) || strings.HasSuffix(target, " (deleted)") {
		_ = f.Close()
		return "", nil, fmt.Errorf("inherited descriptor %d has invalid target %q", fd, target)
	}
	return filepath.Clean(target), f, nil
}

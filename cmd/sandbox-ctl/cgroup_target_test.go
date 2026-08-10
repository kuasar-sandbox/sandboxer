package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestResolveCgroupPathArgPath(t *testing.T) {
	path, inherited, err := resolveCgroupPathArg("/sys/fs/cgroup/example")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/sys/fs/cgroup/example" || inherited != nil {
		t.Fatalf("resolved = %q, %v", path, inherited)
	}
	if _, _, err := resolveCgroupPathArg("relative/path"); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestResolveCgroupPathArgFD(t *testing.T) {
	root, err := os.Open("/sys/fs/cgroup")
	if err != nil {
		t.Skipf("open cgroup root: %v", err)
	}
	defer root.Close()
	fd, err := unix.Dup(int(root.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}

	path, inherited, err := resolveCgroupPathArg("fd=" + strconv.Itoa(fd))
	if err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	defer inherited.Close()
	if want := "/sys/fs/cgroup"; path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("FD_CLOEXEC was not restored")
	}
}

func TestResolveCgroupPathArgRejectsInvalidFD(t *testing.T) {
	for _, value := range []string{"fd=", "fd=-1", "fd=2", "fd=abc", "fd=3x"} {
		t.Run(value, func(t *testing.T) {
			if _, _, err := resolveCgroupPathArg(value); err == nil {
				t.Fatalf("%q accepted", value)
			}
		})
	}

	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if _, _, err := resolveCgroupPathArg("fd=" + strconv.Itoa(int(dir.Fd()))); err == nil || !strings.Contains(err.Error(), "cgroup v2") {
		t.Fatalf("non-cgroup directory error = %v", err)
	}
}

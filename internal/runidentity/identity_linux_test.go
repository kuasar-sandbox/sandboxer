package runidentity

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestAcquireOwnPIDAndRejectSameProcessWithoutUnlock(t *testing.T) {
	dir := t.TempDir()
	g, err := Acquire(dir, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	buf := make([]byte, 32)
	n, err := g.file.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(buf[:n])) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("PID %q", buf[:n])
	}
	if second, err := Acquire(dir, "sandbox"); err == nil {
		second.Close()
		t.Fatal("same-process duplicate accepted")
	}
	probeLock(t, g.path, os.Getpid())
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Acquire(dir, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	// A redundant old Close or cleanup must not affect the new acquisition.
	g.Close()
	if err := g.RemoveRunDir(); err != nil {
		t.Fatal(err)
	}
	probeLock(t, next.path, os.Getpid())
}

func TestDuplicateCannotDeleteLiveDirectory(t *testing.T) {
	dir := t.TempDir()
	g, err := Acquire(dir, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	marker := filepath.Join(dir, "owned-data")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := identityChild("compete", dir, "sandbox")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("competitor: %v %s", err, out)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "preserve" {
		t.Fatalf("owner data lost: %q %v", got, err)
	}
	probeLock(t, g.path, os.Getpid())
}

func TestChangedPIDEntryIsNotAcquiredOrRemoved(t *testing.T) {
	dir := t.TempDir()
	pid := filepath.Join(dir, "sandbox.pid")
	_, err := acquire(dir, "sandbox", func() {
		if err := os.Rename(pid, pid+".retired"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pid, []byte("replacement\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("replaced entry accepted")
	}
	if got, err := os.ReadFile(pid); err != nil || string(got) != "replacement\n" {
		t.Fatalf("replacement changed: %q %v", got, err)
	}
	g, err := Acquire(dir, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := os.Rename(pid, pid+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pid, []byte("new owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveRunDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pid); err != nil {
		t.Fatalf("new entry removed: %v", err)
	}
}

func TestRuntimeDirectoryRemovedBeforeLockRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "phase-a")
	g, err := Acquire(dir, "logical-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.path != filepath.Join(dir, "logical-sandbox.pid") {
		t.Fatalf("PathID layout: %s", g.path)
	}
	if err := g.RemoveRunDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RunDir remains: %v", err)
	}
}

func TestOldExecInheritedPIDLock(t *testing.T) {
	dir := t.TempDir()
	cmd := identityChild("exec", dir, "sandbox")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("same-PID exec: %v %s", err, out)
	}
}

func TestAliasesAndInvalidPIDEntries(t *testing.T) {
	dir := t.TempDir()
	g, err := Acquire(dir, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if h, err := Acquire(alias, "sandbox"); err == nil {
		h.Close()
		t.Fatal("symlink RunDir accepted")
	}
	link := filepath.Join(dir, "other.pid")
	if err := os.Link(g.path, link); err != nil {
		t.Fatal(err)
	}
	if h, err := Acquire(dir, "other"); err == nil {
		h.Close()
		t.Fatal("hard-linked PID accepted")
	}
	probeLock(t, g.path, os.Getpid())
	for _, sid := range []string{"", ".", "..", "../escape", "nested/id"} {
		if h, err := Acquire(dir, sid); err == nil {
			h.Close()
			t.Fatalf("invalid SID %q accepted", sid)
		}
	}
}

func identityChild(mode, dir, sid string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestIdentityChild$")
	env := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "RUNIDENTITY_TEST_") {
			env = append(env, item)
		}
	}
	cmd.Env = append(env, "RUNIDENTITY_TEST_MODE="+mode, "RUNIDENTITY_TEST_DIR="+dir, "RUNIDENTITY_TEST_SID="+sid)
	return cmd
}

func probeLock(t *testing.T, path string, wantPID int) {
	t.Helper()
	cmd := identityChild("probe", filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), ".pid"))
	cmd.Env = append(cmd.Env, "RUNIDENTITY_TEST_OWNER="+strconv.Itoa(wantPID))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe: %v %s", err, out)
	}
}

func TestIdentityChild(t *testing.T) {
	mode := os.Getenv("RUNIDENTITY_TEST_MODE")
	if mode == "" {
		return
	}
	dir, sid := os.Getenv("RUNIDENTITY_TEST_DIR"), os.Getenv("RUNIDENTITY_TEST_SID")
	path := filepath.Join(dir, sid+".pid")
	switch mode {
	case "probe":
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
		if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lock); err != nil {
			t.Fatal(err)
		}
		want, _ := strconv.Atoi(os.Getenv("RUNIDENTITY_TEST_OWNER"))
		if lock.Type == syscall.F_UNLCK || int(lock.Pid) != want {
			t.Fatalf("owner %d, type %d, want %d", lock.Pid, lock.Type, want)
		}
	case "compete":
		if g, err := Acquire(dir, sid); err == nil {
			g.Close()
			t.Fatal("competing process accepted")
		}
	case "exec":
		fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
		if err := syscall.FcntlFlock(uintptr(fd), syscall.F_SETLK, &lock); err != nil {
			t.Fatal(err)
		}
		next := identityChild("inherited", dir, sid)
		if err := syscall.Exec(os.Args[0], next.Args, next.Env); err != nil {
			t.Fatal(err)
		}
	case "inherited":
		g, err := Acquire(dir, sid)
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		probeLock(t, path, os.Getpid())
	default:
		t.Fatal(fmt.Sprintf("unknown helper mode %q", mode))
	}
}

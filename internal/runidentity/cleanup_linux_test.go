package runidentity

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestCleanupRetainsPIDUntilFinalNonRecursiveHandoff(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	g, err := Acquire(dir, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for _, name := range []string{"old-a", "old-b"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "data"), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err = g.removeRunDir(func() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("identity unlinked before old data cleanup: %v", entries)
		}
		// A successor can own the pathname as soon as the old PID entry vanishes.
		// At this point the old owner must never recursively remove files again.
		child := exec.Command(os.Args[0], "-test.run=^TestCleanupSuccessorProcess$")
		child.Env = append(os.Environ(), "RUNIDENTITY_CLEANUP_SUCCESSOR="+dir)
		if out, err := child.CombinedOutput(); err != nil {
			t.Fatalf("successor: %v %s", err, out)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "new-data"))
	if err != nil || string(got) != "successor" {
		t.Fatalf("successor data lost: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sandbox.pid")); err != nil {
		t.Fatalf("successor PID lost: %v", err)
	}
}

func TestCleanupSuccessorProcess(t *testing.T) {
	dir := os.Getenv("RUNIDENTITY_CLEANUP_SUCCESSOR")
	if dir == "" {
		return
	}
	// Model the legacy launcher: it acquires only the existing PID lock before
	// same-PID exec, so it can create a new identity after old PID unlink.
	file, err := os.OpenFile(filepath.Join(dir, "sandbox.pid"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Len: 0}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new-data"), []byte("successor"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryOwnershipRejectsDifferentSandboxIDs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared-path-id")
	owner, err := Acquire(dir, "first-id")
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	marker := filepath.Join(dir, "sandbox.runtime.cfg")
	if err := os.WriteFile(marker, []byte("first configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := Acquire(dir, "second-id"); err == nil {
		duplicate.Close()
		t.Fatal("different SandboxID acquired the same RunDir in-process")
	}
	child := identityChild("compete", dir, "second-id")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("competing ID: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "second-id.pid")); !os.IsNotExist(err) {
		t.Fatalf("competing PID file created: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "first configuration" {
		t.Fatalf("first runtime lost: %q %v", got, err)
	}
	probeLock(t, owner.path, os.Getpid())
}

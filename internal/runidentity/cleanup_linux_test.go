package runidentity

import (
	"os"
	"os/exec"
	"path/filepath"
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
	g, err := Acquire(dir, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := os.WriteFile(filepath.Join(dir, "new-data"), []byte("successor"), 0o600); err != nil {
		t.Fatal(err)
	}
}

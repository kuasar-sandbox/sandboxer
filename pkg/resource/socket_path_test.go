package resource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalSocketPathResolvesParentAliases(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}

	got, err := CanonicalSocketPath(filepath.Join(alias, "missing", "controller.sock"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realDir, "missing", "controller.sock")
	if got != want {
		t.Fatalf("canonical socket = %q, want %q", got, want)
	}
}

func TestCanonicalSocketPathRejectsFinalSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "controller.sock")
	alias := filepath.Join(root, "controller-alias.sock")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalSocketPath(alias); err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("final symlink error = %v", err)
	}
}

func TestCanonicalSocketPathRejectsAbstractAddress(t *testing.T) {
	if _, err := CanonicalSocketPath("@controller"); err == nil {
		t.Fatal("abstract controller address accepted")
	}
}

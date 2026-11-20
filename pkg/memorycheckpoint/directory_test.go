package memorycheckpoint

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/internal/runidentity"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"golang.org/x/sys/unix"
)

func TestHolderIndependentDirectoryLeavesLockedSuccessorUntouched(t *testing.T) {
	p := startRecoveryProcess(t)
	if filepath.Dir(p.record.Holder.Directory.Path) != filepath.Dir(p.runDir) {
		t.Fatal("holder is not a RunDir sibling")
	}
	if err := os.RemoveAll(p.runDir); err != nil {
		t.Fatal(err)
	}
	// The original RunDir is entirely gone while the live holder still serves
	// the exact frozen bytes through its separately guarded namespace.
	d, opts := recoveryOpenOptions(t, p, nil)
	top, err := OpenTop(context.Background(), d, opts)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := top.Reader.ReadAt(b[:], 0); err != nil || b[0] != 0x35 {
		t.Fatalf("pending RAM=%x %v", b[0], err)
	}
	if err := top.Close(); err != nil {
		t.Fatal(err)
	} // Failed successor releases only its reader.
	if _, err := os.Stat(p.record.Holder.SocketPath); err != nil {
		t.Fatal("reader close removed producer service")
	}
	if err := os.Mkdir(p.runDir, 0700); err != nil {
		t.Fatal(err)
	}
	guard, err := openDirectory(p.runDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := unix.Flock(int(guard.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	before, err := guard.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(p.runDir); err != nil || len(entries) != 0 {
		t.Fatalf("successor should be locked before PID: %v %v", entries, err)
	}
	p.kill(t)
	recovery, err := RecoverMemoryRelease(context.Background(), p.path, p.record)
	if err != nil || !recovery.Record.RAMReleased {
		t.Fatalf("release=%+v %v", recovery, err)
	}
	if err := RemoveHolderDirectory(recovery.Record); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p.runDir)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("late holder cleanup replaced successor directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p.runDir, "successor.pid"), []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	if competing, err := runidentity.Acquire(p.runDir, "another"); err == nil {
		competing.Close()
		t.Fatal("late cleanup allowed a second runtime directory owner")
	}
}

func TestHolderDirectoryRefusesReplacementUnknownAndBusy(t *testing.T) {
	for _, mode := range []string{"busy", "replacement", "unknown", "missing-marker-nonempty", "empty-after-marker", "empty-unreleased"} {
		t.Run(mode, func(t *testing.T) {
			p := startRecoveryProcess(t)
			if mode == "busy" {
				if err := RemoveHolderDirectory(p.record); !errors.Is(err, unix.EWOULDBLOCK) {
					t.Fatalf("live holder cleanup=%v", err)
				}
				if _, err := runidentity.Acquire(p.record.Holder.Directory.Path, "collision"); err == nil {
					t.Fatal("Runtime acquired holder directory")
				}
				return
			}
			p.kill(t)
			r, err := RecoverMemoryRelease(context.Background(), p.path, p.record)
			if err != nil {
				t.Fatal(err)
			}
			path := r.Record.Holder.Directory.Path
			switch mode {
			case "replacement":
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				if err := os.WriteFile(filepath.Join(path, "unknown"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-marker-nonempty":
				if err := os.Remove(filepath.Join(path, holderMarkerName)); err != nil {
					t.Fatal(err)
				}
			case "empty-after-marker", "empty-unreleased":
				// Exact cleanup crash interleaving: socket and marker unlinked,
				// producer exits before the final rmdir of this same inode.
				if err := os.Remove(filepath.Join(path, holderSocketName)); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(path, holderMarkerName)); err != nil {
					t.Fatal(err)
				}
				if mode == "empty-unreleased" {
					r.Record.RAMReleased = false
				}
			}
			err = RemoveHolderDirectory(r.Record)
			if mode == "empty-after-marker" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("terminal empty inode was not collected: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("unverified cleanup succeeded")
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("unverified directory was removed")
				}
			}
		})
	}
}

func TestPrepareHolderLongPathAndFullRunDirCleanup(t *testing.T) {
	root := t.TempDir()
	checkpoint := filepath.Join(root, "checkpoint")
	if err := os.Mkdir(checkpoint, 0700); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(root, strings.Repeat("p", 255))
	guard, err := runidentity.Acquire(run, "sid")
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	// A formerly specially preserved entry must now disappear with RunDir.
	if err := os.Mkdir(filepath.Join(run, "memory-holders"), 0700); err != nil {
		t.Fatal(err)
	}
	m, err := memory.Create("long-path-holder", 4096)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := ReadOnlyMemfd(m.FD())
	if err != nil {
		t.Fatal(err)
	}
	h, err := PrepareHolder(checkpoint, run, CaptureRecord{Version: Version, Producer: RunIdentity{SandboxID: "sid"}, CheckpointID: "cp", MemoryID: "memory", Epoch: 1}, fd, 4096, t.Logf)
	if err != nil {
		m.Close()
		t.Fatal(err)
	}
	if len(filepath.Base(h.Record().Read().Holder.Directory.Path)) > 64 {
		t.Fatal("holder appended the long PathID")
	}
	if err := guard.RemoveRunDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(run); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary RunDir was preserved: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Record().Update(func(r *CaptureRecord) {
		r.Committed = true
		r.RuntimeRetired = true
		r.SnapshotRef = "s"
		r.SandboxRef = "e"
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(func(context.Context, io.ReaderAt) (string, error) { return "payload", nil }, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareHolderFailureOwnsFDAndGCPin(t *testing.T) {
	for _, failure := range []string{"missing-run", "locator-publication", "parent-permissions"} {
		t.Run(failure, func(t *testing.T) {
			if failure == "parent-permissions" && os.Geteuid() == 0 {
				t.Skip("requires ordinary service UID")
			}
			root := t.TempDir()
			checkpoint := filepath.Join(root, "checkpoint")
			runRoot := filepath.Join(root, "runs")
			run := filepath.Join(runRoot, "run")
			for _, p := range []string{checkpoint, run} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			switch failure {
			case "missing-run":
				if err := os.Remove(run); err != nil {
					t.Fatal(err)
				}
			case "locator-publication": // Introduced after the initial absence check below.
			case "parent-permissions":
				if err := os.Chmod(runRoot, 0500); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(runRoot, 0700)
			}
			m, err := memory.Create("failed-prepare", 4096)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			fd, err := ReadOnlyMemfd(m.FD())
			if err != nil {
				t.Fatal(err)
			}
			var beforePublish func()
			if failure == "locator-publication" {
				beforePublish = func() {
					if err := os.Mkdir(filepath.Join(checkpoint, "current-memory.json"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if h, err := prepareHolder(checkpoint, run, CaptureRecord{Version: Version, Producer: RunIdentity{SandboxID: "sid"}, CheckpointID: "cp", MemoryID: "memory", Epoch: 1}, fd, 4096, t.Logf, beforePublish); err == nil || h != nil {
				t.Fatal("bad preparation succeeded")
			}
			if failure == "locator-publication" {
				r, err := ReadRecord(checkpoint, "cp")
				if err != nil || !r.Aborted {
					t.Fatalf("published preparation lost abort ownership: %+v %v", r, err)
				}
			}
			if _, err := fd.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("failed preparation retained RAM FD: %v", err)
			}
			pin, err := ExclusiveDirectory(checkpoint)
			if err != nil {
				t.Fatalf("failed preparation leaked GC pin: %v", err)
			}
			pin.Close()
			entries, err := os.ReadDir(runRoot)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".km-") {
					t.Fatal("failed preparation leaked its namespace")
				}
			}
		})
	}
}

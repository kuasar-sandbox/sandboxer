package memorycheckpoint

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"golang.org/x/sys/unix"
)

func attemptHolder(t *testing.T, directory, run, id string) *Holder {
	t.Helper()
	m, err := memory.Create("attempt", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	fd, err := ReadOnlyMemfd(m.FD())
	if err != nil {
		t.Fatal(err)
	}
	h, err := PrepareHolder(directory, run, CaptureRecord{Version: Version, Producer: RunIdentity{SandboxID: "sid", InstanceID: "instance", RunID: "run"}, CheckpointID: "same", MemoryID: id, Epoch: 1}, fd, 4096, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestAbortedCaptureRetryPreservesExactProof(t *testing.T) {
	directory, run := t.TempDir(), t.TempDir()
	first := attemptHolder(t, directory, run, "first")
	if err := first.Abort(); err != nil {
		t.Fatal(err)
	}
	old := first.Record().Read()
	oldPath := first.Record().Path()
	oldJSON, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	second := attemptHolder(t, directory, run, "second")
	defer second.Abort()
	next := second.Record().Read()
	if next.Epoch != old.Epoch+1 || next.MemoryID == old.MemoryID || second.Record().Path() == oldPath {
		t.Fatalf("retry aliases original: old=%+v next=%+v", old, next)
	}
	if got, err := os.ReadFile(oldPath); err != nil || !bytes.Equal(oldJSON, got) {
		t.Fatalf("old receipt changed: %v", err)
	}
	proof, err := openRAMPin(oldPath, old.Holder.RAMLock, unix.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	proof.Close()
	recovered, err := RecoverMemoryRelease(context.Background(), oldPath, old)
	if err != nil || !sameCaptureIdentity(recovered.Record, old) {
		t.Fatalf("old receipt no longer resolvable: %+v %v", recovered, err)
	}
	current, err := ReadCurrent(directory)
	if err != nil || !sameCaptureIdentity(*current, next) {
		t.Fatalf("current did not select new attempt: %+v %v", current, err)
	}
	records, err := ListRecords(directory)
	if err != nil || len(records) != 2 {
		t.Fatalf("attempt enumeration: %v %v", records, err)
	}
	// A stale locator must select its exact old attempt, never the newer one.
	if err := writeJSON(filepath.Join(directory, "current-memory.json"), currentLocator{old.CheckpointID, old.MemoryID, old.Epoch}); err != nil {
		t.Fatal(err)
	}
	current, err = ReadCurrent(directory)
	if err != nil || !sameCaptureIdentity(*current, old) {
		t.Fatalf("stale locator changed generation: %+v %v", current, err)
	}
}

func TestPreparationGuardExcludesLateHolder(t *testing.T) {
	directory, run := t.TempDir(), t.TempDir()
	guard, err := LockPreparation(directory, "same")
	if err != nil {
		t.Fatal(err)
	}
	m, err := memory.Create("attempt", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	fd, err := ReadOnlyMemfd(m.FD())
	if err != nil {
		t.Fatal(err)
	}
	_, err = PrepareHolder(directory, run, CaptureRecord{Version: Version, Producer: RunIdentity{SandboxID: "sid"}, CheckpointID: "same", MemoryID: "blocked", Epoch: 1}, fd, 4096, t.Logf)
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("late prepare admitted: %v", err)
	}
	if _, err := fd.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed preparation kept RAM: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	h := attemptHolder(t, directory, run, "admitted")
	if err := h.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryRejectsCommittedOrReusedAttempt(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "reused-id", true: "committed-id"}[committed], func(t *testing.T) {
			dir := t.TempDir()
			w := testRecord(t, dir)
			if committed {
				if err := w.Update(func(r *CaptureRecord) {
					r.Committed = true
					r.RuntimeRetired = true
					r.FileReady = true
					r.RAMReleased = true
					r.SnapshotRef = "s"
					r.SandboxRef = "e"
					r.PayloadRef = "p"
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := w.Update(func(r *CaptureRecord) { r.Aborted = true }); err != nil {
				t.Fatal(err)
			}
			old := w.Read()
			w.Close()
			next := old
			next.Committed = false
			next.RuntimeRetired = false
			next.FileReady = false
			next.RAMReleased = false
			next.Aborted = false
			next.SnapshotRef = ""
			next.SandboxRef = ""
			next.PayloadRef = ""
			if committed {
				next.MemoryID = "new"
			}
			if got, err := reserveRecord(dir, next); err == nil {
				got.Close()
				t.Fatal("reused identity admitted")
			}
		})
	}
}

func TestRAMPinNonceRejectsRecycledInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempt.json")
	first, old, err := createRAMPin(path)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	if err := os.Remove(path + ".ram.lock"); err != nil {
		t.Fatal(err)
	}
	second, replacement, err := createRAMPin(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	// Match the replacement's device/inode to exercise inode recycling even
	// on filesystems that do not immediately reuse the just-deleted inode.
	old.Device, old.Inode = replacement.Device, replacement.Inode
	if err := validateRAMPin(second, old); err == nil {
		t.Fatal("recycled inode manufactured RAM release proof")
	}
	if err := validateRAMPin(second, replacement); err != nil {
		t.Fatal(err)
	}
	missing := replacement
	missing.Nonce = ""
	if err := validateRAMPin(second, missing); err == nil {
		t.Fatal("missing nonce manufactured release proof")
	}
}

func TestExistingDirectoryPinDoesNotCreateProtocol(t *testing.T) {
	directory := t.TempDir()
	if pin, err := PinExistingDirectory(directory); !errors.Is(err, os.ErrNotExist) {
		if pin != nil {
			pin.Close()
		}
		t.Fatalf("missing protocol accepted: %v", err)
	}
	pin, err := PinDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	pin.Close()
	borrowed, err := PinExistingDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer borrowed.Close()
	if exclusive, err := ExclusiveDirectory(directory); !errors.Is(err, unix.EWOULDBLOCK) {
		if exclusive != nil {
			exclusive.Close()
		}
		t.Fatalf("GC ignored existing pin: %v", err)
	}
}

func TestPreparationReadsOnlyItsCheckpointAttempts(t *testing.T) {
	directory, run := t.TempDir(), t.TempDir()
	first := attemptHolder(t, directory, run, "first")
	if err := first.Abort(); err != nil {
		t.Fatal(err)
	}
	unrelated, err := RecordPath(directory, "unrelated", "broken")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(unrelated), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, []byte("broken unrelated history"), 0600); err != nil {
		t.Fatal(err)
	}
	second := attemptHolder(t, directory, run, "second")
	defer second.Abort()
	if record, err := ReadRecord(directory, "same"); err != nil || record.MemoryID != "second" {
		t.Fatalf("unrelated history affected target lookup: %+v %v", record, err)
	}
	if _, err := ListRecords(directory); err == nil {
		t.Fatal("full inventory hid corrupt history")
	}
}

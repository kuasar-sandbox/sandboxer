package memorycheckpoint

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"golang.org/x/sys/unix"
)

func blockAbortMetadata(t *testing.T, directory string) (string, func()) {
	t.Helper()
	metadata, hidden := filepath.Join(directory, "memory"), filepath.Join(directory, "memory-hidden")
	if err := os.Rename(metadata, hidden); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, nil, 0600); err != nil {
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		if err := os.Remove(metadata); err != nil {
			t.Error(err)
		}
		if err := os.Rename(hidden, metadata); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(restore)
	return hidden, restore
}

func assertAbortLocksHeld(t *testing.T, directory, lockPath string) {
	t.Helper()
	if exclusive, err := ExclusiveDirectory(directory); err == nil {
		exclusive.Close()
		t.Fatal("unknown abort released its directory pin")
	} else if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("unknown abort released its stable record lock: %v", err)
	}
}

func TestAbortRecordRetainsMetadataOwnershipUntilPublication(t *testing.T) {
	directory := t.TempDir()
	record := testRecord(t, directory)
	pin, err := PinDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	lockPath, _ := preparationLockPath(directory, record.Read().CheckpointID)
	hidden, restore := blockAbortMetadata(t, directory)
	var completions atomic.Int32
	done := make(chan struct{})
	complete := func() error {
		completions.Add(1)
		err := pin.Close()
		close(done)
		return err
	}
	for i := 0; i < 2; i++ {
		if err := AbortRecord(record, complete, t.Logf); err == nil {
			t.Fatal("blocked metadata abort reported success")
		}
	}
	if record.Read().Aborted || completions.Load() != 0 {
		t.Fatal("failed abort fabricated completion")
	}
	assertAbortLocksHeld(t, directory, filepath.Join(hidden, filepath.Base(lockPath)))
	restore()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("metadata abort did not finish after storage recovered")
	}
	_ = AbortRecord(record, complete, t.Logf)
	if completions.Load() != 1 {
		t.Fatalf("repeated abort repeated cleanup: count=%d", completions.Load())
	}
	current, err := ReadCurrent(directory)
	if err != nil || !current.Aborted {
		t.Fatalf("abort publication=%+v %v", current, err)
	}
	exclusive, err := ExclusiveDirectory(directory)
	if err != nil {
		t.Fatalf("finished abort retained directory pin: %v", err)
	}
	exclusive.Close()
}

func TestHolderAbortClosesRAMBeforeMetadataRecovery(t *testing.T) {
	for _, postRename := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory-unavailable", true: "rename-published-sync-failed"}[postRename], func(t *testing.T) {
			directory := t.TempDir()
			m, err := memory.Create("abort-holder", 4096)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			m.Bytes()[0] = 0x35
			fd, err := ReadOnlyMemfd(m.FD())
			if err != nil {
				t.Fatal(err)
			}
			holder := testPrepareHolder(t, directory, fd, 4096)
			record := holder.Record()
			pin, err := PinDirectory(directory)
			if err != nil {
				t.Fatal(err)
			}
			lockPath, _ := preparationLockPath(directory, record.Read().CheckpointID)
			var restore func()
			if postRename {
				// The first real atomic write succeeds, then its post-rename
				// directory-sync failpoint reports uncertainty to Update. Later
				// publication stays blocked until this test releases recovery.
				var calls atomic.Int32
				var recovered atomic.Bool
				record.mu.Lock()
				record.writeJSON = func(path string, value any) error {
					if calls.Add(1) == 1 {
						if err := writeJSON(path, value); err != nil {
							return err
						}
						return &recordPublishedError{errors.New("injected directory sync failure after rename")}
					}
					if !recovered.Load() {
						return errors.New("metadata recovery not released")
					}
					return writeJSON(path, value)
				}
				record.mu.Unlock()
				restore = func() { recovered.Store(true) }
				t.Cleanup(restore)
			} else {
				var hidden string
				hidden, restore = blockAbortMetadata(t, directory)
				lockPath = filepath.Join(hidden, filepath.Base(lockPath))
			}
			var completions atomic.Int32
			complete := func() error { completions.Add(1); return pin.Close() }
			if err := holder.AbortWithCleanup(complete); err == nil {
				t.Fatal("injected abort publication failure was hidden")
			}
			if record.Read().Aborted != postRename {
				t.Fatalf("published abort fact=%v, want %v", record.Read().Aborted, postRename)
			}
			if _, err := fd.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("failed abort retained RAM fd: %v", err)
			}
			if _, err := holder.listener.AcceptUnix(); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("failed abort retained listener: %v", err)
			}
			if _, err := os.Stat(record.Read().Holder.SocketPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed abort retained socket: %v", err)
			}
			// The original running VM retains its own mapping and can continue.
			m.Bytes()[0] = 0x73
			if m.Bytes()[0] != 0x73 {
				t.Fatal("abort changed the original VM mapping")
			}
			select {
			case <-holder.Done():
				t.Fatal("metadata failure completed holder ownership early")
			default:
			}
			assertAbortLocksHeld(t, directory, lockPath)
			_ = holder.AbortWithCleanup(complete)
			restore()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := holder.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			holder.finish(nil) // ordinary completion also remains once-only
			if completions.Load() != 1 || !record.Read().Aborted {
				t.Fatalf("abort completion=%d record=%+v", completions.Load(), record.Read())
			}
			exclusive, err := ExclusiveDirectory(directory)
			if err != nil {
				t.Fatalf("completed abort kept directory pin: %v", err)
			}
			exclusive.Close()
		})
	}
}

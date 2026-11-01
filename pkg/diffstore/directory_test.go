package diffstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDirectoryMaintenanceIgnoresInitializerClaims(t *testing.T) {
	owner, m := directoryManager(t, nil)
	directoryFixture(t, owner.Directory, "disk", false)
	claim := filepath.Join(owner.Directory, "diffs", ".claim-disk.diff")
	f, err := os.OpenFile(claim, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, held := range []bool{true, false} {
		op := unix.LOCK_UN
		if held {
			op = unix.LOCK_EX | unix.LOCK_NB
		}
		if err := unix.Flock(int(f.Fd()), op); err != nil {
			t.Fatal(err)
		}
		count := 0
		if err := m.ListCheckpoints(context.Background(), func(id string, ids []string) error {
			count++
			if id != "disk" || len(ids) != 1 || ids[0] != "old" {
				t.Fatalf("claim mixed into versions: %s %v", id, ids)
			}
			return nil
		}); err != nil || count != 1 {
			t.Fatalf("claim held=%v list count=%d: %v", held, count, err)
		}
		if err := m.Sweep(context.Background(), nil); err != nil {
			t.Fatalf("claim held=%v sweep: %v", held, err)
		}
		if _, err := os.Stat(claim); err != nil {
			t.Fatal("enumeration removed initializer ownership", err)
		}
	}
	if err := os.WriteFile(filepath.Join(owner.Directory, "diffs", "broken.diff"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.ListCheckpoints(context.Background(), func(string, []string) error { return nil }); err == nil {
		t.Fatal("a corrupt real DIFF was silently ignored")
	}
}

func directoryFixture(t *testing.T, directory, id string, encrypted bool) *View {
	t.Helper()
	opts := Options{DiffID: id, LogicalSize: 256 * PageSize}
	if encrypted {
		opts.Key = &[32]byte{42}
	}
	s, err := Create(filepath.Join(directory, "diffs", id+".diff"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data := bytes.Repeat([]byte{0x35}, 256*PageSize)
	if _, err = s.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.Checkpoint(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Export(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	if _, err = s.WriteAt(bytes.Repeat([]byte{0x64}, len(data)), 0); err != nil {
		t.Fatal(err)
	}
	return v
}

func directoryManager(t *testing.T, active []string) (*DirectoryOwner, *DirectoryMaintenance) {
	t.Helper()
	owner, err := LockDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	m, err := NewDirectoryMaintenance(owner, active)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return owner, m
}

func TestDirectoryMaintenanceSharedHandlesAndReservedManagementCapacity(t *testing.T) {
	owner, m := directoryManager(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range []string{"one", "two", "admin"} {
		directoryFixture(t, owner.Directory, id, false)
	}
	// Both GC slots are occupied. Admin requests on these files must use their
	// existing writer; another file uses the separately reserved transient slot.
	if err := m.call(ctx, func(handles map[string]*Store) error {
		for _, id := range []string{"one", "two"} {
			s, e := m.open(id)
			if e != nil {
				return e
			}
			handles[id] = s
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "admin"} {
		if err := m.DeleteCheckpoints(ctx, id, []string{"old"}); err != nil {
			t.Fatalf("management at full GC capacity: %v", err)
		}
	}
	if err := m.ListCheckpoints(ctx, func(_ string, ids []string) error {
		if len(ids) != 0 {
			return fmt.Errorf("deleted versions visible: %v", ids)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.call(ctx, func(handles map[string]*Store) error {
		if len(handles) != directoryGCHandles {
			return fmt.Errorf("management changed GC handle budget: %d", len(handles))
		}
		for _, s := range handles {
			if s.Stats().GCPunchCalls != 0 {
				return errors.New("management ran GC")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectorySweepReportsBlockedErrorsRetiredAndContinues(t *testing.T) {
	owner, m := directoryManager(t, []string{"active"})
	ctx := context.Background()
	active, err := Create(filepath.Join(owner.Directory, "diffs", "active.diff"), Options{DiffID: "active", LogicalSize: PageSize})
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	blocked := directoryFixture(t, owner.Directory, "blocked", true)
	released := directoryFixture(t, owner.Directory, "released", false)
	retired := directoryFixture(t, owner.Directory, "retired", false)
	defer retired.Close()
	meta, err := OpenMetadata(filepath.Join(owner.Directory, "diffs", "retired.diff"))
	if err != nil {
		t.Fatal(err)
	}
	if err = meta.Retire(ctx); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err = meta.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"blocked", "released"} {
		if err = m.DeleteCheckpoints(ctx, id, []string{"old"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = released.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(owner.Directory, "diffs", "broken.diff"), []byte("bad header"), 0600); err != nil {
		t.Fatal(err)
	}
	results := map[string]DirectoryGCResult{}
	err = m.Sweep(ctx, func(r DirectoryGCResult) { results[r.DiffID] = r })
	if err == nil || results["broken"].State != "error" {
		t.Fatalf("missing error: %v %+v", err, results)
	}
	if _, ok := results["active"]; ok {
		t.Fatal("active file acquired by second writer")
	}
	if r := results["blocked"]; r.State != string(BlockedByReaders) || r.FreedSlots != 0 {
		t.Fatal(r)
	}
	if r := results["released"]; r.State != string(Complete) || r.FreedSlots == 0 {
		t.Fatal(r)
	}
	if r := results["retired"]; r.State != "skipped_retired" {
		t.Fatal(r)
	}
	if _, err = os.Stat(filepath.Join(owner.Directory, "diffs", "retired.diff")); err != nil {
		t.Fatal("GC removed retired file", err)
	}
	b := make([]byte, PageSize)
	if _, err = blocked.ReadAt(ctx, b, 0); err != nil || !bytes.Equal(b, bytes.Repeat([]byte{0x35}, PageSize)) {
		t.Fatal("reader lost data", err)
	}
	if err = blocked.Close(); err != nil {
		t.Fatal(err)
	}
	results = map[string]DirectoryGCResult{}
	_ = m.Sweep(ctx, func(r DirectoryGCResult) { results[r.DiffID] = r })
	if r := results["blocked"]; r.State != string(Complete) || r.FreedSlots == 0 {
		t.Fatal("no reclaim after reader closed", r)
	}
}

func TestDirectoryMaintenancePeriodicRetryAndBorrowedOwner(t *testing.T) {
	owner, m := directoryManager(t, nil)
	v := directoryFixture(t, owner.Directory, "history", false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.DeleteCheckpoints(ctx, "history", []string{"old"}); err != nil {
		t.Fatal(err)
	}
	results := make(chan DirectoryGCResult, 16)
	if err := m.Start(ctx, func(r DirectoryGCResult) { results <- r }); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		if r.State != string(BlockedByReaders) {
			t.Fatal(r)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		if r.State != string(Complete) || r.FreedSlots == 0 {
			t.Fatal(r)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if second, err := LockDirectory(owner.Directory); !errors.Is(err, ErrBusy) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("borrower released owner: %v", err)
	}
	if err := m.DeleteCheckpoints(ctx, "history", []string{"old"}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed admission: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := LockDirectory(owner.Directory)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}

func TestDirectoryCloseWaitsAcceptedOperation(t *testing.T) {
	owner, m := directoryManager(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	operation := make(chan error, 1)
	go func() {
		operation <- m.call(context.Background(), func(map[string]*Store) error { close(entered); <-release; return nil })
	}()
	awaitSignal(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	awaitSignal(t, m.stop)
	select {
	case err := <-closed:
		t.Fatalf("closed during operation: %v", err)
	default:
	}
	if err := owner.Validate(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := awaitError(t, operation); err != nil {
		t.Fatal(err)
	}
	if err := awaitError(t, closed); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryIncrementalEnumerationAndOwnerIdentity(t *testing.T) {
	owner, m := directoryManager(t, nil)
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(owner.Directory, "diffs", fmt.Sprintf("%04d.diff", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	it, err := m.iterator()
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	if _, err = it.next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(it.entries) > 32 {
		t.Fatal("unbounded directory work set")
	}
	lock := filepath.Join(owner.Directory, "diffs", ".owner.lock")
	if err = os.Rename(lock, lock+".old"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.Sweep(context.Background(), nil); err == nil {
		t.Fatal("accepted replaced lock inode")
	}
}

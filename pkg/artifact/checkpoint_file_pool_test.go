package artifact

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCheckpointHistoryBoundedDescriptors(t *testing.T) {
	if os.Getenv("KUASAR_CHECKPOINT_FD_CHILD") != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestCheckpointHistoryBoundedDescriptors$", "-test.v")
		cmd.Env = append(os.Environ(), "KUASAR_CHECKPOINT_FD_CHILD=1")
		out, err := cmd.CombinedOutput()
		t.Log(string(out))
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	limit.Cur = min(limit.Max, 128)
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	fixture := newMemoryCheckpointFixture(t)
	var value string
	for i := 0; i < 96; i++ {
		value = strings.Repeat(string(byte('a'+i%26)), 6)
		fixture.ref, fixture.path, fixture.descriptor = writeLocalMemory(t, fixture.directory, fmt.Sprintf("data-%d", i), fixture.eRef,
			[]string{fixture.ref}, []string{fixture.external}, value, 0x3f, 0x3f, 0, false)
	}
	before := count()
	root := openLocalFixture(t, fixture)
	during := count()
	if during-before > 20 {
		t.Fatalf("96 layers retained %d descriptors", during-before)
	}
	assertMemoryPages(t, root.Memory, "DDDDDD", value)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	after := count()
	if after > before {
		t.Fatalf("Close retained descriptors: %d -> %d", before, after)
	}
	t.Logf("96 real-data layers, RLIMIT=%d, fd before/open/closed=%d/%d/%d", limit.Cur, before, during, after)
}

func TestCheckpointFilePoolReopenRejectsReplacement(t *testing.T) {
	dir := t.TempDir()
	p := newCheckpointFilePool()
	defer p.Close()
	readers := make([]*checkpointFileReader, checkpointFileSlots+1)
	for i := range readers {
		path := filepath.Join(dir, fmt.Sprint(i))
		if err := os.WriteFile(path, bytes.Repeat([]byte{byte(i)}, 4096), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := p.source(path)
		if err != nil {
			t.Fatal(err)
		}
		readers[i] = r
		var b [1]byte
		if _, err := r.ReadAt(b[:], 0); err != nil {
			t.Fatal(err)
		}
	}
	path := readers[0].path
	// Retain the old inode at another path so replacement cannot reuse it.
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{0}, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := readers[0].ReadAt(b[:], 0); err == nil {
		t.Fatal("reopened replacement carrier")
	}
}

func TestCheckpointFilePoolConcurrentEviction(t *testing.T) {
	dir := t.TempDir()
	p := newCheckpointFilePool()
	defer p.Close()
	readers := make([]*checkpointFileReader, 24)
	for i := range readers {
		path := filepath.Join(dir, fmt.Sprint(i))
		if err := os.WriteFile(path, bytes.Repeat([]byte{byte(i)}, 4096), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := p.source(path)
		if err != nil {
			t.Fatal(err)
		}
		readers[i] = r
	}
	var wg sync.WaitGroup
	for i, r := range readers {
		wg.Add(1)
		go func(i int, r *checkpointFileReader) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				b := make([]byte, 64)
				if got, err := r.ReadAt(b, int64(n)); err != nil || got != len(b) || !bytes.Equal(b, bytes.Repeat([]byte{byte(i)}, len(b))) {
					t.Errorf("read %d: n=%d err=%v", i, got, err)
					return
				}
			}
		}(i, r)
	}
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if len(p.entries) != 0 {
		t.Fatal("pool retained descriptors")
	}
	var b [1]byte
	if _, err := readers[0].ReadAt(b[:], 0); err == nil {
		t.Fatal("read after Close")
	}
}

func TestCheckpointHistoryChangedSuffixDoesNotReintroduceCleanPages(t *testing.T) {
	f := newMemoryCheckpointFixture(t)
	// A dirty-only generation intentionally drops its parent's clean W copies.
	f.ref, f.path, f.descriptor = writeLocalMemory(t, f.directory, "drop-clean", f.eRef, []string{f.ref}, []string{f.external}, "xxxxxx", 0, 0, 0, false)
	// Changing the outer suffix must preserve that already-filtered expression.
	other := "manifest://" + strings.Repeat("e", 64)
	f.ref, f.path, f.descriptor = writeLocalMemory(t, f.directory, "different-suffix", f.eRef, []string{f.ref}, []string{other}, "xxxxxx", 0, 0, 0, false)
	root := openLocalFixture(t, f)
	defer root.Close()
	assertMemoryPages(t, root.Memory, "DDZHZH", "bc\x00\x00\x00\x00")
	if _, err := SnapshotDirtyMemory(root); err != nil {
		t.Fatal(err)
	}
}

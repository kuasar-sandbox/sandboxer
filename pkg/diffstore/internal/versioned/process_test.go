package versioned

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
	"golang.org/x/sys/unix"
)

// The subprocess deliberately opens independent file descriptions. A unit test
// that only shares the owner's description cannot validate OFD exclusion.
func TestProcessHelper(t *testing.T) {
	mode := os.Getenv("DIFFSTORE_HELPER")
	if mode == "" {
		return
	}
	path := os.Getenv("DIFFSTORE_PATH")
	switch mode {
	case "reader":
		v, err := OpenExport(context.Background(), path, "pinned", nil)
		if err != nil {
			fmt.Println("ERROR", err)
			os.Exit(2)
		}
		defer v.Close()
		fmt.Println("READY")
		scan := bufio.NewScanner(os.Stdin)
		for scan.Scan() {
			if scan.Text() == "read" {
				b := make([]byte, PageSize)
				_, err := v.ReadAt(ctx, b, PageSize)
				if err != nil || !bytes.Equal(b, pageFor(1, 1)) {
					fmt.Println("CORRUPT", err)
					os.Exit(3)
				}
				fmt.Println("STABLE")
			}
		}
	case "owner":
		s, err := Open(path, Options{})
		if errors.Is(err, ErrBusy) {
			fmt.Println("BUSY")
			return
		}
		if err != nil {
			fmt.Println("ERROR", err)
			os.Exit(4)
		}
		s.Close()
		fmt.Println("OWNED")
	case "crash-write":
		s, err := Open(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		stage := os.Getenv("DIFFSTORE_CRASH_STAGE")
		if err = s.MarkModified(ctx); err != nil {
			t.Fatal(err)
		}
		seen := 0
		s.mu.Lock()
		s.beforeIO = func(actual string) error {
			if actual == stage {
				seen++
				if seen == 1 {
					os.Exit(87)
				}
			}
			return nil
		}
		s.mu.Unlock()
		err = s.WritePages(ctx, []Page{{1, pageFor(1, 2)}, {17, pageFor(17, 2)}})
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		os.Exit(88)
	case "crash-metadata":
		s, err := Open(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		stopTestGC(s)
		stage := os.Getenv("DIFFSTORE_CRASH_STAGE")
		s.beforeIO = func(actual string) error {
			if actual == stage {
				os.Exit(87)
			}
			return nil
		}
		switch os.Getenv("DIFFSTORE_CRASH_OPERATION") {
		case "create":
			err = s.Checkpoint(ctx, "created")
		case "delete":
			err = s.DeleteCheckpoint(ctx, "target")
		case "gc":
			_, err = s.GC(ctx, 64)
		default:
			t.Fatal("unknown metadata crash operation")
		}
		if err != nil {
			t.Fatal(err)
		}
		os.Exit(88)
	case "short-write":
		f, err := openData(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var limits unix.Rlimit
		if err = unix.Getrlimit(unix.RLIMIT_FSIZE, &limits); err != nil {
			t.Fatal(err)
		}
		limits.Cur = PageSize + 512
		if err = unix.Setrlimit(unix.RLIMIT_FSIZE, &limits); err != nil {
			t.Fatal(err)
		}
		err = directWrite(f, bytes.Repeat([]byte{0x61}, 2*PageSize), 0)
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("expected aligned partial kernel write, got %v", err)
		}
		st, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() != PageSize+512 {
			t.Fatalf("short-write byte count: %d", st.Size())
		}
		fmt.Println("SHORT_WITHOUT_RETRY")
	case "short-initial-presence", "short-initial-presence-tail":
		f, err := openData(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var limits unix.Rlimit
		if err = unix.Getrlimit(unix.RLIMIT_FSIZE, &limits); err != nil {
			t.Fatal(err)
		}
		limits.Cur = presenceAreaPage*PageSize + PageSize + 512
		if mode == "short-initial-presence-tail" {
			limits.Cur = (presenceAreaPage+uint64(presence.Pages(presence.MaxSize))-1)*PageSize + 512
		}
		if err = unix.Setrlimit(unix.RLIMIT_FSIZE, &limits); err != nil {
			t.Fatal(err)
		}
		err = writeInitialPresence(f, make([]byte, presence.Bytes(presence.MaxSize)), presence.MaxSize)
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("expected actual short initialization write, got %v", err)
		}
		info, err := f.Stat()
		if err != nil || uint64(info.Size()) != limits.Cur {
			t.Fatalf("unexpected short write extent: %v %v", info, err)
		}
		fmt.Println("PRESENCE_SHORT_WITHOUT_RETRY")
	case "short-engine-write":
		s, err := Open(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		stopTestGC(s)
		if err = s.MarkModified(ctx); err != nil {
			t.Fatal(err)
		}
		// Allocation reservation completes first. Limit the first newly allocated
		// payload write to a real 512-byte short syscall, while the old version's
		// data remains at its earlier physical slot.
		limit := slotPage(s.ctrl.Next)*PageSize + 512
		s.beforeIO = func(stage string) error {
			if stage != "data" {
				return nil
			}
			var limits unix.Rlimit
			if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limits); err != nil {
				return err
			}
			limits.Cur = limit
			return unix.Setrlimit(unix.RLIMIT_FSIZE, &limits)
		}
		err = s.WritePages(ctx, []Page{{1, pageFor(1, 2)}})
		if !errors.Is(err, io.ErrShortWrite) || !errors.Is(s.Err(), io.ErrShortWrite) {
			t.Fatalf("engine did not retain real short-write failure: %v / %v", err, s.Err())
		}
		s.Close()
		fmt.Println("ENGINE_SHORT_WITHOUT_LOGICAL_PUNCH")
	}
}

// Crash-boundary fixtures explicitly control GC so that a ticker cannot consume
// the intended interruption point while the child process is being launched.
func stopTestGC(s *Store) {
	s.mu.Lock()
	stop, done := s.stop, s.done
	if stop != nil {
		close(stop)
	}
	s.mu.Unlock()
	if stop != nil {
		<-done
		s.mu.Lock()
		s.stop = nil
		s.mu.Unlock()
	}
}

func TestKilledMetadataTransactionsReopen(t *testing.T) {
	for _, operation := range []string{"create", "delete", "gc"} {
		stages := []string{"redo-reset", "redo-index", "redo-images", "redo-commit", "apply", "redo-clear"}
		if operation == "create" {
			stages = append(stages, "new-index")
		} else if operation == "gc" {
			stages = append(stages, "gc-punch", "gc-punched")
		}
		for _, stage := range stages {
			t.Run(operation+"/"+stage, func(t *testing.T) {
				s, path := newTestStore(t, 64, nil)
				stopTestGC(s)
				blocks := []uint64{1, 2, 17, 44}
				putPages(t, s, blocks, 1)
				if err := s.Checkpoint(ctx, "target"); err != nil {
					t.Fatal(err)
				}
				putPages(t, s, blocks, 2)
				if err := s.Checkpoint(ctx, "stable"); err != nil {
					t.Fatal(err)
				}
				want := map[uint64][]byte{}
				for _, block := range blocks {
					want[block] = pageFor(block, 2)
				}
				if operation == "gc" {
					if err := s.DeleteCheckpoint(ctx, "target"); err != nil {
						t.Fatal(err)
					}
					if _, err := s.GC(ctx, 64); err != nil {
						t.Fatal(err)
					}
					if s.ctrl.Garbage == 0 {
						t.Fatal("GC fixture has no reclaimable garbage")
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				cmd := helperCommand(t, "crash-metadata", path)
				cmd.Env = append(cmd.Env, "DIFFSTORE_CRASH_STAGE="+stage, "DIFFSTORE_CRASH_OPERATION="+operation)
				out, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 87 {
					t.Fatalf("did not interrupt metadata boundary: %s %v", out, err)
				}
				// Fixed export reads a committed redo overlay without a writer. Its
				// entire historical payload must survive every metadata boundary.
				v, err := OpenExport(ctx, path, "stable", nil)
				if err != nil {
					t.Fatal(err)
				}
				checkView(t, v, want)
				v.Close()
				s, err = Open(path, Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				stopTestGC(s)
				checkActive(t, s, want)
				ids, err := s.ListCheckpoints(ctx)
				if err != nil {
					t.Fatal(err)
				}
				has := func(id string) bool {
					for _, found := range ids {
						if found == id {
							return true
						}
					}
					return false
				}
				committed := stage == "apply" || stage == "redo-clear"
				if operation == "create" && has("created") != committed || operation == "delete" && has("target") == committed {
					t.Fatalf("wrong replayed visibility at %s: %v", stage, ids)
				}
				if operation == "gc" {
					for i := 0; i < 4; i++ {
						if _, err = s.GC(ctx, 64); err != nil {
							t.Fatal(err)
						}
					}
					before := s.Stats()
					putPages(t, s, []uint64{8, 9, 10}, 3)
					if s.Stats().Reused <= before.Reused {
						t.Fatal("resumed GC did not make punched slots reusable")
					}
				}
			})
		}
	}
}

func TestActualDirectShortWrite(t *testing.T) {
	cmd := helperCommand(t, "short-write", filepath.Join(t.TempDir(), "short.diff"))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "SHORT_WITHOUT_RETRY") {
		t.Fatalf("short syscall: %s %v", out, err)
	}
}

func TestActualEngineShortWritePreservesLogicalControlAndVersion(t *testing.T) {
	s, path := newTestStore(t, 64, nil)
	putPages(t, s, []uint64{1, 17}, 1)
	if err := s.Checkpoint(ctx, "safe"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := helperCommand(t, "short-engine-write", path)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ENGINE_SHORT_WITHOUT_LOGICAL_PUNCH") {
		t.Fatalf("short engine syscall: %s %v", out, err)
	}
	// Logical block 1 is physical control page 1, not a data address. Incorrect
	// legacy cleanup at guest offsets would make this reopen fail immediately.
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := map[uint64][]byte{1: pageFor(1, 1), 17: pageFor(17, 1)}
	checkActive(t, s, want)
	v, err := s.Export(ctx, "safe")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	checkView(t, v, want)
}
func helperCommand(t *testing.T, mode, path string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestProcessHelper$", "-test.v=false")
	cmd.Env = append(os.Environ(), "DIFFSTORE_HELPER="+mode, "DIFFSTORE_PATH="+path)
	return cmd
}

func TestIndependentProcessPinsCrashAndOwnerHandoff(t *testing.T) {
	s, path := newTestStore(t, 64, nil)
	putPages(t, s, []uint64{1, 17}, 1)
	if err := s.Checkpoint(ctx, "pinned"); err != nil {
		t.Fatal(err)
	}
	owner := helperCommand(t, "owner", path)
	out, err := owner.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "BUSY") {
		t.Fatalf("owner collision: %s %v", out, err)
	}
	reader := helperCommand(t, "reader", path)
	stdout, err := reader.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := reader.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	reader.Stderr = os.Stderr
	if err = reader.Start(); err != nil {
		t.Fatal(err)
	}
	defer reader.Process.Kill()
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() || lines.Text() != "READY" {
		t.Fatalf("reader startup %q", lines.Text())
	}
	if err = s.DeleteCheckpoints(ctx, []string{"pinned"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		s.GC(ctx, 64)
	}
	putPages(t, s, []uint64{1, 17}, 2)
	fmt.Fprintln(stdin, "read")
	if !lines.Scan() || lines.Text() != "STABLE" {
		t.Fatalf("parallel export changed %q", lines.Text())
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	putPages(t, s, []uint64{1, 17}, 3)
	fmt.Fprintln(stdin, "read")
	if !lines.Scan() || lines.Text() != "STABLE" {
		t.Fatalf("handoff changed export %q", lines.Text())
	}
	if err = reader.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	reader.Wait()
	// Kernel OFD cleanup, not a destructor callback from the killed reader, must
	// make the pending deletion progress for the next/current owner.
	for i := 0; i < 16; i++ {
		if _, err = s.GC(ctx, 64); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	_, cp, err := s.findCheckpoint("pinned", nil)
	pending := s.ctrl.Pending
	s.mu.Unlock()
	if err != nil || cp.Retained || pending != 0 {
		t.Fatalf("reader crash retained pages: %+v pending=%d err=%v", cp, pending, err)
	}
}

func TestKilledWriterRedoReopen(t *testing.T) {
	for _, stage := range []string{"data", "new-index", "redo-reset", "redo-index", "redo-images", "redo-commit", "apply", "redo-clear"} {
		t.Run(stage, func(t *testing.T) {
			s, path := newTestStore(t, 64, nil)
			putPages(t, s, []uint64{1, 17}, 1)
			if err := s.Checkpoint(ctx, "pinned"); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := helperCommand(t, "crash-write", path)
			cmd.Env = append(cmd.Env, "DIFFSTORE_CRASH_STAGE="+stage)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 87 {
				t.Fatalf("did not kill at %s: %s %v", stage, out, err)
			}
			// A publisher must interpret a committed-but-unapplied record without
			// stealing writer ownership or applying recovery itself.
			v, err := OpenExport(ctx, path, "pinned", nil)
			if err != nil {
				t.Fatal(err)
			}
			want := map[uint64][]byte{1: pageFor(1, 1), 17: pageFor(17, 1)}
			checkView(t, v, want)
			v.Close()
			s, err = Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if stage == "apply" || stage == "redo-clear" {
				want = map[uint64][]byte{1: pageFor(1, 2), 17: pageFor(17, 2)}
			}
			checkActive(t, s, want)
		})
	}
}

func TestSpaceReservationRunsGCBeforeRetry(t *testing.T) {
	s, _ := newTestStore(t, 256, nil)
	putPages(t, s, []uint64{0, 1, 2, 3}, 1)
	if err := s.Discard(ctx, 0, 4*PageSize); err != nil {
		t.Fatal(err)
	}
	fired := false
	s.mu.Lock()
	s.beforeIO = func(stage string) error {
		if stage == "reserve" && !fired {
			fired = true
			return unix.ENOSPC
		}
		return nil
	}
	s.mu.Unlock()
	before := s.Stats()
	putPages(t, s, []uint64{100, 101, 102, 103}, 2)
	after := s.Stats()
	if !fired || after.GCPunched <= before.GCPunched {
		t.Fatalf("ENOSPC did not assist GC: %+v -> %+v", before, after)
	}
	if s.Err() != nil {
		t.Fatal("recoverable reservation failure became sticky", s.Err())
	}
}

func TestReservationFailureNeverOverwritesProtectedPages(t *testing.T) {
	s, path := newTestStore(t, 128, nil)
	putPages(t, s, []uint64{1}, 1)
	s.Checkpoint(ctx, "safe")
	s.MarkModified(ctx)
	s.mu.Lock()
	s.beforeIO = func(stage string) error {
		if stage == "reserve" {
			return unix.ENOSPC
		}
		return nil
	}
	s.mu.Unlock()
	if err := s.WritePages(ctx, []Page{{1, pageFor(1, 2)}}); !errors.Is(err, unix.ENOSPC) {
		t.Fatal(err)
	}
	s.Close()
	v, err := OpenExport(ctx, path, "safe", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	checkView(t, v, map[uint64][]byte{1: pageFor(1, 1)})
}

func TestTemplateRenameKeepsIndependentExports(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "published.diff")
	tmp := filepath.Join(dir, "partial.diff")
	s, err := Create(tmp, Options{DiffID: "rename", LogicalSize: 64 * PageSize, LockPath: final + ".lock"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	putPages(t, s, []uint64{1}, 1)
	if err = s.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(tmp, final); err != nil {
		t.Fatal(err)
	}
	if err = s.Checkpoint(ctx, "ready"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Export(ctx, "ready")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	checkView(t, v, map[uint64][]byte{1: pageFor(1, 1)})
}

func TestBackgroundGCProgressAfterViewClose(t *testing.T) {
	s, _ := newTestStore(t, 64, nil)
	putPages(t, s, []uint64{1}, 1)
	s.Checkpoint(ctx, "old")
	v, err := s.Export(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	putPages(t, s, []uint64{1}, 2)
	s.DeleteCheckpoints(ctx, []string{"old"})
	v.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		pending, garbage := s.ctrl.Pending, s.ctrl.Garbage
		s.mu.Unlock()
		if pending == 0 && garbage == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background GC failed to progress")
}

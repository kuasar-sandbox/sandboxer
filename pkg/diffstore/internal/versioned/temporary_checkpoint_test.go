package versioned

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTemporaryCheckpointRecovery(t *testing.T) {
	for _, failure := range []string{"", "apply"} {
		t.Run("creation-"+failure, func(t *testing.T) {
			s, path := newTestStore(t, 16, nil)
			if err := s.Checkpoint(ctx, "keep"); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected apply failure")
			if failure != "" {
				s.beforeIO = func(stage string) error {
					if stage == failure {
						return injected
					}
					return nil
				}
			}
			err := s.TemporaryCheckpoint(ctx, "temporary")
			if failure == "" && err != nil || failure != "" && !errors.Is(err, injected) {
				t.Fatal(err)
			}
			s.Close()
			reopened, err := Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			for i := 0; i < 3; i++ {
				if _, err := reopened.GC(ctx, 128); err != nil {
					t.Fatal(err)
				}
			}
			ids, err := reopened.ListCheckpoints(ctx)
			if err != nil || len(ids) != 1 || ids[0] != "keep" {
				t.Fatalf("ids=%v err=%v", ids, err)
			}
		})
	}
}

func TestTemporaryRecoveryPreservesReaderPin(t *testing.T) {
	s, path := newTestStore(t, 16, nil)
	putPages(t, s, []uint64{0}, 1)
	if err := s.TemporaryCheckpoint(ctx, "temporary"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Export(ctx, "temporary")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	s.Close()
	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	putPages(t, reopened, []uint64{0}, 2)
	for i := 0; i < 3; i++ {
		if _, err := reopened.GC(ctx, 128); err != nil {
			t.Fatal(err)
		}
	}
	checkView(t, v, map[uint64][]byte{0: pageFor(0, 1)})
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		result, err := reopened.GC(ctx, 128)
		if err != nil {
			t.Fatal(err)
		}
		if result.State == Complete {
			return
		}
	}
	t.Fatal("temporary checkpoint not reclaimed after reader close")
}

func TestTemporaryCheckpointDeleteFailureRecovers(t *testing.T) {
	s, path := newTestStore(t, 16, nil)
	if err := s.TemporaryCheckpoint(ctx, "temporary"); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected delete failure")
	s.beforeIO = func(stage string) error {
		if stage == "redo-commit" {
			return injected
		}
		return nil
	}
	if err := s.DeleteCheckpoint(ctx, "temporary"); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	ids, err := reopened.ListCheckpoints(ctx)
	if err != nil || len(ids) != 0 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

// Exit without Close to model losing the process immediately after creation.
func TestTemporaryCheckpointProcessExit(t *testing.T) {
	if path := os.Getenv("KUASAR_TEST_TEMPORARY_CHECKPOINT"); path != "" {
		s, err := Create(path, Options{DiffID: "crash-test", LogicalSize: 16 * PageSize})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.TemporaryCheckpoint(ctx, "temporary"); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "crash.diff")
	child := exec.Command(os.Args[0], "-test.run=^TestTemporaryCheckpointProcessExit$")
	child.Env = append(os.Environ(), "KUASAR_TEST_TEMPORARY_CHECKPOINT="+path)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, output)
	}
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ids, err := s.ListCheckpoints(ctx)
	if err != nil || len(ids) != 0 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

func TestTemporaryCheckpointProtectsWritesBeforeReaderOpens(t *testing.T) {
	s, _ := newTestStore(t, 16, nil)
	putPages(t, s, []uint64{0}, 1)
	if err := s.TemporaryCheckpoint(ctx, "temporary"); err != nil {
		t.Fatal(err)
	}
	// The guest resumes before snapshot persistence opens its reader.
	putPages(t, s, []uint64{0}, 2)
	for i := 0; i < 3; i++ {
		if _, err := s.GC(ctx, 128); err != nil {
			t.Fatal(err)
		}
	}
	v, err := s.Export(ctx, "temporary")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	checkView(t, v, map[uint64][]byte{0: pageFor(0, 1)})
	if err := s.DeleteCheckpoint(ctx, "temporary"); err != nil {
		t.Fatal(err)
	}
	putPages(t, s, []uint64{0}, 3)
	checkView(t, v, map[uint64][]byte{0: pageFor(0, 1)})
}

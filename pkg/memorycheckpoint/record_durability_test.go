package memorycheckpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRecordPreparationRequiresDurableParents(t *testing.T) {
	for failAt := 1; failAt <= 3; failAt++ {
		t.Run(fmt.Sprintf("sync-%d", failAt), func(t *testing.T) {
			directory := t.TempDir()
			r := CaptureRecord{Version: Version, Producer: RunIdentity{SandboxID: "sid"}, CheckpointID: "cp", MemoryID: "mem", Epoch: 1}
			path, err := RecordPath(directory, r.CheckpointID, r.MemoryID)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{filepath.Join(directory, "memory"), directory, filepath.Join(directory, "memory")}
			var calls []string
			sync := func(path string) error {
				calls = append(calls, path)
				if len(calls) == failAt {
					return unix.EIO
				}
				return syncRecordDirectory(path)
			}
			w, err := reserveRecordWithSync(directory, r, sync)
			if w != nil || !errors.Is(err, unix.EIO) {
				if w != nil {
					w.Close()
				}
				t.Fatalf("parent sync failure admitted preparation: %v", err)
			}
			if !reflect.DeepEqual(calls, want[:failAt]) {
				t.Fatalf("directory sync order: %v, want %v", calls, want[:failAt])
			}
			for _, unpublished := range []string{path, filepath.Join(directory, "current-memory.json")} {
				if _, err := os.Stat(unpublished); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("published after failed parent sync: %s: %v", unpublished, err)
				}
			}
			// Failure can leave directories and the lock behind. Retry must sync
			// their parents again and must not inherit the failed writer's lock.
			calls = nil
			w, err = reserveRecordWithSync(directory, r, func(path string) error {
				calls = append(calls, path)
				return syncRecordDirectory(path)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("retry omitted durability barrier: %v, want %v", calls, want)
			}
		})
	}
}

package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
)

func TestCheckpointExternalLocalLayersRetainOpenFiles(t *testing.T) {
	f := newMemoryCheckpointFixture(t)
	external := t.TempDir()
	var parents []string
	var firstPayload string
	// More carriers than the physical pool can hold, with actual distinct data.
	for i := 0; i < checkpointFileSlots+4; i++ {
		raw, _, d := writeLocalMemory(t, external, fmt.Sprintf("outside-%d", i), f.eRef,
			nil, nil, strings.Repeat(string(rune('a'+i)), 6), 0x3f, 0x3f, 0, false)
		parents = append(parents, strings.Replace(raw, "file://", "file://"+external+"/", 1))
		if i == 0 {
			record, err := memorycheckpoint.ReadRecord(external, d.CheckpointID, d.MemoryID)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := manifest.ParseRef(record.PayloadRef)
			if err != nil {
				t.Fatal(err)
			}
			firstPayload = filepath.Join(external, ref.Path)
		}
	}
	// Model an accepted external copy without the producer's GC protocol.
	lock := filepath.Join(external, ".memory-readers.lock")
	if err := os.Remove(lock); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	f.ref, f.path, f.descriptor = writeLocalMemory(t, f.directory, "external-local-child", f.eRef,
		parents, nil, "xxxxxx", 0, 0, 0, false)
	opened := openLocalFixture(t, f)
	defer opened.Close()
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reader created an external GC protocol: %v", err)
	}
	if err := os.Remove(firstPayload); err != nil {
		t.Fatal(err)
	}
	run, err := opened.Memory.RunAt(0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if n, err := run.ReadAt(context.Background(), b[:], 0); err != nil || n != 1 || b[0] != 'a' {
		t.Fatalf("unlinked external payload: read=%d byte=%q err=%v", n, b, err)
	}
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading created an external GC protocol: %v", err)
	}
}

func TestCheckpointGraphKeepsExternalDirectoryUnpooled(t *testing.T) {
	directory := t.TempDir()
	owner := &memoryGraphOwner{}
	defer owner.Close()
	if pinned, err := owner.pin(directory); err != nil || pinned {
		t.Fatalf("external directory: pinned=%v err=%v", pinned, err)
	}
	// A different opener can establish a lock after this graph chose to retain
	// original file FDs. That must not retroactively enable pool reopening.
	pin, err := memorycheckpoint.PinDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	if pinned, err := owner.pin(directory); err != nil || pinned {
		t.Fatalf("external directory after lock creation: pinned=%v err=%v", pinned, err)
	}
	if len(owner.pins) != 0 {
		t.Fatalf("external directory acquired %d graph pins", len(owner.pins))
	}
}

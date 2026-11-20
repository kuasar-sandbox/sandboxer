package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestCheckpointPoolAliasRetargetAfterSelection(t *testing.T) {
	ctx := context.Background()
	storage, err := NewProcessStorage(plaintextRefConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	save := func(directory, value string) (string, string) {
		sink := snapshot.NewFileSink(directory, "alias", nil, false, nil)
		defer sink.Close()
		ref, path, err := sink.AbsorbImageSource(ctx, sparse.Dense(strings.NewReader(value), uint64(len(value))))
		if err != nil {
			t.Fatal(err)
		}
		return ref, path
	}
	firstDir, secondDir := t.TempDir(), t.TempDir()
	raw, first := save(firstDir, "original")
	_, second := save(secondDir, "replaced")
	alias := filepath.Join(t.TempDir(), "alias.image")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		t.Fatal(err)
	}
	owner := &memoryGraphOwner{pool: newCheckpointFilePool()}
	defer owner.Close()
	selected := 0
	opened, err := storage.openFile(ctx, alias, ref, nil, false, func(physical string) (*checkpointFilePool, error) {
		selected++
		if physical != first {
			return nil, fmt.Errorf("pool selected %q, want %q", physical, first)
		}
		if _, err := owner.pin(filepath.Dir(physical), false); err != nil {
			return nil, err
		}
		if err := os.Remove(alias); err != nil {
			return nil, err
		}
		if err := os.Symlink(second, alias); err != nil {
			return nil, err
		}
		return owner.pool, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if selected != 1 || opened.ResolvedPath() != first {
		t.Fatalf("selection=%d resolved=%q", selected, opened.ResolvedPath())
	}
	// Force the parsed source to reopen its carrier after eviction. It must
	// still use the pinned physical path, never the now-retargeted alias.
	for i := 0; i <= checkpointFileSlots; i++ {
		path := filepath.Join(firstDir, fmt.Sprintf("evict-%d", i))
		if err := os.WriteFile(path, []byte{byte(i)}, 0600); err != nil {
			t.Fatal(err)
		}
		source, err := owner.pool.source(path)
		if err != nil {
			t.Fatal(err)
		}
		var b [1]byte
		if _, err := source.ReadAt(b[:], 0); err != nil {
			t.Fatal(err)
		}
	}
	b := make([]byte, len("original"))
	if _, err := opened.ReadAt(ctx, b, 0); err != nil || string(b) != "original" {
		t.Fatalf("read after retarget/evict = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(secondDir, ".memory-readers.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registered replacement directory: %v", err)
	}
}

func TestCheckpointPoolHolderSwitchDuringGraphConstruction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := newMemoryCheckpointFixture(t)
	owner := &memoryGraphOwner{pool: newCheckpointFilePool()}
	defer owner.Close()
	if _, err := owner.pin(fixture.directory, false); err != nil {
		t.Fatal(err)
	}
	const directories = 48
	type parentCarrier struct {
		directory, path, ref string
		descriptor           memorycheckpoint.Descriptor
	}
	parents := make([]parentCarrier, directories)
	switched := make([]string, directories)
	for i := range parents {
		directory := t.TempDir()
		ref, path, descriptor := writeLocalMemory(t, directory, fmt.Sprintf("parent-%d", i), fixture.eRef,
			nil, nil, "abcdef", 0x3f, 0x3f, 0, false)
		parents[i] = parentCarrier{directory, path, ref, descriptor}
		switched[i] = t.TempDir()
	}
	mem, err := memory.Create("pool-switch", localMemorySize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	copy(mem.Bytes(), checkpointPageBytes("bcxxxx"))
	fd, err := memorycheckpoint.ReadOnlyMemfd(mem.FD())
	if err != nil {
		t.Fatal(err)
	}
	holder, err := memorycheckpoint.PrepareHolder(fixture.directory, t.TempDir(),
		memorycheckpoint.CaptureRecord{Version: memorycheckpoint.Version, Producer: memorycheckpoint.RunIdentity{SandboxID: "switch"},
			CheckpointID: "live", MemoryID: "live-memory", Epoch: 1}, fd, localMemorySize, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = owner.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		_ = holder.Wait(cleanupCtx)
	}()
	record := holder.Record()
	if err := record.Update(func(r *memorycheckpoint.CaptureRecord) {
		r.Committed, r.RuntimeRetired = true, true
		r.SnapshotRef, r.SandboxRef = fixture.ref, fixture.eRef
	}); err != nil {
		t.Fatal(err)
	}
	saved, err := memorycheckpoint.ReadRecord(fixture.directory, fixture.descriptor.CheckpointID, fixture.descriptor.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	begin := make(chan struct{})
	descriptor := fixture.descriptor
	descriptor.CheckpointID, descriptor.MemoryID, descriptor.Parents = "live", "live-memory", nil
	top, err := memorycheckpoint.OpenTop(ctx, descriptor, memorycheckpoint.OpenOptions{Directory: fixture.directory, DirectoryPinned: true,
		OpenFile: func(callCtx context.Context, raw string) (fetch.Stream, error) {
			if raw == saved.PayloadRef {
				close(entered)
				select {
				case <-begin:
				case <-callCtx.Done():
					return nil, callCtx.Err()
				}
				// A real watcher callback registers source directories while
				// the construction side below opens independent parent frames.
				for _, directory := range switched {
					if _, err := owner.pin(directory, false); err != nil {
						return nil, err
					}
				}
			}
			ref, err := manifest.ParseRef(raw)
			if err != nil {
				return nil, err
			}
			return fixture.storage.openFile(callCtx, filepath.Join(fixture.directory, ref.Path), ref, nil, false,
				func(physical string) (*checkpointFilePool, error) {
					ok, err := owner.pin(filepath.Dir(physical), true)
					if err != nil || !ok {
						return nil, err
					}
					return owner.pool, nil
				})
		}})
	if err != nil {
		t.Fatal(err)
	}
	owner.closers = append(owner.closers, top)
	b := make([]byte, 1)
	if _, err := top.Reader.ReadAt(b, 0); err != nil || b[0] != 'b' {
		t.Fatalf("RAM read = %q, %v", b, err)
	}
	if err := mem.Close(); err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(func(_ context.Context, frozen io.ReaderAt) (string, error) {
		actual := make([]byte, localMemorySize)
		if _, err := frozen.ReadAt(actual, 0); err != nil {
			return "", err
		}
		if !bytes.Equal(actual, checkpointPageBytes("bcxxxx")) {
			return "", errors.New("frozen bytes changed")
		}
		return saved.PayloadRef, nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(begin)
	for _, parent := range parents {
		ref, err := manifest.ParseRef(parent.ref)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := fixture.storage.OpenFile(ctx, parent.path, ref)
		if err != nil {
			t.Fatal(err)
		}
		frame, openErr := fixture.storage.newMemoryOpenFrame(ctx, stream, &parent.descriptor, SnapshotOpenOptions{}, owner, false)
		if err := errors.Join(openErr, stream.Close()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := frame.compose(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := holder.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if !record.Read().RAMReleased {
		t.Fatal("watcher did not release RAM")
	}
	if _, err := top.Reader.ReadAt(b, 0); err != nil || b[0] != 'b' {
		t.Fatalf("file read = %q, %v", b, err)
	}
	owner.mu.Lock()
	count := len(owner.pins)
	owner.mu.Unlock()
	if count != 1+2*directories {
		t.Fatalf("directory pins = %d, want %d", count, 1+2*directories)
	}
	for _, directory := range append(switched, parents[0].directory) {
		pin, err := memorycheckpoint.ExclusiveDirectory(directory)
		if err == nil {
			pin.Close()
			t.Fatalf("GC acquired live directory %q", directory)
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	for _, directory := range append(switched, parents[0].directory) {
		pin, err := memorycheckpoint.ExclusiveDirectory(directory)
		if err != nil {
			t.Fatalf("Close retained directory pin: %v", err)
		}
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

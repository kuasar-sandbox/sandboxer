package memorycheckpoint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
)

type namedFixture struct {
	h         *Holder
	d         Descriptor
	opts      OpenOptions
	directory string
}

func newNamedFixture(t *testing.T, directory, id string, retired bool) namedFixture {
	t.Helper()
	m, err := memory.Create("named-checkpoint", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := os.MkdirAll(filepath.Join(directory, "run-"+id), 0700); err != nil {
		t.Fatal(err)
	}
	copy(m.Bytes(), bytes.Repeat([]byte{0x47}, 4096))
	fd, err := ReadOnlyMemfd(m.FD())
	if err != nil {
		t.Fatal(err)
	}
	h, err := PrepareHolder(directory, filepath.Join(directory, "run-"+id), CaptureRecord{
		Version: Version, Producer: RunIdentity{SandboxID: "sid", InstanceID: "instance", RunID: id},
		CheckpointID: id, MemoryID: "mem-" + id, Epoch: 1,
	}, fd, 4096, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Record().Update(func(r *CaptureRecord) {
		r.Committed, r.RuntimeRetired = true, retired
		r.SnapshotRef, r.SandboxRef = "snapshot-"+id, "sandbox-"+id
	}); err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeMap(&memory.CaptureView{Size: 4096, Materialized: []byte{1}, Dirty: []byte{1}, Released: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	d := Descriptor{Version: Version, CheckpointID: id, MemoryID: "mem-" + id, Size: 4096, MapRef: "file://map.image@digest:" + string(bytes.Repeat([]byte{'1'}, 64))}
	opts := OpenOptions{Directory: directory, OpenFile: func(_ context.Context, ref string) (fetch.Stream, error) {
		data := bytes.Repeat([]byte{0x47}, 4096)
		if ref == d.MapRef {
			data = encoded
		} else if ref != "payload" {
			return nil, errors.New("unexpected file ref")
		}
		source, err := sparse.NewSource(bytes.NewReader(data), uint64(len(data)), nil)
		return &testStream{Source: source}, err
	}}
	f := namedFixture{h: h, d: d, opts: opts, directory: directory}
	t.Cleanup(func() {
		select {
		case <-h.Done():
			return
		default:
		}
		_ = h.Record().Update(func(r *CaptureRecord) { r.RuntimeRetired = true })
		_ = h.markDeleted()
		h.mu.Lock()
		started := h.started
		h.mu.Unlock()
		if !started {
			_ = h.Start(func(context.Context, io.ReaderAt) (string, error) { return "payload", nil }, nil)
		}
		f.wait(t)
	})
	return f
}

func (f namedFixture) wait(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
func (f namedFixture) delete(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := DeleteCheckpoint(ctx, f.directory, Reference{f.d.CheckpointID, f.d.MemoryID}); err != nil {
		t.Fatal(err)
	}
}
func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("checkpoint operation did not reach barrier")
	}
}

func TestNamedHoldersCancelIndependently(t *testing.T) {
	dir := t.TempDir()
	a, b := newNamedFixture(t, dir, "a", true), newNamedFixture(t, dir, "b", true)
	for _, f := range []namedFixture{a, b} {
		entered := make(chan struct{})
		if err := f.h.Start(func(ctx context.Context, _ io.ReaderAt) (string, error) {
			close(entered)
			<-ctx.Done()
			return "", ctx.Err()
		}, nil); err != nil {
			t.Fatal(err)
		}
		awaitSignal(t, entered)
	}
	if list, err := ListCheckpoints(dir); err != nil || len(list) != 2 {
		t.Fatalf("list: %d %v", len(list), err)
	}
	a.delete(t)
	a.wait(t)
	if r := a.h.Record().Read(); !r.Deleted || !r.Discarded || !r.RAMReleased || r.FileReady {
		t.Fatalf("cancel facts: %+v", r)
	}
	if r := b.h.Record().Read(); r.Deleted || r.RAMReleased {
		t.Fatalf("other holder affected: %+v", r)
	}
	if list, err := ListCheckpoints(dir); err != nil || len(list) != 1 || list[0].CheckpointID != "b" {
		t.Fatalf("visible list: %v %v", list, err)
	}
	if all, err := ListRecords(dir); err != nil || len(all) != 2 {
		t.Fatalf("recovery inventory lost responsibility: %v", err)
	}
	a.delete(t) // old, non-current exact identity remains idempotent
	b.delete(t)
	b.wait(t)
}

func TestDeletePreservesOpenViewAndFileSwitch(t *testing.T) {
	f := newNamedFixture(t, t.TempDir(), "a", true)
	top, err := OpenTop(context.Background(), f.d, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = top.Close() })
	gate, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(gate) }) })
	if err := f.h.Start(func(ctx context.Context, _ io.ReaderAt) (string, error) {
		close(entered)
		select {
		case <-gate:
			return "payload", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}, nil); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, entered)
	f.delete(t)
	if view, err := OpenTop(context.Background(), f.d, f.opts); !errors.Is(err, ErrDeleted) || view != nil {
		t.Fatalf("new view admitted after deletion: %v", err)
	}
	f.h.mu.Lock()
	proof := f.h.cancellationProofLocked()
	f.h.mu.Unlock()
	if proof != nil {
		proof.Close()
		t.Fatal("cancel acquired proof while reader owns RAM")
	}
	buf := make([]byte, 16)
	if _, err := top.Reader.ReadAt(buf, 0); err != nil || buf[0] != 0x47 {
		t.Fatalf("existing RAM view: %x %v", buf, err)
	}
	once.Do(func() { close(gate) })
	f.wait(t)
	if r := f.h.Record().Read(); !r.Deleted || !r.FileReady || !r.RAMReleased || r.Discarded {
		t.Fatalf("file-switch facts: %+v", r)
	}
	if _, err := top.Reader.ReadAt(buf, 0); err != nil || buf[0] != 0x47 {
		t.Fatalf("existing file view: %x %v", buf, err)
	}
	if _, err := OpenTop(context.Background(), f.d, f.opts); !errors.Is(err, ErrDeleted) {
		t.Fatalf("deleted published view admitted: %v", err)
	}
}

func TestDeleteWaitsForWholeDumpAndRejectsLatePublication(t *testing.T) {
	f := newNamedFixture(t, t.TempDir(), "a", true)
	entered, cancelled, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(finish) }) })
	if err := f.h.Start(func(ctx context.Context, r io.ReaderAt) (string, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-finish
		if _, err := r.ReadAt(make([]byte, 1), 0); err != nil {
			return "", err
		}
		return "late-success", nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, entered)
	f.delete(t)
	awaitSignal(t, cancelled)
	if r := f.h.Record().Read(); r.RAMReleased || r.Discarded || r.FileReady {
		t.Fatalf("released before writer exit: %+v", r)
	}
	once.Do(func() { close(finish) })
	f.wait(t)
	if r := f.h.Record().Read(); !r.RAMReleased || !r.Discarded || r.FileReady || r.PayloadRef != "" {
		t.Fatalf("late writer published: %+v", r)
	}
}

func TestDeleteQueuedRAMDescriptorsHoldCancellation(t *testing.T) {
	f := newNamedFixture(t, t.TempDir(), "a", true)
	r := f.h.Record().Read()
	c, err := dialHolder(context.Background(), r.Holder)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := writeHolderMessage(c, holderMessage{Type: "open", MemoryID: r.MemoryID, Epoch: r.Epoch, CheckpointID: r.CheckpointID, Token: "queued"}); err != nil {
		t.Fatal(err)
	}
	var reply holderMessage
	if err := readHolderMessage(c, &reply); err != nil || reply.Type != "ram" {
		t.Fatalf("open: %+v %v", reply, err)
	}
	// Keep the two descriptors unread in the Unix socket's receive queue.
	entered := make(chan struct{})
	if err := f.h.Start(func(ctx context.Context, _ io.ReaderAt) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}, nil); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, entered)
	f.delete(t)
	f.h.mu.Lock()
	proof := f.h.cancellationProofLocked()
	f.h.mu.Unlock()
	if proof != nil {
		proof.Close()
		t.Fatal("queued SCM_RIGHTS did not protect RAM")
	}
	if f.h.Record().Read().RAMReleased {
		t.Fatal("queued RAM released")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
}

func TestDeleteDoesNotRetireRuntime(t *testing.T) {
	f := newNamedFixture(t, t.TempDir(), "a", false)
	f.delete(t)
	var called atomic.Bool
	dump := func(context.Context, io.ReaderAt) (string, error) { called.Store(true); return "payload", nil }
	if err := f.h.Start(dump, nil); err == nil {
		t.Fatal("started before runtime retired")
	}
	if r := f.h.Record().Read(); r.RuntimeRetired || r.RAMReleased {
		t.Fatalf("delete invented retirement: %+v", r)
	}
	if err := f.h.Record().Update(func(r *CaptureRecord) { r.RuntimeRetired = true }); err != nil {
		t.Fatal(err)
	}
	if err := f.h.Start(dump, nil); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	if called.Load() {
		t.Fatal("started unnecessary dump after deletion")
	}
}

func TestDeletePublishedAndLegacyRecord(t *testing.T) {
	dir := t.TempDir()
	w := testRecord(t, dir)
	if err := w.Update(func(r *CaptureRecord) {
		r.Committed = true
		r.RuntimeRetired = true
		r.FileReady = true
		r.RAMReleased = true
		r.SnapshotRef = "snapshot"
		r.SandboxRef = "sandbox"
		r.PayloadRef = "payload"
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := DeleteCheckpoint(context.Background(), dir, Reference{"cp", "wrong"}); err == nil {
		t.Fatal("inexact delete accepted")
	}
	if err := DeleteCheckpoint(context.Background(), dir, Reference{"cp", "mem"}); err != nil {
		t.Fatal(err)
	}
	r, err := ReadRecord(dir, "cp", "mem")
	if err != nil || !r.Deleted || r.Version != RecordVersion || !r.FileReady || r.Discarded {
		t.Fatalf("legacy deletion: %+v %v", r, err)
	}
	lock, err := openPreparationLock(dir, "cp", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	upgraded := &RecordWriter{path: w.Path(), lock: lock, record: *r}
	if err := upgraded.Update(func(r *CaptureRecord) { r.Deleted = false; r.Version = Version }); err == nil {
		t.Fatal("revoked version resurrected")
	}
	if _, err := os.Stat(w.Path()); err != nil {
		t.Fatal("proof record removed")
	}
}

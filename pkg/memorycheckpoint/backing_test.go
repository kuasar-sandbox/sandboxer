package memorycheckpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
)

func TestMapCodecGeometryAndOverlap(t *testing.T) {
	v := &memory.CaptureView{Size: 9 * 4096, Materialized: []byte{0x81, 1}, Dirty: []byte{1, 0}, Released: []byte{0x82, 1}, WorkingSetOnly: true}
	b, err := EncodeMap(v)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeMap(b, v.Size)
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("round trip=%+v %v", got, err)
	}
	for _, mutate := range []func([]byte){func(b []byte) { b[24] = 2 }, func(b []byte) { b[25] = 1 }, func(b []byte) { b[len(b)-1] |= 0x80 }, func(b []byte) { b[8]++ }} {
		bad := bytes.Clone(b)
		mutate(bad)
		if _, err := DecodeMap(bad, v.Size); err == nil {
			t.Fatal("invalid map accepted")
		}
	}
}

func testRecord(t *testing.T, dir string) *RecordWriter {
	t.Helper()
	start, err := ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	w, err := CreateRecord(dir, CaptureRecord{Version: 1, Producer: RunIdentity{SandboxID: "sid", InstanceID: "instance", RunID: "run"}, CheckpointID: "cp", MemoryID: "mem", Epoch: 1, Holder: HolderIdentity{PID: os.Getpid(), StartTime: start, SocketPath: filepath.Join(dir, "m.sock")}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestCaptureRecordFactsAndUnknownWait(t *testing.T) {
	dir := t.TempDir()
	w := testRecord(t, dir)
	defer w.Close()
	if err := w.Update(func(r *CaptureRecord) { r.FileReady = true }); err == nil {
		t.Fatal("file ready without payload accepted")
	}
	if _, err := WaitCurrent(context.Background(), dir, "sid", true); !errors.Is(err, ErrSavePending) {
		t.Fatalf("pending=%v", err)
	}
	if err := w.Update(func(r *CaptureRecord) { r.Aborted = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := WaitCurrent(context.Background(), dir, "sid", true); err != nil {
		t.Fatal(err)
	}
	if err := w.Update(func(r *CaptureRecord) { r.Aborted = false }); err == nil {
		t.Fatal("aborted capture resurrected")
	}
	if err := os.WriteFile(w.Path(), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := WaitCurrent(context.Background(), dir, "sid", true); err == nil {
		t.Fatal("unknown record treated as ready")
	}
}

func TestCurrentLocatorCannotHideMissingRecord(t *testing.T) {
	dir := t.TempDir()
	w := testRecord(t, dir)
	defer w.Close()
	if err := os.Remove(w.Path()); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCurrent(dir); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling locator treated as absence: %v", err)
	}
}

func TestCheckpointLogicalIDIsNotAPath(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"检查点/第一轮", "../outside", ".", "name with spaces"} {
		path, err := RecordPath(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(path) != filepath.Join(dir, "memory") || len(filepath.Base(path)) != 69 {
			t.Fatalf("unsafe record path %q", path)
		}
	}
}

func TestDiscardRequiresClosedReadersWithoutInventingFile(t *testing.T) {
	dir := t.TempDir()
	w := testRecord(t, dir)
	r := w.Read()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// A reused PID with a different start time is an already-dead producer.
	r.Holder.StartTime++
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(w.Path(), b, 0600); err != nil {
		t.Fatal(err)
	}
	pin, err := PinDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DiscardCapture(context.Background(), dir, r.Producer, r.CheckpointID); err == nil {
		t.Fatal("discarded live reader")
	}
	_ = pin.Close()
	got, err := DiscardCapture(context.Background(), dir, r.Producer, r.CheckpointID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Discarded || !got.RAMReleased || got.FileReady || got.PayloadRef != "" {
		t.Fatalf("invalid destruction fact: %+v", got)
	}
}

type testStream struct {
	sparse.Source
	close func() error
}

func (s *testStream) Close() error {
	if s.close != nil {
		return s.close()
	}
	return nil
}

func TestHolderReadOnlyHandoffAndActiveSwitch(t *testing.T) {
	for _, mode := range []string{"connected", "lost-stream", "long-path"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "ksmem-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			if mode == "long-path" {
				dir = filepath.Join(dir, strings.Repeat("x", 100), strings.Repeat("y", 100))
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			w := testRecord(t, dir)
			m, err := memory.Create("holder-test", 8192)
			if err != nil {
				t.Fatal(err)
			}
			copy(m.Bytes(), bytes.Repeat([]byte{0x35}, 8192))
			fd, err := ReadOnlyMemfd(m.FD())
			if err != nil {
				t.Fatal(err)
			}
			h, err := NewHolder(w, fd, 8192, t.Logf)
			if err != nil {
				m.Close()
				fd.Close()
				w.Close()
				t.Fatal(err)
			}
			if err = w.Update(func(r *CaptureRecord) { r.Committed = true; r.SnapshotRef = "snapshot"; r.SandboxRef = "sandbox" }); err != nil {
				t.Fatal(err)
			}
			view := &memory.CaptureView{Size: 8192, Materialized: []byte{3}, Dirty: []byte{1}, Released: []byte{0}}
			encoded, err := EncodeMap(view)
			if err != nil {
				t.Fatal(err)
			}
			mapRef := "file://map.image@digest:" + string(bytes.Repeat([]byte{'1'}, 64))
			var opens atomic.Int32
			opts := OpenOptions{Directory: dir, OpenFile: func(_ context.Context, ref string) (fetch.Stream, error) {
				var b []byte
				if ref == mapRef {
					b = encoded
				} else if ref == "payload" {
					b = bytes.Repeat([]byte{0x35}, 8192)
					opens.Add(1)
				} else {
					return nil, errors.New("unexpected ref")
				}
				s, err := sparse.NewSource(bytes.NewReader(b), uint64(len(b)), nil)
				return &testStream{Source: s}, err
			}}
			d := Descriptor{Version: 1, MemoryID: "mem", CheckpointID: "cp", Size: 8192, MapRef: mapRef}
			top, err := OpenTop(context.Background(), d, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer top.Close()
			if _, err := top.reader.ram.WriteAt([]byte{1}, 0); err == nil {
				t.Fatal("transferred fd is writable")
			}
			// Keep the same published reader before/after switching, as a sparse
			// Run would. Closing the caller's original mapping cannot invalidate it.
			reader := top.Reader
			if err = m.Close(); err != nil {
				t.Fatal(err)
			}
			if err = w.Update(func(r *CaptureRecord) { r.RuntimeRetired = true }); err != nil {
				t.Fatal(err)
			}
			blocked := make(chan struct{})
			entered := make(chan struct{})
			var released atomic.Bool
			if err = h.Start(func(_ context.Context, r io.ReaderAt) (string, error) {
				close(entered)
				<-blocked
				b := make([]byte, 8192)
				_, err := r.ReadAt(b, 0)
				if err != nil {
					return "", err
				}
				if !bytes.Equal(b, bytes.Repeat([]byte{0x35}, 8192)) {
					return "", errors.New("frozen data changed")
				}
				return "payload", nil
			}, func(context.Context) error {
				if !w.Read().RAMReleased {
					t.Error("resource released before RAM")
				}
				released.Store(true)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			<-entered
			if w.Read().RAMReleased {
				t.Fatal("blocked dump reported ready")
			}
			b := make([]byte, 4096)
			if _, err = reader.ReadAt(b, 0); err != nil || b[0] != 0x35 {
				t.Fatalf("RAM read=%x %v", b[0], err)
			}
			if mode == "lost-stream" {
				_ = top.conn.Close()
			}
			close(blocked)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err = h.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if !released.Load() || opens.Load() == 0 {
				t.Fatal("reader did not actively switch/release")
			}
			if _, err = reader.ReadAt(b, 4096); err != nil || b[0] != 0x35 {
				t.Fatalf("stable reader after release=%x %v", b[0], err)
			}
			if _, err = os.Stat(w.Read().Holder.SocketPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("holder socket remains: %v", err)
			}
		})
	}
}

func TestHolderInterruptedFDHandshakeReleasesKnownToken(t *testing.T) {
	for _, readHeader := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-header", true: "before-fd"}[readHeader], func(t *testing.T) {
			dir, err := os.MkdirTemp("", "kslease-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			w := testRecord(t, dir)
			m, err := memory.Create("handshake", 4096)
			if err != nil {
				t.Fatal(err)
			}
			fd, err := ReadOnlyMemfd(m.FD())
			if err != nil {
				t.Fatal(err)
			}
			h, err := NewHolder(w, fd, 4096, t.Logf)
			if err != nil {
				m.Close()
				fd.Close()
				w.Close()
				t.Fatal(err)
			}
			if err = w.Update(func(r *CaptureRecord) { r.Committed = true; r.SnapshotRef = "snapshot"; r.SandboxRef = "sandbox" }); err != nil {
				t.Fatal(err)
			}
			r := w.Read()
			lease := holderMessage{Type: "open", MemoryID: r.MemoryID, Epoch: r.Epoch, CheckpointID: r.CheckpointID, Token: "preallocated"}
			c, err := dialHolder(context.Background(), r.Holder)
			if err != nil {
				t.Fatal(err)
			}
			if err = writeHolderMessage(c, lease); err != nil {
				t.Fatal(err)
			}
			if readHeader {
				var header holderMessage
				if err = readHolderMessage(c, &header); err != nil || header.Type != "ram" {
					t.Fatalf("header=%+v %v", header, err)
				}
			}
			_ = c.Close()
			lease.Type = "release"
			retryRelease(dir, r, lease)
			_ = m.Close()
			if err = w.Update(func(r *CaptureRecord) { r.RuntimeRetired = true }); err != nil {
				t.Fatal(err)
			}
			if err = h.Start(func(context.Context, io.ReaderAt) (string, error) { return "payload", nil }, nil); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err = h.Wait(ctx); err != nil {
				t.Fatalf("phantom lease retained after failed open: %v", err)
			}
		})
	}
}

func TestReleaseAcknowledgementCanUseExactPhysicalFact(t *testing.T) {
	directory := t.TempDir()
	w := testRecord(t, directory)
	defer w.Close()
	if err := w.Update(func(r *CaptureRecord) {
		r.Committed, r.RuntimeRetired, r.FileReady = true, true, true
		r.SnapshotRef, r.SandboxRef, r.PayloadRef = "snapshot", "sandbox", "payload"
	}); err != nil {
		t.Fatal(err)
	}
	r := w.Read()
	if leaseReleaseRecorded(directory, r) {
		t.Fatal("file publication was treated as physical release")
	}
	if err := w.Update(func(r *CaptureRecord) { r.RAMReleased = true }); err != nil {
		t.Fatal(err)
	}
	// The producer is this still-live SDK process; its generation socket is
	// gone. The persisted release fact must settle a lost final wire ACK.
	top := &TopBacking{record: r, lease: holderMessage{Token: "lost-ack"}, options: OpenOptions{Directory: directory}}
	if !top.releaseLease() || top.lease.Token != "" {
		t.Fatal("lost ACK retained a physically released lease")
	}
	wrong := r
	wrong.MemoryID = "another"
	if leaseReleaseRecorded(directory, wrong) {
		t.Fatal("a different generation settled the old lease")
	}
}

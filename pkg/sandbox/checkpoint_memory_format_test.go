package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// Exercise the real memfd/socket handoff, selected encoder and existing reader.
// No VMM is needed to verify that encoding does not change the frozen layer or
// require a final payload before the initial checkpoint is readable.
func TestCheckpointMemorySaveFormats(t *testing.T) {
	for _, mode := range []string{ctl.SnapshotModeLocal, ctl.SnapshotModeBundle} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			directory := t.TempDir()
			runDir := filepath.Join(t.TempDir(), "run")
			if err := os.Mkdir(runDir, 0700); err != nil {
				t.Fatal(err)
			}
			m, err := memory.Create("format-capture", 3*4096)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			copy(m.Bytes(), bytes.Repeat([]byte{0x63}, 3*4096))
			cfg := &manifest.Config{Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}}, Crypto: manifestcrypto.Config{Chunk: "aes", Manifest: "aes"}}
			key := [32]byte{0x31, 0x72}
			resolverClosed := false
			opts := RunOptions{EnableCheckpoint: true, runtime: &Runtime{}, SandboxID: "sid", checkpointDir: directory, ManifestCfg: cfg, memoryLogf: t.Logf,
				CustomerKeyFn: func() ([32]byte, error) {
					if resolverClosed {
						return [32]byte{}, errors.New("borrowed resolver closed")
					}
					return key, nil
				},
			}
			save, err := prepareCheckpointMemorySave(ctx, ctl.Request{CheckpointID: "first", Mode: mode}, opts, m, runDir)
			if err != nil || save == nil {
				t.Fatalf("prepare: %v %v", save, err)
			}
			defer save.abort()
			resolverClosed = true
			sink, err := save.newSink()
			if err != nil {
				t.Fatal(err)
			}
			defer sink.Close()
			cfgBytes, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: "manifest://" + strings.Repeat("a", 64)})
			if err != nil {
				t.Fatal(err)
			}
			capture := save.capture(opts, nil, false)
			view := &memory.CaptureView{Size: 3 * 4096, Materialized: []byte{1}, Dirty: []byte{1}, Released: []byte{2}}
			source, _, err := capture.Build(ctx, sink, []byte("{}"), []byte("{}"), cfgBytes, view)
			if err != nil {
				t.Fatal(err)
			}
			rootRef, rootPath, err := sink.AbsorbSnapshot(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(directory, "sid.snapshot")); !os.IsNotExist(err) {
				t.Fatalf("uncommitted alias: %v", err)
			}
			open := func(ctx context.Context, raw string) (fetch.Stream, error) {
				ref, err := manifest.ParseRef(raw)
				if err != nil {
					return nil, err
				}
				return artifact.OpenFile(ctx, filepath.Join(directory, ref.Path), ref, cfg, func() ([32]byte, error) { return key, nil }, nil, nil, false)
			}
			// Absorb already returns a fully readable local carrier, including
			// for Bundle. The holder record therefore never stores a bare key.
			root, err := open(ctx, rootRef)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := snapshotfile.ReadMetadata(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			if metadata.LocalCheckpoint == nil {
				t.Fatal("missing local descriptor")
			}
			if err := capture.Commit(&snapshot.Outputs{SnapshotRef: rootRef, SandboxRef: "manifest://" + strings.Repeat("a", 64)}); err != nil {
				t.Fatal(err)
			}
			if err := sink.CommitSnapshot(ctx, rootRef, rootPath); err != nil {
				t.Fatal(err)
			}
			backing, err := memorycheckpoint.OpenTop(ctx, *metadata.LocalCheckpoint, memorycheckpoint.OpenOptions{Directory: directory, OpenFile: open})
			if err != nil {
				t.Fatal(err)
			}
			defer backing.Close()
			check := func() {
				t.Helper()
				top, _, err := snapshot.CaptureMemory(backing.Reader, backing.Map, false)
				if err != nil {
					t.Fatal(err)
				}
				for page, want := range []sparse.RunKind{sparse.Data, sparse.Zero, sparse.Hole} {
					run, err := top.RunAt(uint64(page*4096), 4096)
					if err != nil || run.Kind() != want {
						t.Fatalf("page %d: %v %v", page, run, err)
					}
				}
				b := make([]byte, 4096)
				if _, err := top.ReadAt(ctx, b, 0); err != nil || !bytes.Equal(b, bytes.Repeat([]byte{0x63}, 4096)) {
					t.Fatalf("frozen data changed: %v", err)
				}
			}
			check()
			if err := save.holder.Record().Update(func(r *memorycheckpoint.CaptureRecord) { r.RuntimeRetired = true }); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(runDir); err != nil {
				t.Fatal(err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			allow := make(chan struct{})
			if err := save.holder.Start(func(ctx context.Context, reader io.ReaderAt) (string, error) { <-allow; return save.dump(ctx, reader) }, nil); err != nil {
				t.Fatal(err)
			}
			if save.holder.Record().Read().FileReady {
				t.Fatal("blocked writer published FileReady")
			}
			check()
			close(allow)
			// The live reader switches to the completed file and drops its RAM
			// pin. Waiting for the holder must not require closing this reader.
			if err := save.holder.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			check()
			r := save.holder.Record().Read()
			if !r.FileReady || !r.RAMReleased {
				t.Fatalf("unfinished capture: %+v", r)
			}
			ref, err := manifest.ParseRef(r.PayloadRef)
			if err != nil || ref.Scheme != manifest.RefSchemeFile {
				t.Fatalf("nonlocal payload: %s %v", r.PayloadRef, err)
			}
			if (mode == ctl.SnapshotModeBundle) != (ref.DigestScheme == "manifest" && filepath.Ext(ref.Path) == ".bundle") {
				t.Fatalf("encoding ignored: %s", r.PayloadRef)
			}
			fresh, err := memorycheckpoint.OpenTop(ctx, *metadata.LocalCheckpoint, memorycheckpoint.OpenOptions{Directory: directory, OpenFile: open})
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.Close()
			var b [1]byte
			if _, err := fresh.Reader.ReadAt(b[:], 0); err != nil || b[0] != 0x63 {
				t.Fatalf("reopen completed file: %v", err)
			}
		})
	}
}

func TestCheckpointMemoryFormatsAllowMultipleHolders(t *testing.T) {
	for _, previous := range []string{ctl.SnapshotModeLocal, ctl.SnapshotModeBundle} {
		for _, next := range []string{ctl.SnapshotModeLocal, ctl.SnapshotModeBundle} {
			t.Run(previous+"-to-"+next, func(t *testing.T) {
				directory := t.TempDir()
				record := memorySaveRecord(t, directory)
				if err := record.Update(func(r *memorycheckpoint.CaptureRecord) {
					r.RuntimeRetired = true
					r.FileReady = true
					r.PayloadRef = "previous." + previous
				}); err != nil {
					t.Fatal(err)
				}
				h := &SnapshotHandler{SandboxID: "sid", CheckpointDir: directory, EnableCheckpoint: true, runtime: &Runtime{}, CHProcess: func() processSignaler { return nil }}
				for _, noWait := range []bool{false, true} {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					_, err := h.handleContext(ctx, ctl.Request{CheckpointID: "next", Mode: next, NoWaitForPreviousMemorySave: noWait}, "", nil)
					cancel()
					if err == nil || !strings.Contains(err.Error(), "process is not started") {
						t.Fatalf("format gated new capture: %v", err)
					}
					if record.Read().RAMReleased {
						t.Fatal("old RAM was forgotten")
					}
				}
			})
		}
	}
}

func TestCheckpointMemoryWaitOnlyForNewRetention(t *testing.T) {
	for _, tc := range []struct {
		name   string
		adjust func(*SnapshotHandler, *ctl.Request)
	}{
		{"unnamed", func(_ *SnapshotHandler, r *ctl.Request) { r.CheckpointID = "" }},
		{"resume-after", func(_ *SnapshotHandler, r *ctl.Request) { r.ResumeAfter = true }},
		{"checkpoint-disabled", func(h *SnapshotHandler, _ *ctl.Request) { h.EnableCheckpoint = false }},
		{"no-runtime-owner", func(h *SnapshotHandler, _ *ctl.Request) { h.runtime = nil }},
		{"legacy-managed-host", func(h *SnapshotHandler, _ *ctl.Request) {
			h.CurrentRun = &artifact.CurrentRun{SandboxID: "sid", InstanceID: "instance", RunID: "run"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			_ = memorySaveRecord(t, directory)
			h := &SnapshotHandler{EnableCheckpoint: true, CheckpointDir: directory, runtime: &Runtime{}, CHProcess: func() processSignaler { return nil }}
			req := ctl.Request{CheckpointID: "next", Mode: ctl.SnapshotModeBundle, NoWaitForPreviousMemorySave: true}
			tc.adjust(h, &req)
			if _, err := h.handleContext(context.Background(), req, "", nil); err == nil || !strings.Contains(err.Error(), "process is not started") {
				t.Fatalf("synchronous request waited for previous RAM: %v", err)
			}
		})
	}
}

func TestCheckpointMemoryCaptureStateCapability(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		enabled, owner, wantPending bool
	}{
		{"supported", true, true, true},
		{"checkpoint-disabled", false, true, false},
		{"no-runtime-owner", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			_ = memorySaveRecord(t, directory)
			h := &SnapshotHandler{SandboxID: "sid", EnableCheckpoint: tc.enabled, CheckpointDir: directory}
			if tc.owner {
				h.runtime = &Runtime{}
			}
			q := ctl.CaptureStateQuery{SandboxID: "sid", NoWaitForPreviousMemorySave: true, ForCapture: true}
			state, err := h.captureState(context.Background(), q)
			if err != nil || state.Pending || state.LifecycleVersion != ctl.CheckpointLifecycleVersion || state.RetentionSupported == nil || *state.RetentionSupported != tc.wantPending {
				t.Fatalf("capture admission: %+v %v", state, err)
			}
			q.ForCapture = false
			state, err = h.captureState(context.Background(), q)
			if err != nil || !state.Pending || state.RetentionSupported != nil {
				t.Fatalf("direct save-state query lost obligation: %+v %v", state, err)
			}
			q.ForCapture = true
			q.RunID = "different"
			if _, err := h.captureState(context.Background(), q); err == nil {
				t.Fatal("capability shortcut bypassed exact runtime identity")
			}
		})
	}
}

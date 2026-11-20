package artifact

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestCheckpointPooledBundleKeepsChunkRun(t *testing.T) {
	for _, policy := range []manifestcrypto.LocalPolicy{manifestcrypto.LocalOff, manifestcrypto.LocalAuto} {
		t.Run(string(policy), func(t *testing.T) {
			f := newMemoryCheckpointFixture(t)
			cfg, _ := sourceBundleConfig(policy)
			storage, err := NewProcessStorage(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			f.storage = storage
			admission, err := cfg.WriteAdmission(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			sink, err := snapshot.NewPlannedBundleSink(f.directory, "history", cfg, storage.CustomerKeyFunc(), admission, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer sink.Close()
			encoded, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: "manifest://" + strings.Repeat("e", 64)})
			if err != nil {
				t.Fatal(err)
			}
			data := checkpointPageBytes("abcdef")
			source, err := snapshotfile.BuildSource(sparse.Dense(bytes.NewReader(data), uint64(len(data))), []byte("{}"), []byte("{}"), encoded)
			if err != nil {
				t.Fatal(err)
			}
			raw, _, err := sink.AbsorbSnapshot(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			if err := sink.CommitSnapshot(context.Background(), raw, ""); err != nil {
				t.Fatal(err)
			}
			ref, _, err := snapshot.CommittedArtifactRef(sink, raw, "")
			if err != nil {
				t.Fatal(err)
			}
			f.ref, f.path, f.descriptor = writeLocalMemory(t, f.directory, "bundle-child", f.eRef, []string{ref}, nil, "xxxxxx", 0, 0, 0, false)
			root := openLocalFixture(t, f)
			defer root.Close()
			run, err := root.Memory.RunAt(0, 4096)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := run.(fetch.ChunkRun); !ok {
				t.Fatalf("serving Bundle capability lost: %T", run)
			}
			assertMemoryPages(t, root.Memory, "DDDDDD", "abcdef")
		})
	}
}

func TestCheckpointExternalFileRetainsOpenUnlinkBehavior(t *testing.T) {
	f := newMemoryCheckpointFixture(t)
	external := t.TempDir()
	sink := snapshot.NewFileSink(external, "ordinary", nil, false, nil)
	defer sink.Close()
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: f.eRef})
	if err != nil {
		t.Fatal(err)
	}
	data := checkpointPageBytes("abcdef")
	source, err := snapshotfile.BuildSource(sparse.Dense(bytes.NewReader(data), uint64(len(data))), []byte("{}"), []byte("{}"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw, path, err := sink.AbsorbSnapshot(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	// Bind the relative content ref to its ordinary, non-checkpoint directory.
	raw = strings.Replace(raw, "file://", "file://"+external+"/", 1)
	f.ref, f.path, f.descriptor = writeLocalMemory(t, f.directory, "external-child", f.eRef, []string{raw}, nil, "xxxxxx", 0, 0, 0, false)
	root := openLocalFixture(t, f)
	defer root.Close()
	if _, err := os.Stat(filepath.Join(external, ".memory-readers.lock")); !os.IsNotExist(err) {
		t.Fatalf("created lock in ordinary source: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertMemoryPages(t, root.Memory, "DDDDDD", "abcdef")
}

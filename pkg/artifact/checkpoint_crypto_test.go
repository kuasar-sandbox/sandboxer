package artifact

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestCleanupCheckpointEncryptedBatchedSweep(t *testing.T) {
	for _, policy := range []manifestcrypto.LocalPolicy{manifestcrypto.LocalOff, manifestcrypto.LocalAuto, manifestcrypto.LocalRequired} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			cfg, _ := sourceBundleConfig(policy)
			t.Setenv("MANIFEST_KEY", cfg.Manifest.Key)
			storage, err := NewProcessStorage(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			sink := snapshot.NewFileSink(dir, "owned", storage.LocalCodec(), storage.LocalRequired(), nil)
			portable, _ := publishPortable(t, "")
			makeE := func(value byte) string {
				src, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{value}, 8192)), nil, portable)
				if err != nil {
					t.Fatal(err)
				}
				ref, path, err := sink.AbsorbSandbox(ctx, src)
				if err != nil {
					t.Fatal(err)
				}
				if err = sink.CommitSandbox(ctx, ref, path); err != nil {
					t.Fatal(err)
				}
				return ref
			}
			oldE := makeE(1)
			e := makeE(2)
			sc, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: e})
			if err != nil {
				t.Fatal(err)
			}
			src, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{3}, 8192)), []byte("{}"), []byte("{}"), sc)
			if err != nil {
				t.Fatal(err)
			}
			s, sPath, err := sink.AbsorbSnapshot(ctx, src)
			if err != nil {
				t.Fatal(err)
			}
			if err = sink.CommitSnapshot(ctx, s, sPath); err != nil {
				t.Fatal(err)
			}
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
			old, err := manifest.ParseRef(oldE)
			if err != nil {
				t.Fatal(err)
			}
			gone := []string{old.Path}
			for n := 0; n < 400; n++ {
				name := fmt.Sprintf("owned.snapshot.%d.partial", n)
				if err = os.WriteFile(filepath.Join(dir, name), []byte("interrupted"), 0600); err != nil {
					t.Fatal(err)
				}
				gone = append(gone, name)
			}
			unknown := []string{"owned.snapshot.+1.partial", "owned.snapshot.01.partial", "owned.snapshot.4294967296.partial", "owned2.snapshot.2.partial", "notes.snapshot"}
			for _, name := range unknown {
				if err = os.WriteFile(filepath.Join(dir, name), []byte("user"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			checkpoint := Checkpoint{Directory: dir, SandboxID: "owned", SnapshotRef: s, SandboxRef: e}
			if err = storage.CleanupCheckpoint(ctx, checkpoint); err != nil {
				t.Fatal(err)
			}
			for _, name := range gone {
				if _, err = os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Errorf("obsolete remains %s: %v", name, err)
				}
			}
			ep, _ := manifest.ParseRef(e)
			sp, _ := manifest.ParseRef(s)
			for _, name := range append(unknown, ep.Path, sp.Path, "owned.sandbox", "owned.snapshot") {
				if _, err = os.Lstat(filepath.Join(dir, name)); err != nil {
					t.Errorf("required/unknown removed %s: %v", name, err)
				}
			}
			if err = storage.CleanupCheckpoint(ctx, checkpoint); err != nil {
				t.Fatal("idempotent sweep", err)
			}
			if policy == manifestcrypto.LocalRequired {
				bad := *cfg
				bad.Manifest = cfg.Manifest
				bad.Manifest.Key = strings.Repeat("f", 64)
				t.Setenv("MANIFEST_KEY", bad.Manifest.Key)
				invalid, err := NewProcessStorage(&bad)
				if err != nil {
					t.Fatal(err)
				}
				defer invalid.Close()
				sentinel := filepath.Join(dir, "owned.bundle.222.partial")
				if err = os.WriteFile(sentinel, []byte("previous failed operation"), 0600); err != nil {
					t.Fatal(err)
				}
				if err = invalid.CleanupCheckpoint(ctx, checkpoint); err == nil {
					t.Fatal("wrong-key keep plan allowed deletion")
				}
				if _, err = os.Stat(sentinel); err != nil {
					t.Fatal("failed plan removed owned candidate", err)
				}
			}
			t.Log("exact encrypted current roots preserved; 400 interrupted candidates removed; unknown files and failed-plan candidates preserved")
		})
	}
}

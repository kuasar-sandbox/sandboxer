package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

type checkpointRemoteProbe struct {
	inner fetch.Fetcher
	calls int
}

func (f *checkpointRemoteProbe) OpenManifest(ctx context.Context, key store.ContentKey) (fetch.Stream, error) {
	f.calls++
	if f.inner == nil {
		return nil, readerr.Mark(errors.New("test remote source unavailable"), false)
	}
	return f.inner.OpenManifest(ctx, key)
}

func checkpointBundleSelector(t *testing.T, path, raw string) string {
	t.Helper()
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		t.Fatal(err)
	}
	return (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path, DigestScheme: "manifest", Digest: ref.Path}).String()
}

func checkpointRefsSnapshot(t *testing.T, cfg *manifest.Config, storage *ProcessStorage, e string, refs []string) (Checkpoint, string) {
	t.Helper()
	ctx, dir := context.Background(), t.TempDir()
	admission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := snapshot.NewPlannedBundleSink(dir, "refs", cfg, storage.CustomerKeyFunc(), admission, refs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sink.Close(); err != nil {
			t.Error(err)
		}
	})
	encoded, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: e})
	if err != nil {
		t.Fatal(err)
	}
	source, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x63}, 4096)), []byte("{}"), []byte("{}"), encoded)
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := sink.AbsorbSnapshot(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSnapshot(ctx, s, ""); err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef(s)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ref.Path+".bundle")
	return Checkpoint{Directory: dir, SandboxID: "refs", SnapshotRef: checkpointBundleSelector(t, path, s), SandboxRef: e}, path
}

func TestCleanupCheckpointUnusedBundleLocations(t *testing.T) {
	for _, policy := range []manifestcrypto.LocalPolicy{manifestcrypto.LocalOff, manifestcrypto.LocalAuto, manifestcrypto.LocalRequired} {
		for _, snapshotRoot := range []bool{false, true} {
			role := "E"
			if snapshotRoot {
				role = "S"
			}
			t.Run(string(policy)+"/"+role, func(t *testing.T) {
				ctx := context.Background()
				cfg, _ := sourceBundleConfig(policy)
				storage, err := NewProcessStorage(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer storage.Close()
				remote := &checkpointRemoteProbe{}
				storage.fetcher.inner = remote
				root, path := bundlePublishFixtureWithRefs(t, cfg, storage, []string{"file://unused.bundle@location:unmapped"})
				info, err := storage.Inspect(ctx, path, nil)
				if err != nil {
					t.Fatalf("ordinary read must not require unused location: %v", err)
				}
				c := Checkpoint{Directory: filepath.Dir(path), SandboxID: "exact", SandboxRef: checkpointBundleSelector(t, path, info.Snapshot.SandboxRef)}
				if snapshotRoot {
					c.SnapshotRef = checkpointBundleSelector(t, path, root)
				}
				calls := 0
				c.ResolveLocation = func(string) (string, error) { calls++; return "", errors.New("unused location is unavailable") }
				obsolete := filepath.Join(c.Directory, "exact.snapshot.31.partial")
				protected := filepath.Join(c.Directory, "user.snapshot")
				for _, file := range []string{obsolete, protected} {
					if err := os.WriteFile(file, []byte("sentinel"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := storage.CleanupCheckpoint(ctx, c); err != nil {
					t.Fatalf("self-contained checkpoint cleanup: %v", err)
				}
				if calls != 0 || remote.calls != 0 {
					t.Fatalf("consulted unused location/remote: %d/%d", calls, remote.calls)
				}
				if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
					t.Fatalf("obsolete file retained: %v", err)
				}
				for _, file := range []string{path, protected} {
					if _, err := os.Stat(file); err != nil {
						t.Fatalf("required/protected file removed: %v", err)
					}
				}
			})
		}
	}
}

func TestCleanupCheckpointBundleLocationSelection(t *testing.T) {
	for _, scenario := range []string{"later-hit", "remote-fallback", "needed-unavailable", "malformed-candidate", "cancelled-resolver", "explicit-root-location"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg, _ := sourceBundleConfig(manifestcrypto.LocalOff)
			storage, err := NewProcessStorage(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			_, external := bundlePublishFixture(t, cfg, storage)
			info, err := storage.Inspect(ctx, external, nil)
			if err != nil {
				t.Fatal(err)
			}
			e := info.Snapshot.SandboxRef
			externalBefore, err := os.ReadFile(external)
			if err != nil {
				t.Fatal(err)
			}
			refs := []string{"file://missing.bundle@location:missing", "file://" + filepath.Base(external) + "@location:used", "file://unused.bundle@location:unused"}
			if scenario == "remote-fallback" || scenario == "needed-unavailable" {
				refs = refs[:1]
			}
			if scenario == "malformed-candidate" {
				refs[0] = "file://broken.bundle"
			}
			c, current := checkpointRefsSnapshot(t, cfg, storage, e, refs)
			if scenario != "remote-fallback" && scenario != "needed-unavailable" {
				c.SandboxRef = checkpointBundleSelector(t, external, e)
			}
			if scenario == "explicit-root-location" {
				r, err := manifest.ParseRef(c.SandboxRef)
				if err != nil {
					t.Fatal(err)
				}
				r.Path, r.Location = filepath.Base(external), "missing"
				c.SandboxRef = r.String()
			}
			if scenario == "malformed-candidate" {
				if err := os.WriteFile(filepath.Join(c.Directory, "broken.bundle"), []byte("not a Bundle"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			remote := &checkpointRemoteProbe{}
			if scenario == "remote-fallback" {
				opened, err := storage.OpenFile(ctx, external, manifest.Ref{Scheme: manifest.RefSchemeFile, Path: external})
				if err != nil {
					t.Fatal(err)
				}
				defer opened.Close()
				remote.inner = opened.ScopedFetcher()
			}
			storage.fetcher.inner = remote
			var locations []string
			c.ResolveLocation = func(name string) (string, error) {
				locations = append(locations, name)
				if scenario == "cancelled-resolver" {
					cancel()
					return "", ctx.Err()
				}
				if name == "used" {
					return filepath.Dir(external), nil
				}
				return "", errors.New("test location unavailable: " + name)
			}
			obsolete := filepath.Join(c.Directory, "refs.snapshot.47.partial")
			protected := filepath.Join(c.Directory, "other.snapshot.47.partial")
			for _, path := range []string{obsolete, protected} {
				if err := os.WriteFile(path, []byte("sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = storage.CleanupCheckpoint(ctx, c)
			success := scenario == "later-hit" || scenario == "remote-fallback"
			if (err == nil) != success {
				t.Fatalf("cleanup err=%v, success=%t, resolver calls=%v", err, success, locations)
			}
			if scenario == "cancelled-resolver" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if scenario == "later-hit" {
				if len(locations) < 2 || locations[0] != "missing" || locations[1] != "used" {
					t.Fatalf("wrong ordered resolution: %v", locations)
				}
				for _, name := range locations {
					if name == "unused" {
						t.Fatal("resolved later source after a hit")
					}
				}
			}
			if scenario == "malformed-candidate" && len(locations) != 0 {
				t.Fatalf("fell through malformed source: %v", locations)
			}
			if scenario == "remote-fallback" || scenario == "needed-unavailable" {
				if remote.calls == 0 {
					t.Fatal("remote fallback was not attempted")
				}
			} else if remote.calls != 0 {
				t.Fatalf("unexpected remote fallback: %d", remote.calls)
			}
			_, statErr := os.Stat(obsolete)
			if success && !os.IsNotExist(statErr) || !success && statErr != nil {
				t.Fatalf("incorrect deletion after err=%v: %v", err, statErr)
			}
			for _, file := range []string{current, protected} {
				if _, err := os.Stat(file); err != nil {
					t.Fatalf("current/protected file removed: %v", err)
				}
			}
			after, err := os.ReadFile(external)
			if err != nil || !bytes.Equal(after, externalBefore) {
				t.Fatal("external carrier changed")
			}
		})
	}
}

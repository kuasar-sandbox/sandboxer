package artifact

import (
	"bytes"
	"context"
	"errors"
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

func checkpointFixture(t *testing.T, dir string) (Checkpoint, string, []string) {
	t.Helper()
	ctx := context.Background()
	sink := snapshot.NewFileSink(dir, "sid", nil, false, nil)
	makeE := func(value byte, lower string) string {
		cfg, _ := publishPortable(t, lower)
		src, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{value}, 4096)), nil, cfg)
		if err != nil {
			t.Fatal(err)
		}
		ref, path, err := sink.AbsorbSandbox(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.CommitSandbox(ctx, ref, path); err != nil {
			t.Fatal(err)
		}
		return ref
	}
	makeS := func(value byte, e string, refs []string) string {
		cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: snapshot.SnapshotConfigVersion, SandboxRef: e, FromRefs: refs})
		if err != nil {
			t.Fatal(err)
		}
		src, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{value}, 4096)), []byte("{}"), []byte("{}"), cfg)
		if err != nil {
			t.Fatal(err)
		}
		ref, path, err := sink.AbsorbSnapshot(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.CommitSnapshot(ctx, ref, path); err != nil {
			t.Fatal(err)
		}
		return ref
	}
	oldE := makeE(1, "")
	history := makeS(1, oldE, nil)
	// A retained disk leaf with deliberately unreadable content proves that keep
	// selection doesn't hash/decrypt/read payloads or inspect candidate histories.
	leaf := strings.Repeat("d", 64) + ".overlay"
	if err := os.WriteFile(filepath.Join(dir, leaf), []byte("not an artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	e := makeE(2, "file://"+leaf+"@digest:"+strings.Repeat("d", 64))
	s := makeS(2, e, []string{history})
	name := func(raw string) string {
		r, err := manifest.ParseRef(raw)
		if err != nil {
			t.Fatal(err)
		}
		return r.Path
	}
	return Checkpoint{Directory: dir, SandboxID: "sid", SandboxRef: e, SnapshotRef: s}, name(oldE), []string{name(e), name(s), name(history), leaf, "sid.sandbox", "sid.snapshot"}
}

func TestCleanupCheckpointRolesNamesAndNoPayloadScans(t *testing.T) {
	dir := t.TempDir()
	c, obsolete, keep := checkpointFixture(t, dir)
	// These valid portable identities are host bindings, not checkpoint files.
	for _, name := range []string{"vmlinux", "runtime.bundle"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("fixture fabricated boot input %s: %v", name, err)
		}
	}
	gone := []string{obsolete, strings.Repeat("e", 64) + ".snapshot", "sid.snapshot.12.partial", "sid.bundle.4294967295.partial"}
	for _, name := range gone[1:] {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("incomplete"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tmp := ".sid.snapshot." + strings.Repeat("a", 32) + ".tmp"
	if err := os.Symlink(gone[1], filepath.Join(dir, tmp)); err != nil {
		t.Fatal(err)
	}
	gone = append(gone, tmp)
	untouched := []string{"notes.snapshot", "user.tmp", "other.snapshot.1.partial", "sid2.bundle.2.partial", "sid.bundle.01.partial", strings.Repeat("A", 64) + ".overlay"}
	for _, name := range untouched {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("user"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	unknownDir := strings.Repeat("f", 64) + ".snapshot"
	if err := os.Mkdir(filepath.Join(dir, unknownDir), 0700); err != nil {
		t.Fatal(err)
	}
	untouched = append(untouched, unknownDir)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	link := strings.Repeat("b", 64) + ".image"
	if err := os.Symlink(outside, filepath.Join(dir, link)); err != nil {
		t.Fatal(err)
	}
	untouched = append(untouched, link)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if err := storage.CleanupCheckpoint(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, name := range gone {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("obsolete %q remains: %v", name, err)
		}
	}
	for _, name := range append(keep, untouched...) {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("needed/unknown %q removed: %v", name, err)
		}
	}
	if body, err := os.ReadFile(outside); err != nil || string(body) != "safe" {
		t.Fatalf("outside target changed: %q %v", body, err)
	}
	// E-only transition retires all memory (including its fixed alias), and is
	// idempotent when previously removed candidates are already absent.
	c.SnapshotRef = ""
	for i := 0; i < 2; i++ {
		if err := storage.CleanupCheckpoint(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{keep[1], keep[2], "sid.snapshot"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("E-only retains %q: %v", name, err)
		}
	}
}

func TestCleanupCheckpointPlanFailureAndCancellationDeleteNothing(t *testing.T) {
	for _, variant := range []string{"missing-root", "mismatched-pair", "cancel", "symlink-directory", "external-root-alias"} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			c, obsolete, _ := checkpointFixture(t, dir)
			ctx := context.Background()
			switch variant {
			case "missing-root":
				c.SnapshotRef = "file://missing.snapshot@digest:" + strings.Repeat("a", 64)
			case "mismatched-pair":
				c.SandboxRef = "file://" + obsolete + "@digest:" + strings.TrimSuffix(obsolete, ".sandbox")
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "external-root-alias":
				ref, err := manifest.ParseRef(c.SnapshotRef)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, ref.Path)
				outside := filepath.Join(t.TempDir(), ref.Path)
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "symlink-directory":
				link := filepath.Join(t.TempDir(), "checkpoint")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				c.Directory = link
			}
			storage, _ := NewProcessStorage(nil)
			defer storage.Close()
			if err := storage.CleanupCheckpoint(ctx, c); err == nil {
				t.Fatal("invalid keep plan succeeded")
			}
			if _, err := os.Stat(filepath.Join(dir, obsolete)); err != nil {
				t.Fatalf("failed plan deleted previous E: %v", err)
			}
		})
	}
}

func TestCleanupCheckpointBundleMembersAndMixedCarriers(t *testing.T) {
	ctx := context.Background()
	cfg, _ := sourceBundleConfig(manifestcrypto.LocalOff)
	storage, err := NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	rootRef, bundlePath := bundlePublishFixture(t, cfg, storage)
	dir := filepath.Dir(bundlePath)
	parsed, _ := manifest.ParseRef(rootRef)
	sRef := (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: filepath.Base(bundlePath), DigestScheme: "manifest", Digest: parsed.Path}).String()
	info, err := storage.Inspect(ctx, bundlePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	eKey, _ := manifest.ParseRef(info.Snapshot.SandboxRef)
	eRef := (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: filepath.Base(bundlePath), DigestScheme: "manifest", Digest: eKey.Path}).String()
	c := Checkpoint{Directory: dir, SandboxID: "exact", SnapshotRef: sRef, SandboxRef: eRef}
	junk := filepath.Join(dir, strings.Repeat("9", 64)+".bundle")
	if err := os.WriteFile(junk, []byte("interrupted final"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := storage.CleanupCheckpoint(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Fatalf("obsolete Bundle remains: %v", err)
	}
	// Retiring S cannot remove its carrier while the E member remains current.
	alias := filepath.Join(dir, "exact.snapshot")
	if _, err := os.Lstat(alias); err != nil {
		t.Fatal("fixture needs old Snapshot alias", err)
	}
	c.SnapshotRef = ""
	if err := storage.CleanupCheckpoint(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatal("live E carrier removed", err)
	}
	if _, err := os.Lstat(alias); !os.IsNotExist(err) {
		t.Fatalf("E-only retained obsolete Snapshot alias: %v", err)
	}
	// A new tarstream root depends on the historical S memory in that same
	// carrier. Its old E is irrelevant, but the physical Bundle remains needed.
	current, _, _ := checkpointFixture(t, dir)
	sink := snapshot.NewFileSink(dir, "exact", nil, false, nil)
	sc, err := snapshot.MarshalConfig(&snapshot.Config{Version: snapshot.SnapshotConfigVersion, SandboxRef: current.SandboxRef, FromRefs: []string{sRef}})
	if err != nil {
		t.Fatal(err)
	}
	src, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{3}, 4096)), []byte("{}"), []byte("{}"), sc)
	if err != nil {
		t.Fatal(err)
	}
	next, path, err := sink.AbsorbSnapshot(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSnapshot(ctx, next, path); err != nil {
		t.Fatal(err)
	}
	current.SandboxID = "exact"
	current.SnapshotRef = next
	if err := storage.CleanupCheckpoint(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatal("live historical memory carrier removed", err)
	}
	current.SnapshotRef = ""
	if err := storage.CleanupCheckpoint(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bundlePath); !os.IsNotExist(err) {
		t.Fatalf("unneeded Bundle remains after E-only: %v", err)
	}
}

func TestCleanupCheckpointSameBasenameExternalLocation(t *testing.T) {
	ctx := context.Background()
	owned, external := t.TempDir(), t.TempDir()
	c, _, _ := checkpointFixture(t, owned)
	leaf := strings.Repeat("d", 64) + ".overlay"
	if err := os.WriteFile(filepath.Join(external, leaf), []byte("external, different bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	located := (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: leaf, Location: "shared", DigestScheme: "digest", Digest: strings.Repeat("d", 64)}).String()
	raw, _ := publishPortable(t, located)
	src, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{4}, 4096)), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(owned, "sid", nil, false, nil)
	e, _, err := sink.AbsorbSandbox(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	c.SnapshotRef = ""
	c.SandboxRef = e
	c.ResolveLocation = func(name string) (string, error) {
		if name != "shared" {
			t.Fatalf("unexpected location %q", name)
		}
		return external, nil
	}
	storage, _ := NewProcessStorage(nil)
	defer storage.Close()
	if err := storage.CleanupCheckpoint(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(owned, leaf)); !os.IsNotExist(err) {
		t.Fatalf("external basename incorrectly retained local candidate: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(external, leaf)); err != nil || string(b) != "external, different bytes" {
		t.Fatalf("external changed %q %v", b, err)
	}
}

func TestCleanupCheckpointReaderCloseFailureDeletesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c, obsolete, _ := checkpointFixture(t, dir)
	runtime, _ := publishPortable(t, "")
	source, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{7}, 4096)), nil, runtime)
	if err != nil {
		t.Fatal(err)
	}
	e := "manifest://" + publishTestSHA
	key, _ := manifest.ParseKeyRef(e)
	failure := errors.New("injected keep reader Close failure")
	spy := &reportSpyFetcher{source: source, key: key, closeErr: failure}
	storage := &ProcessStorage{fetcher: &onDemandManifestFetcher{inner: spy}}
	sc, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: e})
	if err != nil {
		t.Fatal(err)
	}
	src, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{8}, 4096)), []byte("{}"), []byte("{}"), sc)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(dir, "sid", nil, false, nil)
	s, _, err := sink.AbsorbSnapshot(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	c.SnapshotRef, c.SandboxRef = s, e
	if err := storage.CleanupCheckpoint(ctx, c); !errors.Is(err, failure) {
		t.Fatalf("close failure lost: %v", err)
	}
	if spy.closes != 1 {
		t.Fatalf("E reader closes=%d", spy.closes)
	}
	if _, err := os.Stat(filepath.Join(dir, obsolete)); err != nil {
		t.Fatal("deletion preceded reader Close", err)
	}
}

func TestCleanupCheckpointDirectoryReplacementFailsBeforeUnlink(t *testing.T) {
	ctx := context.Background()
	parent, external := t.TempDir(), t.TempDir()
	dir := filepath.Join(parent, "checkpoint")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c, obsolete, _ := checkpointFixture(t, dir)
	leaf := strings.Repeat("d", 64) + ".overlay"
	if err := os.WriteFile(filepath.Join(external, leaf), []byte("external"), 0600); err != nil {
		t.Fatal(err)
	}
	located := (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: leaf, Location: "shared", DigestScheme: "digest", Digest: strings.Repeat("d", 64)}).String()
	raw, _ := publishPortable(t, located)
	source, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{9}, 4096)), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(dir, "sid", nil, false, nil)
	c.SandboxRef, _, err = sink.AbsorbSandbox(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	c.SnapshotRef = ""
	retired := filepath.Join(parent, "retired")
	c.ResolveLocation = func(string) (string, error) {
		// The last metadata edge resolves while the old directory is pinned.
		if err := os.Rename(dir, retired); err != nil {
			return "", err
		}
		if err := os.Mkdir(dir, 0700); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, obsolete), []byte("replacement owner"), 0600); err != nil {
			return "", err
		}
		return external, nil
	}
	storage, _ := NewProcessStorage(nil)
	defer storage.Close()
	if err := storage.CleanupCheckpoint(ctx, c); err == nil || !strings.Contains(err.Error(), "directory identity changed") {
		t.Fatalf("replacement was accepted: %v", err)
	}
	for _, root := range []string{dir, retired} {
		if _, err := os.Stat(filepath.Join(root, obsolete)); err != nil {
			t.Fatal("identity failure deleted a candidate", err)
		}
	}
}

func TestCleanupCheckpointRetiresAliasesPointingToRetainedLowers(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	c, oldE, keep := checkpointFixture(t, dir)
	history := keep[2]
	disk := "file://" + oldE + "@digest:" + strings.TrimSuffix(oldE, ".sandbox")
	raw, _ := publishPortable(t, disk)
	source, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{9}, 4096)), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(dir, "sid", nil, false, nil)
	c.SandboxRef, _, err = sink.AbsorbSandbox(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: c.SandboxRef, FromRefs: []string{"file://" + history + "@digest:" + strings.TrimSuffix(history, ".snapshot")}})
	if err != nil {
		t.Fatal(err)
	}
	src, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{10}, 4096)), []byte("{}"), []byte("{}"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.SnapshotRef, _, err = sink.AbsorbSnapshot(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	for alias, target := range map[string]string{"sid.snapshot": history, "sid.sandbox": oldE} {
		path := filepath.Join(dir, alias)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	storage, _ := NewProcessStorage(nil)
	defer storage.Close()
	if err := storage.CleanupCheckpoint(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"sid.snapshot", "sid.sandbox"} {
		if _, err := os.Lstat(filepath.Join(dir, alias)); !os.IsNotExist(err) {
			t.Fatalf("old root alias retained for lower: %s: %v", alias, err)
		}
	}
	for _, name := range []string{history, oldE} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("needed lower carrier removed", err)
		}
	}
}

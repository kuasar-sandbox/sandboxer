package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestCheckpointBundleSnapshotPublishesOrdinaryArtifacts(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "contained", true: "external"}[external], func(t *testing.T) {
			ctx := context.Background()
			cfg, customerKey := sourceBundleConfig(manifestcrypto.LocalOff)
			storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return customerKey, nil })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = storage.Close() })
			baseDir := t.TempDir()
			_, basePath := overlayBundleFixture(t, cfg, storage, baseDir)
			baseName := filepath.Base(basePath)
			parent := "file://" + baseName + "@manifest:" + strings.TrimSuffix(baseName, ".bundle") + "@location:base"
			f := newDiffCheckpointFixture(t, parent)
			admission, err := cfg.WriteAdmission(ctx)
			if err != nil {
				t.Fatal(err)
			}
			bundleDir := f.dir
			if external {
				bundleDir = t.TempDir()
			}
			sink, err := snapshot.NewPlannedBundleSink(bundleDir, "bundle", cfg, storage.CustomerKeyFunc(), admission, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			cpSource, err := sandboxfile.BuildCheckpointSource(ctx, f.cfg, f.cp)
			if err != nil {
				t.Fatal(err)
			}
			e, _, err := sink.AbsorbSandbox(ctx, cpSource)
			if err != nil {
				t.Fatal(err)
			}
			capturedE := e
			if external {
				ref, _ := manifest.ParseRef(f.ref)
				ref.Location = "source"
				e = ref.String()
			}
			raw, err := snapshot.MarshalConfig(&snapshot.Config{Version: snapshot.SnapshotConfigVersion, SandboxRef: e})
			if err != nil {
				t.Fatal(err)
			}
			ss, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x77}, 8192)), []byte("{}"), []byte("{}"), raw)
			if err != nil {
				t.Fatal(err)
			}
			sref, _, err := sink.AbsorbSnapshot(ctx, ss)
			if err != nil {
				t.Fatal(err)
			}
			if err := sink.CommitSnapshot(ctx, sref, ""); err != nil {
				t.Fatal(err)
			}
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
			key, err := manifest.ParseKeyRef(sref)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(bundleDir, manifest.HexKey(key)+".bundle")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, store := range f.stores {
				if _, err := store.WriteAt(bytes.Repeat([]byte{0xaa}, 4096), 0); err != nil {
					t.Fatal(err)
				}
			}
			openBundle := func(out, raw string) *OpenedFile {
				t.Helper()
				ref, err := manifest.ParseRef(raw)
				if err != nil || ref.Location != "published" || ref.DigestScheme != "manifest" || ref.Path != ref.Digest+".bundle" {
					t.Fatalf("expected published Manifest Bundle, got %q: %v", raw, err)
				}
				stream, err := storage.OpenFileWithLocations(ctx, filepath.Join(out, ref.Path), ref, config.RefLocations{"published": out})
				if err != nil {
					t.Fatal(err)
				}
				return stream
			}
			for _, rewrite := range []bool{false, true} {
				out := t.TempDir()
				p, err := NewLocationPublisher(storage, "published", out, config.RefLocations{"source": f.dir, "base": baseDir}, nil)
				if err != nil {
					t.Fatal(err)
				}
				var result PublishResult
				if rewrite {
					result, err = p.PublishWithOptions(ctx, path, RewriteOptions{ReduceAny: true})
				} else {
					result, err = p.Publish(ctx, path)
				}
				if err != nil {
					t.Fatal(err)
				}
				if !rewrite {
					// A subsequent tarstream input on the very same publisher
					// still selects the original named tarstream target.
					plain, err := p.Publish(ctx, f.path)
					if err != nil {
						t.Fatal(err)
					}
					tarRef, err := manifest.ParseRef(plain.Ref)
					if err != nil || filepath.Ext(tarRef.Path) != ".sandbox" || tarRef.DigestScheme != "hmac" {
						t.Fatalf("publishing a Bundle changed later tarstream output: %s (%v)", plain.Ref, err)
					}
				}
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
				if !rewrite {
					s, err := snapshotfile.Open(ctx, openBundle(out, result.Ref))
					if err != nil {
						t.Fatal(err)
					}
					sconfig, err := snapshot.ParseConfig(s.SnapshotConfig)
					if err != nil || sconfig.SandboxRef != result.SandboxRef {
						t.Fatalf("published Snapshot disagrees with reported Sandbox: %v, %v", sconfig, err)
					}
					memory := make([]byte, 8192)
					if _, err := s.Memory.ReadAt(ctx, memory, 0); err != nil || !bytes.Equal(memory, bytes.Repeat([]byte{0x77}, len(memory))) {
						t.Fatalf("published Snapshot lost captured memory: %v", err)
					}
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					if !external {
						// Select E directly, as a disk-only export does. The result must
						// also agree with the E linked from the materialized Snapshot.
						eKey, err := manifest.ParseKeyRef(capturedE)
						if err != nil {
							t.Fatal(err)
						}
						ePub, err := NewLocationPublisher(storage, "published", out, nil, nil)
						if err != nil {
							t.Fatal(err)
						}
						disk, err := ePub.Publish(ctx, "file://"+path+"@manifest:"+manifest.HexKey(eKey))
						if err := errors.Join(err, ePub.Close()); err != nil {
							t.Fatal(err)
						}
						if disk.Role != RoleSandbox || disk.Ref != result.SandboxRef || disk.SandboxRef != disk.Ref {
							t.Fatalf("disk-only publication returned %+v, expected %s", disk, result.SandboxRef)
						}
					}
				}
				ref, err := manifest.ParseRef(result.SandboxRef)
				if err != nil {
					t.Fatal(err)
				}
				stream, err := storage.OpenFileWithLocations(ctx, filepath.Join(out, ref.Path), ref, config.RefLocations{"published": out})
				if err != nil {
					t.Fatal(err)
				}
				root, err := sandboxfile.Open(ctx, stream)
				if err != nil {
					t.Fatal(err)
				}
				if root.Checkpoint != nil {
					t.Fatal("Bundle publication retained checkpoint.json")
				}
				buf := make([]byte, 4096)
				if _, err := root.Payload.ReadAt(ctx, buf, 0); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(buf, bytes.Repeat([]byte{1}, 4096)) {
					t.Fatal("Bundle publication lost pinned data")
				}
				if !rewrite {
					if !slices.Contains(root.Portable.Boot.Root.BaseFromRefs, parent) {
						t.Fatal("materialization dropped the immutable base's located selector")
					}
					for i, disk := range root.Portable.Boot.Disks {
						data := openBundle(out, disk.Base)
						_, readErr := data.ReadAt(ctx, buf, 0)
						if err := errors.Join(readErr, data.Close()); err != nil || !bytes.Equal(buf, bytes.Repeat([]byte{byte(i+2)}, len(buf))) {
							t.Fatalf("materialization lost data disk %d: %v", i, err)
						}
					}
				}
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("source Bundle changed")
			}
		})
	}
}

func TestCleanupCheckpointKeepsOnlyCurrentDiskVersions(t *testing.T) {
	ctx := context.Background()
	f := newDiffCheckpointFixture(t)
	storage, _ := NewProcessStorage(nil)
	defer storage.Close()
	for _, store := range f.stores {
		if err := store.Checkpoint(ctx, "obsolete"); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	old, err := diffstore.Create(filepath.Join(f.dir, "diffs", "orphan.diff"), diffstore.Options{DiffID: "orphan", BaseID: "old", LogicalSize: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.WriteAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if err := old.Checkpoint(ctx, "orphan-version"); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storage.CleanupCheckpoint(ctx, Checkpoint{Directory: f.dir, SandboxID: "fixture", SandboxRef: f.ref}); err != nil {
		t.Fatal(err)
	}
	refs, err := storage.ListDiffCheckpoints(ctx, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("retained versions %v", refs)
	}
	for _, ref := range refs {
		if ref.ID != f.cp.CheckpointID {
			t.Fatalf("kept obsolete version %v", ref)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "diffs", "orphan.diff")); !os.IsNotExist(err) {
		t.Fatalf("orphan DIFF not retired: %v", err)
	}
}

func TestDiffCheckpointBaseIdentityIgnoresCarrierLocation(t *testing.T) {
	key := publishTestSHA
	a := config.PortableRootConfig{Base: "manifest://" + key}
	b := config.PortableRootConfig{Base: "file://elsewhere.bundle@manifest:" + key}
	if DiffCheckpointBaseID(a) != DiffCheckpointBaseID(b) {
		t.Fatal("same manifest under another carrier changed base identity")
	}
	c := config.PortableRootConfig{Base: "file://base.overlay@digest:" + key}
	d := c
	d.Base = "file://different.overlay@digest:" + key
	if DiffCheckpointBaseID(c) != DiffCheckpointBaseID(d) {
		t.Fatal("same immutable payload under another filename changed identity")
	}
	if DiffCheckpointBaseID(a) == DiffCheckpointBaseID(c) {
		t.Fatal("distinct identity algorithms conflated")
	}
}

func TestCleanupCheckpointValidatesEveryDIFFIdentityBeforeMutation(t *testing.T) {
	ctx := context.Background()
	f := newDiffCheckpointFixture(t)
	for _, s := range f.stores {
		if err := s.Checkpoint(ctx, "obsolete"); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	wrong, err := diffstore.Create(filepath.Join(f.dir, "diffs", "wrong.diff"), diffstore.Options{DiffID: "different", LogicalSize: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.Close(); err != nil {
		t.Fatal(err)
	}
	storage, _ := NewProcessStorage(nil)
	defer storage.Close()
	if err := storage.CleanupCheckpoint(ctx, Checkpoint{Directory: f.dir, SandboxID: "fixture", SandboxRef: f.ref}); err == nil {
		t.Fatal("cleanup accepted a mismatched DIFF")
	}
	for _, id := range []string{"root-1", "data-1", "data-2"} {
		s, err := diffstore.OpenMetadata(filepath.Join(f.dir, "diffs", id+".diff"))
		if err != nil {
			t.Fatal(err)
		}
		ids, err := s.ListCheckpoints(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 2 {
			t.Fatalf("cleanup mutated before validating complete keep plan: %v", ids)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCleanupRetiredDIFFKeepsRetryUntilExistingReaderCloses(t *testing.T) {
	ctx := context.Background()
	f := newDiffCheckpointFixture(t)
	for _, s := range f.stores {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.dir, "diffs", "orphan.diff")
	s, err := diffstore.Create(path, diffstore.Options{DiffID: "orphan", LogicalSize: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt(bytes.Repeat([]byte{0x45}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx, "held"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	view, err := diffstore.OpenExport(ctx, path, "held", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	storage, _ := NewProcessStorage(nil)
	defer storage.Close()
	current := Checkpoint{Directory: f.dir, SandboxID: "fixture", SandboxRef: f.ref}
	if err := storage.CleanupCheckpoint(ctx, current); !errors.Is(err, diffstore.ErrBusy) {
		t.Fatalf("retire lost retry ownership: %v", err)
	}
	b := make([]byte, 4096)
	if _, err := view.ReadAt(ctx, b, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, bytes.Repeat([]byte{0x45}, 4096)) {
		t.Fatal("retirement damaged existing reader")
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storage.CleanupCheckpoint(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("retired DIFF still exists after retry: %v", err)
	}
}

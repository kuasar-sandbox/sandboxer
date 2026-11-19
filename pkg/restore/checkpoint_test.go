package restore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func restoreCheckpointFixture(t *testing.T, encrypted ...bool) (Options, *openedSandboxSource, []byte) {
	t.Helper()
	return restoreCheckpointDiskFixture(t, len(encrypted) > 0 && encrypted[0], 0)
}

func restoreCheckpointDiskFixture(t *testing.T, encrypted bool, dataDisks int) (Options, *openedSandboxSource, []byte) {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	pathID := "different-path-id"
	dir := filepath.Join(base, pathID, "checkpoint")
	owner, err := diffstore.LockDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	p := &config.PortableSandboxConfig{Version: 1, Resources: config.PortableResourcesConfig{Capacity: config.CapacityConfig{CPU: 1, Memory: "1GiB"}, Allocatable: config.AllocatableConfig{CPU: 1, Memory: "1GiB"}}, Boot: config.PortableBootConfig{Kernel: "file://vmlinux@digest:" + sha, Runtime: "file://runtime.bundle@digest:" + sha, Root: config.PortableRootConfig{Base: "self"}}, Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"}}
	for i := 0; i < dataDisks; i++ {
		name := fmt.Sprintf("data-%d", i)
		p.Boot.Disks = append(p.Boot.Disks, config.PortableDiskConfig{Name: name})
		p.Mounts = append(p.Mounts, config.MountConfig{Type: "disk", Source: name, Target: "/" + name})
	}
	p, err = p.Checkpointed("")
	if err != nil {
		t.Fatal(err)
	}
	var codec tarstream.Codec
	key := [32]byte{0x31}
	if encrypted {
		codec, err = manifestcrypto.NewTarStreamCodec(key)
		if err != nil {
			t.Fatal(err)
		}
	}
	cp := &sandboxfile.Checkpoint{Version: 1, CheckpointID: "cp", Root: sandboxfile.CheckpointDisk{DiffID: "root"}, Disks: []sandboxfile.CheckpointNamedDisk{}}
	roots := []config.PortableRootConfig{p.Boot.Root}
	ids := []string{"root"}
	for _, disk := range p.Boot.Disks {
		roots = append(roots, disk.PortableRootConfig)
		ids = append(ids, disk.Name)
		cp.Disks = append(cp.Disks, sandboxfile.CheckpointNamedDisk{Name: disk.Name, DiffID: disk.Name})
	}
	contents := bytes.Repeat([]byte{0x46}, 8192)
	binary.LittleEndian.PutUint16(contents[1024+0x38:], 0xef53)
	for i, root := range roots {
		storeOpts := diffstore.Options{DiffID: ids[i], BaseID: artifact.DiffCheckpointBaseID(root), LogicalSize: int64(len(contents))}
		if encrypted {
			storeOpts.Key, storeOpts.RequireEncryption = &key, true
		}
		store, err := diffstore.Create(filepath.Join(dir, "diffs", ids[i]+".diff"), storeOpts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.WriteAt(contents, 0); err != nil {
			t.Fatal(err)
		}
		if err := store.Checkpoint(ctx, "cp"); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := sandboxfile.BuildCheckpointSource(ctx, p, cp)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(dir, "test", codec, codec != nil, nil)
	_, path, err := sink.AbsorbSandbox(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{BaseRoot: base, PathID: pathID, SandboxID: "sandbox-id", EnableCheckpoint: true, HostCfg: &config.SandboxConfig{}}
	if codec != nil {
		opts.LocalCodec, opts.LocalRequired = codec, true
		opts.CustomerKeyFn = func() ([32]byte, error) { return key, nil }
	}
	opened, err := openReferencedSandbox(ctx, "file://"+path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Root.Close() })
	return opts, opened, contents
}

func TestRestoreCheckpointSameDirectoryUsesPlanAndRealDIFF(t *testing.T) {
	ctx := context.Background()
	opts, source, want := restoreCheckpointFixture(t)
	opts, source, release, err := prepareRestoreCheckpoint(ctx, opts, source)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if len(opts.checkpointReopen) != 1 || opts.checkpointReopen[0] != "root" {
		t.Fatalf("no direct reopen: %v", opts.checkpointReopen)
	}
	directory := filepath.Join(opts.BaseRoot, opts.PathID, "checkpoint")
	if _, err := diffstore.LockDirectory(directory); !errors.Is(err, diffstore.ErrBusy) {
		t.Fatalf("owner not held: %v", err)
	}
	cfg := &config.SandboxConfig{}
	opts.checkpointPlan, err = sandbox.PrepareCheckpointDiskPlan(cfg, source.Root.Portable, directory, opts.checkpointReopen)
	if err != nil {
		t.Fatal(err)
	}
	if err := preflightRestoreDiskGraph(ctx, cfg, opts, source.RelativeDir, [32]byte{}); err != nil {
		t.Fatal(err)
	}
	cache, err := diffstore.NewCache(8192, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	db, closeDisk, err := reconstructDisk(ctx, opts, [32]byte{}, true, "", nil, "", cfg.Boot.Root.Diff, "overlay", func(string, ...any) {}, cache)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDisk()
	got := make([]byte, len(want))
	if _, err := db.Cow.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("reopened real DIFF content changed")
	}
	if db.DiffPath != filepath.Join(directory, "diffs", "root.diff") || db.OwnedDiff {
		t.Fatalf("wrong persistent disk path/ownership: %+v", db)
	}
}

func TestRestoreCheckpointDifferentDirectoryMaterializes(t *testing.T) {
	ctx := context.Background()
	opts, source, want := restoreCheckpointFixture(t)
	original := source
	opts.BaseRoot = t.TempDir()
	opts, source, release, err := prepareRestoreCheckpoint(ctx, opts, source)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if len(opts.checkpointReopen) != 0 || source.Root.Checkpoint != nil {
		t.Fatal("cross-directory restore did not materialize")
	}
	got := make([]byte, len(want))
	if _, err := source.Root.Payload.ReadAt(ctx, got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("materialized payload changed")
	}
	if original.Root.Checkpoint == nil {
		t.Fatal("source artifact changed")
	}
	if _, err := os.Stat(filepath.Join(original.RelativeDir, "diffs", "root.diff")); err != nil {
		t.Fatal(err)
	}
}

// Keep the import bound to the same Source contract used for checkpoint exports.
var _ sparse.Source = (*diffstore.View)(nil)

func TestRestoreEncryptedCheckpointWithoutManifestConfig(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-directory", true: "cross-directory"}[cross], func(t *testing.T) {
			ctx := context.Background()
			opts, source, want := restoreCheckpointFixture(t, true)
			if cross {
				opts.BaseRoot = t.TempDir()
			}
			opts, source, release, err := prepareRestoreCheckpoint(ctx, opts, source)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if opts.ManifestCfg != nil || opts.LocalCodec == nil || !opts.LocalRequired {
				t.Fatal("local policy changed")
			}
			if !cross {
				if len(opts.checkpointReopen) != 1 {
					t.Fatal("encrypted same-directory source did not reopen")
				}
				key, err := opts.CustomerKeyFn()
				if err != nil {
					t.Fatal(err)
				}
				cfg := &config.SandboxConfig{}
				opts.checkpointPlan, err = sandbox.PrepareCheckpointDiskPlan(cfg, source.Root.Portable, filepath.Join(opts.BaseRoot, opts.PathID, "checkpoint"), opts.checkpointReopen)
				if err != nil {
					t.Fatal(err)
				}
				if err := preflightRestoreDiskGraph(ctx, cfg, opts, source.RelativeDir, key); err != nil {
					t.Fatal(err)
				}
			} else {
				if source.Root.Checkpoint != nil {
					t.Fatal("cross-directory source was not materialized")
				}
				got := make([]byte, len(want))
				if _, err := source.Root.Payload.ReadAt(ctx, got, 0); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatal("encrypted materialized payload changed")
				}
				ref, err := manifest.ParseRef(source.SelectedRef)
				if err != nil {
					t.Fatal(err)
				}
				opened, err := artifact.OpenFile(ctx, ref.Path, ref, nil, nil, nil, nil, false)
				if err == nil {
					opened.Close()
					t.Fatal("materialization emitted plaintext despite explicit local policy")
				}
			}
		})
	}
}

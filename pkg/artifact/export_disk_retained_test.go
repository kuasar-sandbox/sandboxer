package artifact

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func exportDiskSource(t *testing.T, sink snapshot.ArtifactSink, disk config.PortableDiskConfig) (string, string) {
	t.Helper()
	_, cfg := publishPortable(t, "")
	cfg.Version = 2
	cfg.Boot.Disks = []config.PortableDiskConfig{disk}
	cfg.Mounts = []config.MountConfig{{Type: "disk", Source: disk.Name, Target: "/work"}}
	if disk.Slot != nil {
		cfg.Boot.DiskCapabilities = &config.DiskCapabilities{SlotV1: true}
	}
	body, err := config.MarshalPortableSandboxConfig(cfg)
	requireDiskIO(t, err)
	source, err := sandboxfile.BuildSource(publishSource(t, make([]byte, 4096)), nil, body)
	requireDiskIO(t, err)
	ref, path, err := sink.AbsorbSandbox(context.Background(), source)
	requireDiskIO(t, err)
	requireDiskIO(t, sink.CommitSandbox(context.Background(), ref, path))
	return ref, path
}

func TestExportDiskRetainsPublishedGraphWithoutStorageAccess(t *testing.T) {
	for _, destination := range []string{"local", "store", "named"} {
		t.Run(destination, func(t *testing.T) {
			ctx := context.Background()
			storage, err := NewProcessStorage(nil)
			requireDiskIO(t, err)
			defer storage.Close()
			probe := &retainedBundleRemoteProbe{}
			reader := storage.BorrowWithFetcher(probe)
			defer reader.Close()
			parents := []string{
				"file://parent.bundle@manifest:" + strings.Repeat("a", 64) + "@location:offline",
				"file://parent.overlay@digest:" + strings.Repeat("b", 64) + "@location:unmapped",
				"file://parent.overlay@hmac:" + strings.Repeat("c", 64) + "@location:wrong-key",
			}
			base := "manifest://" + strings.Repeat("d", 64)
			sourceDir := t.TempDir()
			sink := snapshot.NewFileSink(sourceDir, "source", nil, false, nil)
			defer sink.Close()
			ref, _ := exportDiskSource(t, sink, config.PortableDiskConfig{
				Name: "work", PureBlock: true, Slot: &config.DiskSlotConfig{},
				Binding:            &config.SlotBinding{ID: "binding", SourceID: "source", SizeBytes: 16384},
				PortableRootConfig: config.PortableRootConfig{Base: base, BaseFromRefs: parents},
			})
			output := filepath.Join(t.TempDir(), "unused")
			req := ExportDiskRequest{SourceRef: ref, SourceDir: sourceDir, DiskName: "work", OutputDir: output}
			if destination != "local" {
				req.OutputDir, req.Upload = "", true
			}
			if destination == "named" {
				req.ToRefLocation = config.RefLocations{"output": output}
			}
			var disk DiskV1
			if req.Upload {
				result, e := reader.ExportDiskWithReport(ctx, req)
				requireDiskIO(t, e)
				disk = result.Disk
				report, e := result.Report()
				requireDiskIO(t, e)
				requireReferenceSet(t, report.DiskRefs, append([]string{base}, parents...)...)
				if !report.Refs || report.RemovedRefs == nil || len(report.RemovedRefs) != 0 {
					t.Fatalf("all-retained report: %+v", report)
				}
			} else {
				disk, err = reader.ExportDisk(ctx, req)
				requireDiskIO(t, err)
			}
			requireDiskIO(t, disk.Validate())
			if disk.Base != base || !slices.Equal(disk.BaseFromRefs, parents) || *disk.Size != 16384 {
				t.Fatalf("published graph changed: %+v", disk)
			}
			if probe.calls != 0 {
				t.Fatalf("retained remote source opened %d times", probe.calls)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("all-retained graph created output: %v", err)
			}
		})
	}
}

func TestExportDiskMaterializesOnlyPrivateLayers(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprintf("named=%t", named), func(t *testing.T) {
			testExportDiskMaterializesOnlyPrivateLayers(t, named)
		})
	}
}

func testExportDiskMaterializesOnlyPrivateLayers(t *testing.T, named bool) {
	t.Helper()
	ctx := context.Background()
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer storage.Close()
	const page = 4096
	parentDir, sourceDir, output := t.TempDir(), t.TempDir(), t.TempDir()
	parent := testRefOverlay(t, parentDir, "parents", nil, bytes.Repeat([]byte{'P'}, 2*page)).String()
	sink := snapshot.NewFileSink(sourceDir, "source", nil, false, nil)
	defer sink.Close()
	data := make([]byte, 4*page)
	copy(data[page:], bytes.Repeat([]byte{'T'}, page))
	top, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(data), []sparse.Extent{{Offset: 0, Size: page}, {Offset: 3 * page, Size: page}})
	requireDiskIO(t, err)
	ref, _ := exportDiskSource(t, sink, config.PortableDiskConfig{Name: "work", PureBlock: true, PortableRootConfig: config.PortableRootConfig{Base: top, BaseFromRefs: []string{parent}}})
	// The exporter has no mapping for the immutable parent. Only the private
	// top can be opened; its metadata provides capacity for this ordinary disk.
	req := ExportDiskRequest{SourceRef: ref, SourceDir: sourceDir, DiskName: "work", OutputDir: output}
	locations := config.RefLocations{"parents": parentDir}
	if named {
		req.OutputDir, req.Upload = "", true
		req.ToRefLocation = config.RefLocations{"output": output}
		locations["output"] = output
	}
	disk, err := storage.ExportDisk(ctx, req)
	requireDiskIO(t, err)
	if !slices.Equal(disk.BaseFromRefs, []string{parent}) {
		t.Fatalf("retained parent changed: %+v", disk)
	}
	base, err := manifest.ParseRef(disk.Base)
	requireDiskIO(t, err)
	path, err := locations.ResolveFile(base, "")
	requireDiskIO(t, err)
	if filepath.Dir(path) != output || base.DigestScheme != "manifest" || named && base.Location != "output" {
		t.Fatalf("new top was not published to the selected output: %s", disk.Base)
	}
	files, err := os.ReadDir(output)
	requireDiskIO(t, err)
	if len(files) != 1 {
		t.Fatalf("materialized %d files, want only the private top Bundle", len(files))
	}
	requireDiskIO(t, os.RemoveAll(sourceDir))
	got := readExportedDisk(t, storage, disk, locations)
	want := bytes.Clone(data)
	copy(want, bytes.Repeat([]byte{'P'}, page))
	if !bytes.Equal(got, want) {
		t.Fatal("mixed graph changed inherited, modified, explicit-zero or short-parent bytes")
	}
}

func TestExportDiskRetainsSelectedNamedBundleWithoutReadingPayload(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("borrowed=%t", borrowed), func(t *testing.T) {
			testExportDiskRetainsSelectedNamedBundle(t, borrowed, false)
		})
	}
}

func TestExportDiskReportSelectedNamedBundle(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("borrowed=%t", borrowed), func(t *testing.T) { testExportDiskRetainsSelectedNamedBundle(t, borrowed, true) })
	}
}

func testExportDiskRetainsSelectedNamedBundle(t *testing.T, borrowed, withReport bool) {
	t.Helper()
	ctx := context.Background()
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer storage.Close()
	parentDir, sourceDir, output := t.TempDir(), t.TempDir(), t.TempDir()
	parent, parentPath := overlayBundleFixture(t, cfg, storage, parentDir)
	original, err := os.ReadFile(parentPath)
	requireDiskIO(t, err)
	corrupted := bytes.Clone(original)
	corruptBundleChunk(t, corrupted)
	requireDiskIO(t, os.WriteFile(parentPath, corrupted, 0o644))
	named := "file://" + filepath.Base(parentPath) + "@location:parents"
	admission, err := cfg.WriteAdmission(ctx)
	requireDiskIO(t, err)
	sink, err := snapshot.NewPlannedBundleSink(sourceDir, "source", cfg, storage.CustomerKeyFunc(), admission, []string{named}, nil)
	requireDiskIO(t, err)
	defer sink.Close()
	data := make([]byte, 8192)
	copy(data[4096:], bytes.Repeat([]byte{'T'}, 4096))
	top, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(data), []sparse.Extent{{Offset: 0, Size: 4096}})
	requireDiskIO(t, err)
	root, _ := exportDiskSource(t, sink, config.PortableDiskConfig{Name: "work", PureBlock: true, PortableRootConfig: config.PortableRootConfig{Base: top, BaseFromRefs: []string{parent}}})
	requireDiskIO(t, sink.Close())
	rootKey, err := manifest.ParseKeyRef(root)
	requireDiskIO(t, err)
	sourcePath := filepath.Join(sourceDir, manifest.HexKey(rootKey)+".bundle")
	locations := config.RefLocations{"parents": parentDir}
	reader, sourceRef := storage, sourcePath
	if borrowed {
		bundle, err := storage.OpenFileWithLocations(ctx, sourcePath, manifest.Ref{Scheme: manifest.RefSchemeFile, Path: sourcePath}, locations)
		requireDiskIO(t, err)
		defer bundle.Close()
		reader, sourceRef = storage.BorrowWithFetcher(bundle.ScopedFetcher()), root
		defer reader.Close()
	}
	req := ExportDiskRequest{SourceRef: sourceRef, DiskName: "work", OutputDir: output, RefLocations: locations}
	var disk DiskV1
	if withReport {
		req.Upload, req.OutputDir, req.ToRefLocation = true, "", config.RefLocations{"output": output}
		result, e := reader.ExportDiskWithReport(ctx, req)
		requireDiskIO(t, e)
		disk = result.Disk
		report, e := result.Report()
		requireDiskIO(t, e)
		requireReferenceSet(t, report.DiskRefs, append([]string{disk.Base}, disk.BaseFromRefs...)...)
		wantRemoved := 1
		if borrowed {
			wantRemoved = 0
		} // borrowed private carrier has no public old-file identity
		if len(report.RemovedRefs) != wantRemoved {
			t.Fatalf("selected Bundle delta=%v", report.RemovedRefs)
		}
		locations["output"] = output
	} else {
		disk, err = reader.ExportDisk(ctx, req)
		requireDiskIO(t, err)
	}
	parentKey, err := manifest.ParseKeyRef(parent)
	requireDiskIO(t, err)
	wantParent := bundleMemberRef(named, parentKey)
	if !slices.Equal(disk.BaseFromRefs, []string{wantParent}) {
		t.Fatalf("selected named Bundle binding lost: %+v, want %s", disk, wantParent)
	}
	if _, err := os.Stat(filepath.Join(output, filepath.Base(parentPath))); !os.IsNotExist(err) {
		t.Fatalf("named parent was copied: %v", err)
	}
	// A consumer still needs the selected parent's authentic data. Restoring
	// those bytes, then deleting only the source sandbox, makes the disk usable.
	requireDiskIO(t, os.WriteFile(parentPath, original, 0o644))
	requireDiskIO(t, os.RemoveAll(sourceDir))
	got := readExportedDisk(t, storage, disk, locations)
	copy(data, bytes.Repeat([]byte{0x39}, 4096))
	if !bytes.Equal(got, data) {
		t.Fatal("named parent inheritance changed")
	}
}

func TestExportDiskLocalLayersFromBorrowedBundle(t *testing.T) {
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer storage.Close()
	reader, root, sourceDir := borrowedDiskBundle(t, storage)
	disk, err := reader.ExportDisk(context.Background(), ExportDiskRequest{SourceRef: root, DiskName: "work", OutputDir: t.TempDir()})
	requireDiskIO(t, err)
	requireDiskIO(t, os.RemoveAll(sourceDir))
	got := readExportedDisk(t, storage, disk)
	if !bytes.Equal(got[:4096], bytes.Repeat([]byte{'P'}, 4096)) || !bytes.Equal(got[4096:], bytes.Repeat([]byte{'T'}, 4096)) {
		t.Fatal("private Bundle was mistaken for published Store content")
	}
}

func TestExportDiskTransformsPublishedLegacySandboxLayer(t *testing.T) {
	ctx := context.Background()
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer storage.Close()
	parentDir, sourceDir := t.TempDir(), t.TempDir()
	raw, _ := testRefSandbox(t, parentDir, "", nil)
	parent := locatedTestRef(t, raw, "parents").String()
	sink := snapshot.NewFileSink(sourceDir, "source", nil, false, nil)
	defer sink.Close()
	ref, _ := exportDiskSource(t, sink, config.PortableDiskConfig{Name: "work", PortableRootConfig: config.PortableRootConfig{Base: parent}})
	disk, err := storage.ExportDisk(ctx, ExportDiskRequest{SourceRef: ref, SourceDir: sourceDir, DiskName: "work", OutputDir: t.TempDir(), RefLocations: config.RefLocations{"parents": parentDir}})
	requireDiskIO(t, err)
	if disk.Base == parent || *disk.Size != 8192 {
		t.Fatalf("legacy Sandbox bytes retained as pure disk: %+v", disk)
	}
	requireDiskIO(t, os.RemoveAll(parentDir))
	requireDiskIO(t, os.RemoveAll(sourceDir))
	if !bytes.Equal(readExportedDisk(t, storage, disk), bytes.Repeat([]byte{7}, 8192)) {
		t.Fatal("legacy Sandbox payload conversion changed disk content")
	}
}

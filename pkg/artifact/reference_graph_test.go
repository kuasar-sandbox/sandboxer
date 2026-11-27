package artifact

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func requireReferenceSet(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("references=%v want=%v", got, want)
	}
}

func TestPublishReferenceGroupsUseFinalRolesAndRetainedE(t *testing.T) {
	ctx := context.Background()
	storage, err := NewProcessStorage(plaintextRefConfig())
	requireDiskIO(t, err)
	defer storage.Close()
	dir := t.TempDir()
	sink := snapshot.NewFileSink(dir, "groups", nil, false, nil)
	defer sink.Close()
	shared := reportRef("c")
	disk := "file://offline.bundle@manifest:" + publishTestSHA + "@location:offline"
	_, cfg := publishPortable(t, shared)
	cfg.Version = 2
	cfg.Boot.Disks = []config.PortableDiskConfig{{Name: "work", PureBlock: true, PortableRootConfig: config.PortableRootConfig{Base: disk}}}
	cfg.Mounts = append(cfg.Mounts, config.MountConfig{Type: "disk", Source: "work", Target: "/work"})
	body := bytes.Repeat([]byte{0x53}, 4096)
	e := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
	publisher := rewriteTarget(t, storage, t.TempDir())
	for _, retainedE := range []bool{false, true} {
		sourceE := e
		if retainedE {
			published, err := publisher.Publish(ctx, "file://"+filepath.Join(dir, mustRefPath(t, e)))
			requireDiskIO(t, err)
			sourceE = published.Ref
		}
		s := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: sourceE, FromRefs: []string{shared}}, rewriteSparse(t, body))
		result, err := publisher.Publish(ctx, "file://"+filepath.Join(dir, mustRefPath(t, s)))
		requireDiskIO(t, err)
		report, err := result.Report()
		requireDiskIO(t, err)
		if !report.Refs {
			t.Fatal("ordinary publication did not report complete groups")
		}
		requireReferenceSet(t, report.SnapshotRefs, result.Ref, shared)
		requireReferenceSet(t, report.SandboxRefs, result.SandboxRef, shared, disk)
		if retainedE && result.SandboxRef != sourceE {
			t.Fatal("retained E was replaced")
		}
		for _, removed := range report.RemovedRefs {
			if removed == shared || removed == disk || removed == result.SandboxRef {
				t.Fatalf("retained reference removed: %s", removed)
			}
		}
	}
}

func mustRefPath(t *testing.T, raw string) string {
	t.Helper()
	ref, err := manifest.ParseRef(raw)
	requireDiskIO(t, err)
	return ref.Path
}

func TestPublishReportRetainedBundleEIncludesMemberBindings(t *testing.T) {
	ctx := context.Background()
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer storage.Close()
	eDir, sDir, out := t.TempDir(), t.TempDir(), t.TempDir()
	admission, err := cfg.WriteAdmission(ctx)
	requireDiskIO(t, err)
	eSink, err := snapshot.NewPlannedBundleSink(eDir, "parent", cfg, storage.CustomerKeyFunc(), admission, []string{"file://unused.bundle@location:offline"}, nil)
	requireDiskIO(t, err)
	layer, _, err := eSink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{7}, 4096)), nil)
	requireDiskIO(t, err)
	runtime, _ := publishPortable(t, layer)
	eSource, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{9}, 4096)), nil, runtime)
	requireDiskIO(t, err)
	eManifest, _, err := eSink.AbsorbSandbox(ctx, eSource)
	requireDiskIO(t, err)
	requireDiskIO(t, eSink.CommitSandbox(ctx, eManifest, ""))
	requireDiskIO(t, eSink.Close())
	eKey, err := manifest.ParseKeyRef(eManifest)
	requireDiskIO(t, err)
	carrier := "file://" + manifest.HexKey(eKey) + ".bundle@location:parents"
	eRef := bundleMemberRef(carrier, eKey)
	sink := snapshot.NewFileSink(sDir, "source", nil, false, nil)
	defer sink.Close()
	s := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: eRef}, rewriteSparse(t, bytes.Repeat([]byte{1}, 4096)))
	publisher, err := NewLocationPublisher(storage, "output", out, config.RefLocations{"parents": eDir}, nil)
	requireDiskIO(t, err)
	defer publisher.Close()
	result, err := publisher.Publish(ctx, "file://"+filepath.Join(sDir, mustRefPath(t, s)))
	requireDiskIO(t, err)
	report, err := result.Report()
	requireDiskIO(t, err)
	layerKey, err := manifest.ParseKeyRef(layer)
	requireDiskIO(t, err)
	requireReferenceSet(t, report.SandboxRefs, eRef, bundleMemberRef(carrier, layerKey))
	requireReferenceSet(t, report.SnapshotRefs, result.Ref)
	if result.SandboxRef != eRef {
		t.Fatal("retained E binding changed")
	}
}

func TestPublishReportExactBundleGroupsUseSelectedCarriers(t *testing.T) {
	ctx := context.Background()
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
	requireDiskIO(t, err)
	defer storage.Close()
	parentDir, sourceDir, output := t.TempDir(), t.TempDir(), t.TempDir()
	parent, parentPath := overlayBundleFixture(t, cfg, storage, parentDir)
	parentCarrier := "file://" + filepath.Base(parentPath) + "@location:parents"
	admission, err := cfg.WriteAdmission(ctx)
	requireDiskIO(t, err)
	sink, err := snapshot.NewPlannedBundleSink(sourceDir, "graph", cfg, storage.CustomerKeyFunc(), admission,
		[]string{parentCarrier, "file://unused.bundle@location:unavailable"}, nil)
	requireDiskIO(t, err)
	localDisk, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{4}, 4096)), nil)
	requireDiskIO(t, err)
	_, portable := publishPortable(t, parent)
	portable.Boot.Root.BaseFromRefs = []string{localDisk, parent}
	runtime, err := config.MarshalPortableSandboxConfig(portable)
	requireDiskIO(t, err)
	eSource, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{8}, 4096)), nil, runtime)
	requireDiskIO(t, err)
	eRef, _, err := sink.AbsorbSandbox(ctx, eSource)
	requireDiskIO(t, err)
	// Historical S is a memory payload only. Its unrelated E and memory graph
	// are deliberately unavailable and must not appear in the final report.
	addSnapshot := func(sc *snapshot.Config, value byte) string {
		metadata, err := snapshot.MarshalConfig(sc)
		requireDiskIO(t, err)
		source, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{value}, 4096)), []byte("{}"), []byte("{}"), metadata)
		requireDiskIO(t, err)
		ref, _, err := sink.AbsorbSnapshot(ctx, source)
		requireDiskIO(t, err)
		return ref
	}
	oldMemory := addSnapshot(&snapshot.Config{Version: 1, SandboxRef: reportRef("d"), FromRefs: []string{reportRef("e")}}, 1)
	root := addSnapshot(&snapshot.Config{Version: 1, SandboxRef: eRef, FromRefs: []string{oldMemory}}, 2)
	requireDiskIO(t, sink.CommitSnapshot(ctx, root, ""))
	requireDiskIO(t, sink.Close())
	rootKey, err := manifest.ParseKeyRef(root)
	requireDiskIO(t, err)
	sourcePath := filepath.Join(sourceDir, manifest.HexKey(rootKey)+".bundle")
	publisher, err := NewLocationPublisher(storage, "output", output, config.RefLocations{"parents": parentDir}, nil)
	requireDiskIO(t, err)
	defer publisher.Close()
	result, err := publisher.Publish(ctx, sourcePath)
	requireDiskIO(t, err)
	report, err := result.Report()
	requireDiskIO(t, err)
	eKey, err := manifest.ParseKeyRef(eRef)
	requireDiskIO(t, err)
	diskKey, err := manifest.ParseKeyRef(localDisk)
	requireDiskIO(t, err)
	parentKey, err := manifest.ParseKeyRef(parent)
	requireDiskIO(t, err)
	memoryKey, err := manifest.ParseKeyRef(oldMemory)
	requireDiskIO(t, err)
	requireReferenceSet(t, report.SnapshotRefs, result.Ref, bundleMemberRef(result.Ref, memoryKey))
	requireReferenceSet(t, report.SandboxRefs, bundleMemberRef(result.Ref, eKey), bundleMemberRef(result.Ref, diskKey), bundleMemberRef(parentCarrier, parentKey))
	assertRemoved(t, result, "file://"+sourcePath, bundleMemberRef("file://"+sourcePath, eKey), bundleMemberRef("file://"+sourcePath, diskKey), bundleMemberRef("file://"+sourcePath, memoryKey))
}

func TestPublishReportSkippedBundleAliasUsesOnlyKnownCarrier(t *testing.T) {
	for _, current := range []bool{false, true} {
		t.Run(map[bool]string{false: "unselected-fallback", true: "current-member"}[current], func(t *testing.T) {
			ctx := context.Background()
			cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
			storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return key, nil })
			requireDiskIO(t, err)
			defer storage.Close()
			parent, parentPath := overlayBundleFixture(t, cfg, storage, t.TempDir())
			parentCarrier := "file://" + filepath.Base(parentPath) + "@location:unavailable-parent"
			sourceDir, replacementDir := t.TempDir(), t.TempDir()
			admission, err := cfg.WriteAdmission(ctx)
			requireDiskIO(t, err)
			sink, err := snapshot.NewPlannedBundleSink(sourceDir, "skip", cfg, storage.CustomerKeyFunc(), admission, []string{parentCarrier}, nil)
			requireDiskIO(t, err)
			old := parent
			if current {
				old, _, err = sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{3}, 4096)), nil)
				requireDiskIO(t, err)
			}
			runtime, _ := publishPortable(t, old)
			esource, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{8}, 4096)), nil, runtime)
			requireDiskIO(t, err)
			root, _, err := sink.AbsorbSandbox(ctx, esource)
			requireDiskIO(t, err)
			requireDiskIO(t, sink.CommitSandbox(ctx, root, ""))
			requireDiskIO(t, sink.Close())
			rootKey, err := manifest.ParseKeyRef(root)
			requireDiskIO(t, err)
			sourcePath := filepath.Join(sourceDir, manifest.HexKey(rootKey)+".bundle")
			replacementSink := snapshot.NewFileSink(replacementDir, "replacement", nil, false, nil)
			defer replacementSink.Close()
			replacement, _, err := replacementSink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{6}, 4096)), nil)
			requireDiskIO(t, err)
			replacement = locatedTestRef(t, replacement, "replacement").String()
			// Deliberately omit the old fallback's location. Skip must not open
			// it merely to populate removedRefs, nor claim it was in the Store.
			publisher, err := NewLocationPublisher(storage, "output", t.TempDir(), config.RefLocations{"replacement": replacementDir}, nil)
			requireDiskIO(t, err)
			defer publisher.Close()
			result, err := publisher.PublishWithOptions(ctx, sourcePath, RewriteOptions{
				Replacements: []RefReplacement{{Old: old, New: replacement}}, SkipVerify: true,
			})
			requireDiskIO(t, err)
			want := []string{"file://" + sourcePath}
			if current {
				oldKey, err := manifest.ParseKeyRef(old)
				requireDiskIO(t, err)
				want = append(want, bundleMemberRef("file://"+sourcePath, oldKey))
				opened, err := storage.OpenFile(ctx, sourcePath, manifest.Ref{Scheme: manifest.RefSchemeFile, Path: sourcePath})
				requireDiskIO(t, err)
				borrowedScope := publishScope{current: opened.BundleReader(), fetcher: opened.ScopedFetcher()}
				_, known := knownScopedRef(old, borrowedScope)
				_, finalErr := publisher.boundReportReference(ctx, old, borrowedScope)
				requireDiskIO(t, opened.Close())
				if known || finalErr == nil {
					t.Fatal("borrowed Bundle without a carrier was treated as a Store source")
				}
			}
			assertRemoved(t, result, want...)
			report, err := result.Report()
			requireDiskIO(t, err)
			requireReferenceSet(t, report.SandboxRefs, result.Ref, replacement)
		})
	}
}

func TestPublishReportRejectsPreviouslyUnboundFinalAlias(t *testing.T) {
	raw := reportRef("a")
	scope := publishScope{bindings: map[string]string{raw: raw}, unbound: map[string]bool{raw: true}}
	publisher := &Publisher{}
	if _, err := publisher.boundReportReference(context.Background(), raw, scope); err == nil {
		t.Fatal("previously selected borrowed alias became a false Store reference")
	}
}

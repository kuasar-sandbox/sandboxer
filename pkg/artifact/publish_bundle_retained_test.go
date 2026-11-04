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
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func sandboxBundleWithParent(t *testing.T, cfg *manifest.Config, storage *ProcessStorage, directory, parent string, refs []string) (string, string) {
	t.Helper()
	ctx := context.Background()
	admission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := snapshot.NewPlannedBundleSink(directory, "retained", cfg, storage.CustomerKeyFunc(), admission, refs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	runtimeConfig, _ := publishPortable(t, parent)
	source, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x4a}, 8192)), nil, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := sink.AbsorbSandbox(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSandbox(ctx, root, ""); err != nil {
		t.Fatal(err)
	}
	key, err := manifest.ParseKeyRef(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(directory, manifest.HexKey(key)+".bundle")
}

type retainedBundleRemoteProbe struct{ calls int }

func (f *retainedBundleRemoteProbe) OpenManifest(context.Context, store.ContentKey) (fetch.Stream, error) {
	f.calls++
	return nil, readerr.Mark(errors.New("retained remote parent is offline"), false)
}

func TestPublishBundleRetainsUnavailableRemoteParents(t *testing.T) {
	for _, target := range []string{"location", "manifest"} {
		t.Run(target, func(t *testing.T) {
			ctx := context.Background()
			cfg, storage := manifestPublisherFixture(t)
			parent := "manifest://" + strings.Repeat("f", 64)
			_, input := sandboxBundleWithParent(t, cfg, storage, t.TempDir(), parent,
				[]string{"file://unavailable.bundle@location:offline"})
			probe := &retainedBundleRemoteProbe{}
			reader := storage.BorrowWithFetcher(probe)
			defer reader.Close()
			output := t.TempDir()
			locations := config.RefLocations{"out": output}
			var publisher *Publisher
			var err error
			if target == "location" {
				publisher, err = NewLocationPublisher(reader, "out", output, nil, nil)
			} else {
				publisher, err = NewManifestPublisher(reader, cfg, nil, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer publisher.Close()
			result, err := publisher.Publish(ctx, input)
			if err != nil {
				t.Fatalf("publish with offline remote parent: %v", err)
			}
			assertRemoved(t, result, "file://"+input)
			if probe.calls != 0 {
				t.Fatalf("publication opened retained remote parent %d times", probe.calls)
			}
			info, err := storage.Inspect(ctx, result.Ref, locations)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Sandbox.Boot.Root.BaseFromRefs; !slices.Equal(got, []string{parent}) {
				t.Fatalf("retained remote parent changed: %v", got)
			}
		})
	}
}

func TestPublishBundleRetainsNamedParentPayload(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	parentDirectory := t.TempDir()
	parent, parentPath := overlayBundleFixture(t, cfg, storage, parentDirectory)
	original, err := os.ReadFile(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := bytes.Clone(original)
	corruptBundleChunk(t, corrupted)
	if err := os.WriteFile(parentPath, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}
	inputDirectory := t.TempDir()
	const fallbackName = "fallback.bundle"
	if err := os.WriteFile(filepath.Join(inputDirectory, fallbackName), original, 0o644); err != nil {
		t.Fatal(err)
	}
	named := "file://" + filepath.Base(parentPath) + "@location:parent"
	refs := []string{named, "file://" + fallbackName}
	_, input := sandboxBundleWithParent(t, cfg, storage, inputDirectory, parent, refs)
	output := t.TempDir()
	locations := config.RefLocations{"parent": parentDirectory, "out": output}
	publisher, err := NewLocationPublisher(storage, "out", output, locations, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	result, err := publisher.Publish(ctx, input)
	if err != nil {
		t.Fatalf("retain named parent with corrupt payload: %v", err)
	}
	assertRemoved(t, result, "file://"+input)
	if _, err := os.Stat(filepath.Join(output, filepath.Base(parentPath))); !os.IsNotExist(err) {
		t.Fatalf("retained named parent was copied: %v", err)
	}
	info, err := storage.Inspect(ctx, result.Ref, locations)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Sandbox.Boot.Root.BaseFromRefs; !slices.Equal(got, []string{parent}) {
		t.Fatalf("retained named parent changed: %v", got)
	}
	ref, err := manifest.ParseRef(result.Ref)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := storage.OpenFileWithLocations(ctx, filepath.Join(output, ref.Path), ref, locations)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if got := opened.BundleReader().Refs(); !slices.Equal(got, refs) {
		t.Fatalf("Bundle source order changed: %v", got)
	}
	key, err := manifest.ParseKeyRef(parent)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := opened.ManifestFetcher().SelectManifest(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Ref != named {
		t.Fatalf("retained parent source = %q, want %q", selected.Ref, named)
	}
	// A real read still uses the selected named source and rejects corruption;
	// it must not switch to the later valid copy of the same Manifest.
	stream, readErr := selected.OpenManifest(ctx, key)
	if readErr == nil {
		_, readErr = stream.ReadAt(ctx, make([]byte, 8192), 0)
		readErr = errors.Join(readErr, stream.Close())
	}
	if readErr == nil {
		t.Fatal("actual read accepted corrupt named parent or changed source")
	}

	// Manifest publication copies the named Bundle's selected closure. That
	// transfer must still authenticate the bytes it uploads.
	uploader, err := NewManifestPublisher(storage, cfg, locations, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer uploader.Close()
	if _, err := uploader.Publish(ctx, input); err == nil {
		t.Fatal("Manifest upload accepted corrupt named parent")
	}
	if err := os.WriteFile(parentPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := uploader.Publish(ctx, input); err != nil {
		t.Fatalf("Manifest upload of restored parent: %v", err)
	}
}

func TestLocationPublisherVerifiesCopiedBundleParents(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	directory := t.TempDir()
	parent, parentPath := overlayBundleFixture(t, cfg, storage, directory)
	original, err := os.ReadFile(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := bytes.Clone(original)
	corruptBundleChunk(t, corrupted)
	if err := os.WriteFile(parentPath, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}
	_, input := sandboxBundleWithParent(t, cfg, storage, directory, parent,
		[]string{"file://" + filepath.Base(parentPath)})
	output := t.TempDir()
	publisher, err := NewLocationPublisher(storage, "out", output, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	if _, err := publisher.Publish(ctx, input); err == nil {
		t.Fatal("location copy accepted corrupt selected parent")
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed source verification exposed output: %v", entries)
	}
	if err := os.WriteFile(parentPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(ctx, input); err != nil {
		t.Fatalf("location copy of restored parent: %v", err)
	}
}

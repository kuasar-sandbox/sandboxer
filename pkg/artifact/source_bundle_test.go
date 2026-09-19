package artifact

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

func sourceBundleConfig(policy manifestcrypto.LocalPolicy) (*config.ManifestConfig, [32]byte) {
	key := [32]byte{0x31, 0x32, 0x33, 0x34}
	return &config.ManifestConfig{
		Manifest: manifest.ManifestSubConfig{Key: hex.EncodeToString(key[:]), WriteGeneration: "G1"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes", Local: policy},
	}, key
}

func sourceBundleFlattenedImage(t *testing.T) ([]byte, []byte) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "image.img")
	if err := os.WriteFile(path, publishEROFSFixture(), 0o600); err != nil {
		t.Fatal(err)
	}
	imageConfig := &image.RuntimeConfig{
		Cmd: []string{"/bin/app", "serve"}, Env: []string{"A=B"}, WorkingDir: "/srv",
	}
	if err := image.AppendConfigZip(path, imageConfig); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := sandboxfile.OpenFlattenedEROFS(context.Background(), &sourceBundleStream{Source: publishSource(t, body)})
	if err != nil {
		t.Fatal(err)
	}
	rawConfig := append([]byte(nil), opened.ImageConfig...)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	return body, rawConfig
}

func sourceBundleSandbox(t *testing.T, imageConfig []byte) sparse.Source {
	t.Helper()
	_, portable := publishPortable(t, "")
	portable.Boot.Root = config.PortableRootConfig{Base: "self", Overlay: &config.PortableOverlayConfig{}}
	runtimeConfig, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	source, err := sandboxfile.BuildSource(publishSource(t, publishEROFSFixture()), imageConfig, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

type sourceBundleStream struct {
	sparse.Source
}

func (*sourceBundleStream) Close() error { return nil }

type closeTrackingSource struct {
	sparse.Source
	closed bool
}

func (s *closeTrackingSource) Close() error {
	s.closed = true
	return nil
}

type failingBundleSource struct {
	sparse.Source
	err error
}

func (s *failingBundleSource) RunAt(uint64, uint64) (sparse.Run, error) {
	return nil, s.err
}

func (s *failingBundleSource) ReadAt(context.Context, []byte, uint64) (int, error) {
	return 0, s.err
}

func newSourceBundlePublisher(t *testing.T, cfg *config.ManifestConfig, key [32]byte, directory string) *Publisher {
	t.Helper()
	admission, err := cfg.WriteAdmission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewSingleRootBundlePublisher(cfg, func() ([32]byte, error) { return key, nil }, admission, "release", directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

func TestSingleRootBundlePublishesImageAndSandboxRoles(t *testing.T) {
	flattened, imageConfig := sourceBundleFlattenedImage(t)
	for _, policy := range []manifestcrypto.LocalPolicy{
		manifestcrypto.LocalOff, manifestcrypto.LocalAuto, manifestcrypto.LocalRequired,
	} {
		for _, test := range []struct {
			name   string
			role   LogicalRole
			source func() sparse.Source
			open   func(context.Context, *OpenedFile) error
		}{
			{
				name: "image", role: RoleImage,
				source: func() sparse.Source { return publishSource(t, flattened) },
				open: func(ctx context.Context, stream *OpenedFile) error {
					image, err := sandboxfile.OpenFlattenedEROFS(ctx, stream)
					if err != nil {
						return err
					}
					if !bytes.Equal(image.ImageConfig, imageConfig) {
						return errors.New("image config changed")
					}
					return image.Close()
				},
			},
			{
				name: "sandbox", role: RoleSandbox,
				source: func() sparse.Source { return sourceBundleSandbox(t, imageConfig) },
				open: func(ctx context.Context, stream *OpenedFile) error {
					root, err := sandboxfile.Open(ctx, stream)
					if err != nil {
						return err
					}
					if root.Portable.Boot.Root.Base != "self" || root.Portable.Boot.Root.Overlay == nil {
						return errors.New("Sandbox root is not the direct EROFS self layout")
					}
					return root.Close()
				},
			},
		} {
			t.Run(string(policy)+"/"+test.name, func(t *testing.T) {
				cfg, key := sourceBundleConfig(policy)
				directory := t.TempDir()
				publisher := newSourceBundlePublisher(t, cfg, key, directory)
				result, err := publisher.PublishSource(context.Background(), test.role, test.source())
				if err != nil {
					t.Fatal(err)
				}
				if result.Role != test.role {
					t.Fatalf("role = %q, want %q", result.Role, test.role)
				}
				ref, err := manifest.ParseRef(result.Ref)
				if err != nil {
					t.Fatal(err)
				}
				if ref.Location != "release" || ref.DigestScheme != "manifest" || filepath.Ext(ref.Path) != ".bundle" || strings.TrimSuffix(ref.Path, ".bundle") != ref.Digest {
					t.Fatalf("located Bundle ref = %#v", ref)
				}
				entries, err := os.ReadDir(directory)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 || entries[0].Name() != ref.Path {
					t.Fatalf("location entries = %v, want only %s", entries, ref.Path)
				}
				bundleReader, err := manifestbundle.Open(filepath.Join(directory, ref.Path))
				if err != nil {
					t.Fatal(err)
				}
				expectedAdmission, err := cfg.WriteAdmission(context.Background())
				if err != nil {
					_ = bundleReader.Close()
					t.Fatal(err)
				}
				manifestKeys := bundleReader.ManifestKeys()
				if bundleReader.Admission() != expectedAdmission || len(bundleReader.Refs()) != 0 || len(manifestKeys) != 1 || manifest.HexKey(manifestKeys[0]) != ref.Digest {
					_ = bundleReader.Close()
					t.Fatalf("single-root Bundle metadata: admission=%+v refs=%v manifests=%v", bundleReader.Admission(), bundleReader.Refs(), manifestKeys)
				}
				if err := bundleReader.Close(); err != nil {
					t.Fatal(err)
				}
				storage, err := NewProcessStorage(cfg)
				if err != nil {
					t.Fatal(err)
				}
				opened, err := storage.OpenFileWithLocations(context.Background(), filepath.Join(directory, ref.Path), ref, config.RefLocations{"release": directory})
				if err != nil {
					_ = storage.Close()
					t.Fatal(err)
				}
				if err := test.open(context.Background(), opened); err != nil {
					_ = storage.Close()
					t.Fatal(err)
				}
				if err := storage.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSingleRootBundleReusesValidFinalAndConvergesConcurrentWriters(t *testing.T) {
	flattened, _ := sourceBundleFlattenedImage(t)
	for _, policy := range []manifestcrypto.LocalPolicy{
		manifestcrypto.LocalOff, manifestcrypto.LocalAuto, manifestcrypto.LocalRequired,
	} {
		t.Run(string(policy), func(t *testing.T) {
			cfg, key := sourceBundleConfig(policy)
			directory := t.TempDir()
			firstPublisher := newSourceBundlePublisher(t, cfg, key, directory)
			first, err := firstPublisher.PublishSource(context.Background(), RoleImage, publishSource(t, flattened))
			if err != nil {
				t.Fatal(err)
			}
			ref, err := manifest.ParseRef(first.Ref)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, ref.Path)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			reusePublisher := newSourceBundlePublisher(t, cfg, key, directory)
			reused, err := reusePublisher.PublishSource(context.Background(), RoleImage, publishSource(t, flattened))
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if reused.Ref != first.Ref || !os.SameFile(before, after) {
				t.Fatal("valid content-addressed Bundle was not reused")
			}

			const writers = 6
			publishers := make([]*Publisher, writers)
			sources := make([]sparse.Source, writers)
			for index := range writers {
				publishers[index] = newSourceBundlePublisher(t, cfg, key, directory)
				sources[index] = publishSource(t, flattened)
			}
			refs := make(chan string, writers)
			errs := make(chan error, writers)
			var group sync.WaitGroup
			for index := range writers {
				group.Add(1)
				go func() {
					defer group.Done()
					result, err := publishers[index].PublishSource(context.Background(), RoleImage, sources[index])
					if err != nil {
						errs <- err
						return
					}
					refs <- result.Ref
				}()
			}
			group.Wait()
			close(refs)
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			for got := range refs {
				if got != first.Ref {
					t.Fatalf("concurrent ref = %q, want %q", got, first.Ref)
				}
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != ref.Path {
				t.Fatalf("concurrent location entries = %v", entries)
			}
		})
	}
}

func TestSingleRootBundleRejectsInvalidExistingFinals(t *testing.T) {
	flattened, _ := sourceBundleFlattenedImage(t)
	for _, kind := range []string{"corrupt", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
			directory := t.TempDir()
			publisher := newSourceBundlePublisher(t, cfg, key, directory)
			first, err := publisher.PublishSource(context.Background(), RoleImage, publishSource(t, flattened))
			if err != nil {
				t.Fatal(err)
			}
			ref, err := manifest.ParseRef(first.Ref)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, ref.Path)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "corrupt":
				if err := os.WriteFile(path, []byte("corrupt Bundle"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(directory, "other")
				if err := os.WriteFile(target, []byte("not the Bundle"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			retry := newSourceBundlePublisher(t, cfg, key, directory)
			target := retry.target.(*singleRootBundleTarget)
			targetRetry := locationRetryPolicy{window: 20 * time.Millisecond, initial: time.Millisecond, maximum: 2 * time.Millisecond}
			target.retry = targetRetry
			result, err := retry.PublishSource(context.Background(), RoleImage, publishSource(t, flattened))
			if err == nil || result.Ref != "" {
				t.Fatalf("invalid final result = %+v, err=%v", result, err)
			}
		})
	}
}

func TestSingleRootBundlePreservesReplacementFinalInode(t *testing.T) {
	flattened, _ := sourceBundleFlattenedImage(t)
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	directory := t.TempDir()
	publisher := newSourceBundlePublisher(t, cfg, key, directory)
	target := publisher.target.(*singleRootBundleTarget)
	var hookErr error
	var final string
	target.beforeCommit = func(path string) {
		final = path
		hookErr = os.Remove(path)
		if hookErr == nil {
			hookErr = os.WriteFile(path, []byte("replacement inode"), 0o644)
		}
	}
	result, err := publisher.PublishSource(context.Background(), RoleImage, publishSource(t, flattened))
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !errors.Is(err, errLocationFinalVanished) || result.Ref != "" {
		t.Fatalf("replaced final result = %+v, err=%v", result, err)
	}
	data, err := os.ReadFile(final)
	if err != nil || string(data) != "replacement inode" {
		t.Fatalf("replacement removed or changed: %q, %v", data, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".bundle" {
		t.Fatalf("unexpected outputs: %v, %v", entries, err)
	}
}

func TestSingleRootBundleCancellationAndValidation(t *testing.T) {
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	admission, err := cfg.WriteAdmission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSingleRootBundlePublication(cfg, func() ([32]byte, error) { return key, nil }, admission); err != nil {
		t.Fatal(err)
	}
	badAdmission := store.WriteAdmission{Generation: admission.Generation}
	if err := ValidateSingleRootBundlePublication(cfg, func() ([32]byte, error) { return key, nil }, badAdmission); err == nil {
		t.Fatal("invalid admission was accepted")
	}
	if _, err := NewSingleRootBundlePublisher(cfg, func() ([32]byte, error) { return key, nil }, admission, "", t.TempDir(), nil); err == nil {
		t.Fatal("empty location name was accepted")
	}
	if _, err := NewSingleRootBundlePublisher(cfg, func() ([32]byte, error) { return key, nil }, admission, "release", "relative", nil); err == nil {
		t.Fatal("relative location directory was accepted")
	}
	directory := t.TempDir()
	publisher := newSourceBundlePublisher(t, cfg, key, directory)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := publisher.PublishSource(ctx, RoleImage, publishSource(t, publishEROFSFixture()))
	if !errors.Is(err, context.Canceled) || result.Ref != "" {
		t.Fatalf("canceled publication result = %+v, err=%v", result, err)
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("canceled publication entries = %v, err=%v", entries, readErr)
	}

	failedDirectory := t.TempDir()
	failedPublisher := newSourceBundlePublisher(t, cfg, key, failedDirectory)
	failure := errors.New("injected source failure")
	failedResult, err := failedPublisher.PublishSource(context.Background(), RoleImage, &failingBundleSource{
		Source: publishSource(t, publishEROFSFixture()), err: failure,
	})
	if !errors.Is(err, failure) || failedResult.Ref != "" {
		t.Fatalf("failed publication result = %+v, err=%v", failedResult, err)
	}
	entries, readErr = os.ReadDir(failedDirectory)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed publication entries = %v, err=%v", entries, readErr)
	}
}

func TestPublishSourceRetainsCallerSourceOwnership(t *testing.T) {
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	publisher := newSourceBundlePublisher(t, cfg, key, t.TempDir())
	source := &closeTrackingSource{Source: publishSource(t, publishEROFSFixture())}
	if _, err := publisher.PublishSource(context.Background(), RoleImage, source); err != nil {
		t.Fatal(err)
	}
	if source.closed {
		t.Fatal("PublishSource closed its caller-owned source")
	}
}

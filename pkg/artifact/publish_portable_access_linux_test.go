package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func portableParentSandboxSource(t *testing.T, disk string) sparse.Source {
	t.Helper()
	raw, _ := publishPortable(t, disk)
	source, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x51}, 8192)), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func portableParentSnapshotSource(t *testing.T, e, memory string) sparse.Source {
	t.Helper()
	raw, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: e, FromRefs: []string{memory}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x62}, 8192)), []byte("{}"), []byte("{}"), raw)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func portableParentInput(t *testing.T, bundle, memoryRoot bool, cfg *config.ManifestConfig, keyFn ingest.CustomerKeyFunc, disk, memory string) string {
	t.Helper()
	ctx, dir := context.Background(), t.TempDir()
	var sink snapshot.ArtifactSink = snapshot.NewFileSink(dir, "input", nil, false, nil)
	if bundle {
		admission, err := cfg.WriteAdmission(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sink, err = snapshot.NewPlannedBundleSink(dir, "input", cfg, keyFn, admission, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer sink.Close()
	ref, path, err := sink.AbsorbSandbox(ctx, portableParentSandboxSource(t, disk))
	if err != nil {
		t.Fatal(err)
	}
	if memoryRoot {
		ref, path, err = sink.AbsorbSnapshot(ctx, portableParentSnapshotSource(t, ref, memory))
		if err == nil {
			err = sink.CommitSnapshot(ctx, ref, path)
		}
	} else {
		err = sink.CommitSandbox(ctx, ref, path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if bundle {
		return filepath.Join(dir, strings.TrimPrefix(ref, "manifest://")+".bundle")
	}
	return path
}

func TestPublishRetainedParentsAreNotOpened(t *testing.T) {
	for _, bundle := range []bool{false, true} {
		for _, memoryRoot := range []bool{false, true} {
			for _, condition := range []string{"available", "unmapped", "offline", "wrong-key", "wrong-identity"} {
				t.Run(fmt.Sprintf("bundle=%t/snapshot=%t/%s", bundle, memoryRoot, condition), func(t *testing.T) {
					ctx, parents := context.Background(), t.TempDir()
					cfg, key := sourceBundleConfig(crypto.LocalOff)
					keyFn := func() ([32]byte, error) { return key, nil }
					storage, err := NewProcessStorageWithCustomerKey(cfg, keyFn)
					if err != nil {
						t.Fatal(err)
					}
					defer storage.Close()
					parentKey := key
					if condition == "wrong-key" {
						parentKey[0] ^= 0xff
					}
					codec, err := crypto.NewTarStreamCodec(parentKey)
					if err != nil {
						t.Fatal(err)
					}
					disk := testRefOverlay(t, parents, "parents", codec, bytes.Repeat([]byte{1}, 4096))
					history := "file://unavailable.sandbox@digest:" + publishTestSHA + "@location:historical"
					m, _ := testRefSnapshot(t, parents, history, nil, codec)
					memory := locatedTestRef(t, m, "parents")
					if condition == "wrong-identity" {
						disk.Digest, memory.Digest = strings.Repeat("0", 64), strings.Repeat("0", 64)
					}
					locations := config.RefLocations{"parents": parents}
					if condition == "unmapped" {
						locations = nil
					}
					for _, ref := range []manifest.Ref{disk, memory} {
						path := filepath.Join(parents, ref.Path)
						if condition == "offline" {
							if err := os.Remove(path); err != nil {
								t.Fatal(err)
							}
						}
						// Actual use still enforces access, authentication and identity.
						opened, err := storage.OpenFileWithLocations(ctx, path, ref, locations)
						if opened != nil {
							opened.Close()
						}
						if condition == "available" && err != nil || condition != "available" && err == nil {
							t.Fatalf("direct parent read: %v", err)
						}
						switch condition {
						case "wrong-key":
							if !errors.Is(err, tarstream.ErrAuthentication) {
								t.Fatalf("wrong-key read: %v", err)
							}
						case "wrong-identity":
							if !errors.Is(err, tarstream.ErrDigestMismatch) {
								t.Fatalf("wrong-identity read: %v", err)
							}
						}
					}
					input := portableParentInput(t, bundle, memoryRoot, cfg, keyFn, disk.String(), memory.String())
					openedParent := watchReportOpens(t, parents)
					out := t.TempDir()
					publisher, err := NewLocationPublisher(storage, "out", out, locations, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer publisher.Close()
					result, err := publisher.Publish(ctx, input)
					if err != nil {
						t.Fatalf("pure parent retention: %v", err)
					}
					if openedParent() {
						t.Fatal("publication opened a retained parent")
					}
					outputs := config.RefLocations{"out": out}
					if memoryRoot {
						doc, err := storage.Inspect(ctx, result.Ref, outputs)
						if err != nil || !slices.Equal(doc.Snapshot.FromRefs, []string{memory.String()}) {
							t.Fatalf("retained memory reference changed: %+v, %v", doc, err)
						}
						if bundle {
							logicalE, err := manifest.ParseRef(doc.Snapshot.SandboxRef)
							if err != nil || logicalE.Scheme != manifest.RefSchemeManifest {
								t.Fatalf("Bundle logical E = %q: %v", doc.Snapshot.SandboxRef, err)
							}
							carrier, err := manifest.ParseRef(result.Ref)
							if err != nil || carrier.Scheme != manifest.RefSchemeFile || carrier.Location != "out" {
								t.Fatalf("output Bundle carrier = %q: %v", result.Ref, err)
							}
							boundE, err := manifest.ParseRef(result.SandboxRef)
							if err != nil || boundE.Scheme != carrier.Scheme || boundE.Path != carrier.Path || boundE.Location != carrier.Location || boundE.DigestScheme != "manifest" || boundE.Digest != logicalE.Path {
								t.Fatalf("E binding = %q, logical E = %q, carrier = %q: %v", result.SandboxRef, doc.Snapshot.SandboxRef, result.Ref, err)
							}
						} else if doc.Snapshot.SandboxRef != result.SandboxRef {
							t.Fatalf("published E changed: result=%q snapshot=%q", result.SandboxRef, doc.Snapshot.SandboxRef)
						}
					}
					doc, err := storage.Inspect(ctx, result.SandboxRef, outputs)
					if err != nil || len(doc.Sandbox.Boot.Root.BaseFromRefs) != 1 || doc.Sandbox.Boot.Root.BaseFromRefs[0] != disk.String() {
						t.Fatalf("retained disk reference changed: %+v, %v", doc, err)
					}
					report, err := result.Report()
					if err != nil {
						t.Fatal(err)
					}
					for _, removed := range report.RemovedRefs {
						if removed == disk.String() || removed == memory.String() {
							t.Fatalf("retained parent reported removed: %s", removed)
						}
					}
				})
			}
		}
	}
}

func TestPublishSnapshotSourceRetainsUnavailableParents(t *testing.T) {
	for _, bundle := range []bool{false, true} {
		t.Run(fmt.Sprintf("bundle=%t", bundle), func(t *testing.T) {
			ctx := context.Background()
			cfg, key := sourceBundleConfig(crypto.LocalOff)
			cfg.Crypto.Ref = crypto.LocalRequired
			keyFn := func() ([32]byte, error) { return key, nil }
			storage, err := NewProcessStorageWithCustomerKey(cfg, keyFn)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			e := "file://unavailable.sandbox@hmac:" + publishTestSHA + "@location:offline-e"
			memory := "file://unavailable.snapshot@hmac:" + publishTestSHA + "@location:offline-memory"
			source := portableParentSnapshotSource(t, e, memory)
			out := t.TempDir()
			var publisher *Publisher
			if bundle {
				admission, err := cfg.WriteAdmission(ctx)
				if err != nil {
					t.Fatal(err)
				}
				publisher, err = NewSingleRootBundlePublisher(cfg, keyFn, admission, "out", out, nil)
			} else {
				publisher, err = NewLocationPublisher(storage, "out", out, nil, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer publisher.Close()
			result, err := publisher.PublishSource(ctx, RoleSnapshot, source, e)
			if err != nil || result.SandboxRef != e {
				t.Fatalf("direct source publication: %+v, %v", result, err)
			}
			assertRemoved(t, result)
			outputs := config.RefLocations{"out": out}
			doc, err := storage.Inspect(ctx, result.Ref, outputs)
			if err != nil || doc.Snapshot.SandboxRef != e || len(doc.Snapshot.FromRefs) != 1 || doc.Snapshot.FromRefs[0] != memory {
				t.Fatalf("direct source references changed: %+v, %v", doc, err)
			}
			wrong, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return [32]byte{0xff}, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer wrong.Close()
			if _, err := wrong.Inspect(ctx, result.Ref, outputs); err == nil {
				t.Fatal("published output accepted the wrong key")
			}
		})
	}
}

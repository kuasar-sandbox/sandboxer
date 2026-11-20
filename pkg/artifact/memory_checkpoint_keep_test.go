package artifact

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestCleanupCheckpointRetainsLocalMemoryLowers(t *testing.T) {
	for _, rootKind := range []string{"ordinary", "local", "current-only"} {
		for _, located := range []bool{false, true} {
			name := rootKind + "/direct"
			if located {
				name = rootKind + "/named-location"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				fixture := newMemoryCheckpointFixture(t)
				lowerRef, lowerPath, lowerDescriptor := writeLocalMemory(t, fixture.directory, "lower", fixture.eRef,
					[]string{fixture.ref}, []string{fixture.external}, "xxxxxq", 0x20, 0x20, 0, false)
				lower := fixture
				lower.ref, lower.path, lower.descriptor = lowerRef, lowerPath, lowerDescriptor
				view := openLocalFixture(t, lower)
				assertMemoryPages(t, view.Memory, "DDZHZD", "bc\x00\x00\x00q")
				if err := view.Close(); err != nil {
					t.Fatal(err)
				}
				locations := config.RefLocations{}
				if located {
					ref, err := manifest.ParseRef(lowerRef)
					if err != nil {
						t.Fatal(err)
					}
					ref.Location = "retained-memory"
					lowerRef = ref.String()
					locations[ref.Location] = fixture.directory
				}
				// The current capture has no internal parent. The previous two
				// local objects are needed only through this flat memory lower.
				suffix := []string{lowerRef, fixture.external}
				currentRef, _, _ := writeLocalMemory(t, fixture.directory, "current", fixture.eRef,
					nil, suffix, "xxxrxx", 8, 8, 0, false)
				rootRef := currentRef
				if rootKind == "ordinary" {
					sink := snapshot.NewFileSink(fixture.directory, "sid", nil, false, nil)
					top, _, err := snapshot.CaptureMemory(bytes.NewReader(checkpointPageBytes("xxxrxx")),
						&memory.CaptureView{Size: localMemorySize, Materialized: []byte{8}, Dirty: []byte{8}, Released: []byte{0}}, false)
					if err != nil {
						t.Fatal(err)
					}
					rootRef = rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: fixture.eRef, FromRefs: suffix}, top)
					if err := sink.Close(); err != nil {
						t.Fatal(err)
					}
				} else if rootKind == "current-only" {
					rootRef = "" // Current SID memory remains an independent keep root.
				}
				garbage := filepath.Join(fixture.directory, strings.Repeat("e", 64)+".image")
				if err := os.WriteFile(garbage, []byte("unreferenced"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := fixture.storage.CleanupCheckpoint(ctx, Checkpoint{Directory: fixture.directory,
					SandboxID: "sid", SnapshotRef: rootRef, SandboxRef: fixture.eRef, RefLocations: locations}); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"one", "lower"} {
					record, err := memorycheckpoint.ReadRecord(fixture.directory, id)
					if err != nil {
						t.Fatalf("retained memory lower lost capture record %s: %v", id, err)
					}
					ref, err := manifest.ParseRef(record.PayloadRef)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(filepath.Join(fixture.directory, ref.Path)); err != nil {
						t.Fatalf("retained memory lower lost payload: %v", err)
					}
				}
				view = openLocalFixture(t, lower)
				assertMemoryPages(t, view.Memory, "DDZHZD", "bc\x00\x00\x00q")
				if err := view.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(garbage); !os.IsNotExist(err) {
					t.Fatalf("unreferenced artifact was not collected: %v", err)
				}
			})
		}
	}
}

func TestCleanupCheckpointOrdinaryMemoryLowersRemainOpaque(t *testing.T) {
	for _, variant := range []string{"direct", "named-local", "named-external", "direct-inode", "named-local-inode"} {
		t.Run(variant, func(t *testing.T) {
			location := strings.TrimSuffix(variant, "-inode")
			fixture := newMemoryCheckpointFixture(t)
			directory := fixture.directory
			if location == "named-external" {
				directory = t.TempDir()
			}
			name := strings.Repeat("a", 64) + ".snapshot"
			path := filepath.Join(directory, name)
			if err := os.WriteFile(path, []byte("opaque historical memory payload"), 0600); err != nil {
				t.Fatal(err)
			}
			ref := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: name, DigestScheme: "digest", Digest: strings.Repeat("a", 64)}
			if strings.HasSuffix(variant, "-inode") {
				captured, err := manifest.ParseRef(fixture.ref)
				if err != nil {
					t.Fatal(err)
				}
				// Matching the digest alone is not evidence: this selected
				// carrier is a different inode from the capture record's file.
				ref.Digest = captured.Digest
			}
			locations := config.RefLocations{}
			if location != "direct" {
				ref.Location = "opaque-memory"
				locations[ref.Location] = directory
			}
			sink := snapshot.NewFileSink(fixture.directory, "sid", nil, false, nil)
			root := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: fixture.eRef,
				FromRefs: []string{ref.String()}}, rewriteSparse(t, checkpointPageBytes("xxxxxx")))
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fixture.storage.CleanupCheckpoint(context.Background(), Checkpoint{Directory: fixture.directory,
				SandboxID: "sid", SnapshotRef: root, SandboxRef: fixture.eRef, RefLocations: locations}); err != nil {
				t.Fatalf("cleanup opened an ordinary memory lower: %v", err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "opaque historical memory payload" {
				t.Fatalf("opaque lower changed: %q, %v", got, err)
			}
		})
	}
}

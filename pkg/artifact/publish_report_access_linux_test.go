package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"golang.org/x/sys/unix"
)

func watchReportOpens(t *testing.T, paths ...string) func() bool {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	for _, path := range paths {
		if _, err := unix.InotifyAddWatch(fd, path, unix.IN_OPEN); err != nil {
			t.Fatal(err)
		}
	}
	return func() bool {
		var b [4096]byte
		n, err := unix.Read(fd, b[:])
		if err != nil && !errors.Is(err, unix.EAGAIN) {
			t.Fatal(err)
		}
		return n > 0
	}
}

func TestReportExplicitChainAccess(t *testing.T) {
	for _, skip := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("skip=%t/missing=%t", skip, missing), func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				storage, _ := NewProcessStorage(nil)
				defer storage.Close()
				sink := snapshot.NewFileSink(dir, "chain", nil, false, nil)
				refs := make([]string, 3)
				paths := make([]string, 3)
				for i := range refs {
					var err error
					refs[i], paths[i], err = sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{byte(i + 1)}, 4096)), nil)
					if err != nil {
						t.Fatal(err)
					}
				}
				// Separate portable binding of A is the explicit merged chain target.
				target, _ := manifest.ParseRef(refs[0])
				target.Location = "replacement"
				targetDir := t.TempDir()
				content, err := os.ReadFile(paths[0])
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(targetDir, target.Path), content, 0600); err != nil {
					t.Fatal(err)
				}
				_, cfg := publishPortable(t, "")
				cfg.Boot.Disks = []config.PortableDiskConfig{{Name: "data", PortableRootConfig: config.PortableRootConfig{Base: refs[0], BaseFromRefs: refs[1:]}}}
				cfg.Mounts = []config.MountConfig{{Type: "disk", Source: "data", Target: "/data"}}
				e := rewriteSaveE(t, sink, cfg, rewriteSparse(t, bytes.Repeat([]byte{5}, 4096)))
				parsed, _ := manifest.ParseRef(e)
				root := "file://" + filepath.Join(dir, parsed.Path)
				var observed func() bool
				if missing {
					for _, path := range paths {
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					observed = watchReportOpens(t, paths...)
				}
				publisher := rewriteTarget(t, storage, t.TempDir())
				publisher.locations["replacement"] = targetDir
				result, err := publisher.PublishWithOptions(ctx, root, RewriteOptions{SkipVerify: skip, Reductions: []RefReduction{{Top: refs[0], Target: target.String()}}})
				if missing && !skip {
					if err == nil {
						t.Fatal("missing verified chain succeeded")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				assertRemoved(t, result, append([]string{root}, refs...)...)
				if observed != nil && observed() == skip {
					t.Fatalf("old chain access does not match skip=%t", skip)
				}
			})
		}
	}
}

type reportNoReadSource struct{}

func (reportNoReadSource) Size() uint64                             { return 4096 }
func (reportNoReadSource) RunAt(uint64, uint64) (sparse.Run, error) { panic("report scanned source") }
func (reportNoReadSource) ReadAt(context.Context, []byte, uint64) (int, error) {
	panic("report read payload")
}

type reportMetadataTarget struct{ calls int }

func (t *reportMetadataTarget) Put(context.Context, LogicalRole, sparse.Source) (string, error) {
	t.calls++
	return "manifest://" + publishTestSHA, nil
}
func (*reportMetadataTarget) Close() error { return nil }

func TestPublishSourceReportUsesKnownMetadata(t *testing.T) {
	target := &reportMetadataTarget{}
	p := newPublisher(nil, nil, target, nil)
	for _, role := range []LogicalRole{RoleImage, RoleSandbox, RoleSnapshot} {
		var known []string
		if role == RoleSnapshot {
			known = []string{"manifest://" + fmt.Sprintf("%064x", 42)}
		}
		result, err := p.PublishSource(context.Background(), role, reportNoReadSource{}, known...)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.RemovedRefs) != 0 || result.RemovedRefs == nil {
			t.Fatal("creation removals must be empty array")
		}
		if role == RoleSnapshot && result.SandboxRef != known[0] {
			t.Fatal("known E lost")
		}
		if role != RoleImage {
			report, err := result.Report()
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if _, err := json.Marshal(report); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if target.calls != 3 {
		t.Fatalf("Put calls=%d", target.calls)
	}
	if _, err := p.PublishSource(context.Background(), RoleSnapshot, reportNoReadSource{}); err == nil {
		t.Fatal("missing known E accepted")
	}
	if target.calls != 3 {
		t.Fatal("missing metadata wrote output")
	}
}

func TestReportOldSandboxAccessBoundary(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprint(skip), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			storage, _ := NewProcessStorage(nil)
			defer storage.Close()
			sink := snapshot.NewFileSink(dir, "e-boundary", nil, false, nil)
			body := bytes.Repeat([]byte{7}, 4096)
			leaf, leafPath, err := sink.AbsorbOverlay(ctx, bytes.NewReader(body), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, oldConfig := publishPortable(t, leaf)
			oldE := rewriteSaveE(t, sink, oldConfig, rewriteSparse(t, body))
			oldParsed, _ := manifest.ParseRef(oldE)
			_, newConfig := publishPortable(t, "")
			newE := rewriteSaveE(t, sink, newConfig, rewriteSparse(t, body))
			s := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: oldE}, rewriteSparse(t, body))
			parsed, _ := manifest.ParseRef(s)
			root := "file://" + filepath.Join(dir, parsed.Path)
			openedOld := watchReportOpens(t, filepath.Join(dir, oldParsed.Path), leafPath)
			p := rewriteTarget(t, storage, t.TempDir())
			result, err := p.PublishWithOptions(ctx, root, RewriteOptions{SkipVerify: skip, Replacements: []RefReplacement{{Old: oldE, New: newE}}})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{root, oldE}
			if !skip {
				want = append(want, leaf)
			}
			assertRemoved(t, result, want...)
			if openedOld() == skip {
				t.Fatalf("old subtree read boundary changed with skip=%t", skip)
			}
		})
	}
}

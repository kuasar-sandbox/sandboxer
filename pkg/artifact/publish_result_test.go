package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func assertRemoved(t *testing.T, result PublishResult, want ...string) {
	t.Helper()
	report, err := result.Report()
	if err != nil {
		t.Fatal(err)
	}
	expected := make([]string, 0, len(want))
	for _, raw := range want {
		ref, err := PublicRef(raw)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, ref)
	}
	sort.Strings(expected)
	if !reflect.DeepEqual(report.RemovedRefs, expected) {
		t.Fatalf("removed=%v want=%v", report.RemovedRefs, expected)
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatal(err)
	}
	count := 2
	if result.Role == RoleSnapshot {
		count = 3
	}
	if len(shape) != count || shape["sandboxRef"] == nil || shape["removedRefs"] == nil {
		t.Fatalf("shape=%s", body)
	}
}

func TestReportScopesBeforeProjection(t *testing.T) {
	p := (&Publisher{}).operation()
	for _, identity := range []string{"", "@digest:" + publishTestSHA, "@hmac:" + publishTestSHA, "@manifest:" + publishTestSHA} {
		p.original("file://same.bundle"+identity, publishScope{relativeDir: "/source/one"})
		p.retained("file://same.bundle"+identity, publishScope{relativeDir: "/source/two"})
	}
	// Alternate spellings in the same source scope compare before projection.
	p.original("file://retained.sandbox", publishScope{relativeDir: "/source/one"})
	p.retained("file:///source/one/./retained.sandbox", publishScope{})
	result := p.result(RoleSandbox, "manifest://"+publishTestSHA, "")
	assertRemoved(t, result, "file://same.bundle", "file://same.bundle@digest:"+publishTestSHA, "file://same.bundle@hmac:"+publishTestSHA, "file://same.bundle@manifest:"+publishTestSHA)
	report, _ := result.Report()
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "/source") {
		t.Fatalf("private path: %s", raw)
	}
}

func TestReportOrdinaryRepeatAndNoop(t *testing.T) {
	ctx := context.Background()
	dir, output := t.TempDir(), t.TempDir()
	storage, _ := NewProcessStorage(nil)
	defer storage.Close()
	sink := snapshot.NewFileSink(dir, "report", nil, false, nil)
	body := bytes.Repeat([]byte{0x71}, 4096)
	layer, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, cfg := publishPortable(t, layer)
	e := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
	s := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: e}, rewriteSparse(t, body))
	parsed, _ := manifest.ParseRef(s)
	root := "file://" + filepath.Join(dir, parsed.Path)
	p := rewriteTarget(t, storage, output)
	first, err := p.Publish(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, first, root, e, layer)
	info, err := storage.Inspect(ctx, first.Ref, p.locations)
	if err != nil {
		t.Fatal(err)
	}
	if first.SandboxRef != info.Snapshot.SandboxRef {
		t.Fatalf("E=%s actual=%s", first.SandboxRef, info.Snapshot.SandboxRef)
	}
	// Reporting must preserve the existing memo fast path across calls.
	parsedLayer, _ := manifest.ParseRef(layer)
	if err := os.Remove(filepath.Join(dir, parsedLayer.Path)); err != nil {
		t.Fatal(err)
	}
	again, err := p.Publish(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, again, root, e, layer)
	if again.Ref != first.Ref {
		t.Fatal("repeat changed root")
	}
	noop, err := p.Publish(ctx, first.Ref)
	if err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, noop)
	// A separate call must never inherit refs or memoized file content.
	_, freshCfg := publishPortable(t, "")
	fresh := rewriteSaveE(t, sink, freshCfg, rewriteSparse(t, body))
	parsed, _ = manifest.ParseRef(fresh)
	last, err := p.Publish(ctx, "file://"+filepath.Join(dir, parsed.Path))
	if err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, last, "file://"+parsed.Path)
}

func TestReportReduceAndSharedRetained(t *testing.T) {
	for _, mode := range []string{"replace", "explicit", "automatic", "any"} {
		for _, skip := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/skip=%t", mode, skip), func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				storage, _ := NewProcessStorage(nil)
				defer storage.Close()
				sink := snapshot.NewFileSink(dir, "report", nil, false, nil)
				body := bytes.Repeat([]byte{0x53}, 4096)
				a, _, _ := sink.AbsorbOverlay(ctx, bytes.NewReader(body), nil)
				// Same bytes at a separately named location remain a distinct reference.
				parsed, _ := manifest.ParseRef(a)
				located := parsed
				located.Location = "source"
				retained := located.String()
				_, cfg := publishPortable(t, "")
				cfg.Boot.Disks = []config.PortableDiskConfig{
					{Name: "one", PortableRootConfig: config.PortableRootConfig{Base: retained, BaseFromRefs: []string{a}}},
					{Name: "two", PortableRootConfig: config.PortableRootConfig{Base: retained}},
				}
				cfg.Mounts = []config.MountConfig{{Target: "/one", Type: "disk", Source: "one"}, {Target: "/two", Type: "disk", Source: "two"}}
				e := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
				parsedE, _ := manifest.ParseRef(e)
				root := "file://" + filepath.Join(dir, parsedE.Path)
				p := rewriteTarget(t, storage, t.TempDir())
				p.locations["source"] = dir
				opts := RewriteOptions{SkipVerify: skip}
				switch mode {
				case "replace":
					opts.Replacements = []RefReplacement{{Old: a, New: retained}}
					cfg.Boot.Disks[0].Base = a
					cfg.Boot.Disks[0].BaseFromRefs = nil
					e = rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
					parsedE, _ = manifest.ParseRef(e)
					root = "file://" + filepath.Join(dir, parsedE.Path)
				case "explicit":
					opts.Reductions = []RefReduction{{Top: retained, Target: retained}}
				case "automatic":
					opts.Reductions = []RefReduction{{Top: retained}}
				case "any":
					opts.ReduceAny = true
				}
				result, err := p.PublishWithOptions(ctx, root, opts)
				if err != nil {
					t.Fatal(err)
				}
				assertRemoved(t, result, root, a)
			})
		}
	}
}

func TestReportSkipEReplacementBoundary(t *testing.T) {
	for _, skip := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("skip=%t/missing=%t", skip, missing), func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				storage, _ := NewProcessStorage(nil)
				defer storage.Close()
				sink := snapshot.NewFileSink(dir, "report", nil, false, nil)
				body := bytes.Repeat([]byte{0x31}, 4096)
				leaf, _, _ := sink.AbsorbOverlay(ctx, bytes.NewReader(body), nil)
				_, oldCfg := publishPortable(t, leaf)
				oldE := rewriteSaveE(t, sink, oldCfg, rewriteSparse(t, body))
				oldParsed, _ := manifest.ParseRef(oldE)
				// Equivalent new E using another binding of the same payload.
				newLeaf, _ := manifest.ParseRef(leaf)
				newLeaf.Location = "known"
				_, newCfg := publishPortable(t, newLeaf.String())
				newE := rewriteSaveE(t, sink, newCfg, rewriteSparse(t, body))
				s := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: oldE}, rewriteSparse(t, body))
				parsedS, _ := manifest.ParseRef(s)
				root := "file://" + filepath.Join(dir, parsedS.Path)
				if missing {
					if err := os.Remove(filepath.Join(dir, oldParsed.Path)); err != nil {
						t.Fatal(err)
					}
				}
				p := rewriteTarget(t, storage, t.TempDir())
				p.locations["known"] = dir
				result, err := p.PublishWithOptions(ctx, root, RewriteOptions{SkipVerify: skip, Replacements: []RefReplacement{{Old: oldE, New: newE}}})
				if missing && !skip {
					if err == nil {
						t.Fatal("missing old E accepted with verification")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				want := []string{root, oldE}
				if !skip {
					want = append(want, leaf)
				}
				assertRemoved(t, result, want...)
				info, err := storage.Inspect(ctx, result.Ref, p.locations)
				if err != nil {
					t.Fatal(err)
				}
				if result.SandboxRef != info.Snapshot.SandboxRef {
					t.Fatal("report did not return final E")
				}
			})
		}
	}
}

func TestReportConcurrentOperations(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storage, _ := NewProcessStorage(nil)
	defer storage.Close()
	sink := snapshot.NewFileSink(dir, "parallel", nil, false, nil)
	body := bytes.Repeat([]byte{0x27}, 16384)
	layer, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, cfg := publishPortable(t, layer)
	e := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
	parsed, _ := manifest.ParseRef(e)
	root := "file://" + filepath.Join(dir, parsed.Path)
	p := rewriteTarget(t, storage, t.TempDir())
	type outcome struct {
		result PublishResult
		err    error
	}
	results := make(chan outcome, 4)
	start := make(chan struct{})
	for range 4 {
		go func() { <-start; result, err := p.Publish(ctx, root); results <- outcome{result, err} }()
	}
	close(start)
	var final string
	for range 4 {
		out := <-results
		if out.err != nil {
			t.Fatal(out.err)
		}
		assertRemoved(t, out.result, root, layer)
		if final != "" && out.result.Ref != final {
			t.Fatal("parallel roots differ")
		}
		final = out.result.Ref
	}
}

func TestReportSeparateBundleECarrier(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	key, err := cfg.CustomerKey()
	if err != nil {
		t.Fatal(err)
	}
	input := t.TempDir()
	ePublisher := newSourceBundlePublisher(t, cfg, key, input)
	runtime, _ := publishPortable(t, "")
	eSource, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{9}, 4096)), nil, runtime)
	if err != nil {
		t.Fatal(err)
	}
	e, err := ePublisher.PublishSource(ctx, RoleSandbox, eSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := ePublisher.Close(); err != nil {
		t.Fatal(err)
	}
	eCarrier, _ := manifest.ParseRef(e.Ref)
	eKey, _ := manifest.ParseKeyRef(eCarrier.Digest)
	eManifest := "manifest://" + manifest.HexKey(eKey)
	admission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := snapshot.NewPlannedBundleSink(input, "separate", cfg, storage.CustomerKeyFunc(), admission, []string{"file://" + eCarrier.Path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	scfg, _ := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: eManifest})
	sSource, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{8}, 4096)), []byte("{}"), []byte("{}"), scfg)
	if err != nil {
		t.Fatal(err)
	}
	sRef, _, err := sink.AbsorbSnapshot(ctx, sSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSnapshot(ctx, sRef, ""); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	sKey, _ := manifest.ParseKeyRef(sRef)
	source := "file://" + filepath.Join(input, manifest.HexKey(sKey)+".bundle")
	output := t.TempDir()
	p := rewriteTarget(t, storage, output)
	result, err := p.Publish(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	finalCarrier := eCarrier
	finalCarrier.Location = "result"
	if result.SandboxRef != finalCarrier.String() {
		t.Fatalf("E=%s want selected sibling %s", result.SandboxRef, finalCarrier.String())
	}
	assertRemoved(t, result, source, bundleMemberRef("file://"+filepath.Join(input, eCarrier.Path), eKey))
	selected, err := storage.SnapshotSandboxRef(ctx, result.Ref, p.locations)
	if err != nil || selected != result.SandboxRef {
		t.Fatalf("portable E=%s want=%s err=%v", selected, result.SandboxRef, err)
	}
	noop, err := p.Publish(ctx, result.Ref)
	if err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, noop)
	remote, err := NewManifestPublisher(storage, cfg, p.locations, nil)
	if err != nil {
		t.Fatal(err)
	}
	uploaded, err := remote.Publish(ctx, result.Ref)
	closeErr := remote.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("upload err=%v close=%v", err, closeErr)
	}
	if uploaded.SandboxRef != eManifest {
		t.Fatalf("remote E=%s", uploaded.SandboxRef)
	}
	assertRemoved(t, uploaded, result.Ref, result.SandboxRef)
}

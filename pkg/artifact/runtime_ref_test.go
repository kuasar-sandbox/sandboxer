package artifact

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

func TestAnnotatedSandboxRejectsBeforePublication(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	portable, _ := publishPortable(t, "")
	source, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x20}, 4096)), nil, portable)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewManifestPublisher(storage, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	e, err := remote.PublishSource(ctx, RoleSandbox, source)
	if err != nil {
		t.Fatal(err)
	}
	key, err := cfg.CustomerKey()
	if err != nil {
		t.Fatal(err)
	}
	bundleDir, tarDir := t.TempDir(), t.TempDir()
	bundle := newSourceBundlePublisher(t, cfg, key, bundleDir)
	bundleE, err := bundle.PublishSource(ctx, RoleSandbox, source)
	closeErr := bundle.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("prepare Bundle E: %v %v", err, closeErr)
	}
	tar, err := NewLocationPublisher(storage, "tar", tarDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tarE, err := tar.Publish(ctx, e.Ref)
	closeErr = tar.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("prepare tarstream E: %v %v", err, closeErr)
	}
	locations := config.RefLocations{"release": bundleDir, "tar": tarDir}
	for _, raw := range []string{e.Ref, bundleE.Ref, tarE.Ref} {
		annotated := raw + "@runtime:runtime.bundle:digest:" + strings.Repeat("a", 64)
		if _, err := storage.Inspect(ctx, annotated, locations); err == nil {
			t.Fatalf("inspection accepted non-Snapshot declaration: %s", annotated)
		}
		for _, rewrite := range []bool{false, true} {
			output := t.TempDir()
			target, err := NewLocationPublisher(storage, "output", output, locations, nil)
			if err != nil {
				t.Fatal(err)
			}
			if rewrite {
				_, err = target.PublishWithOptions(ctx, annotated, RewriteOptions{ReduceAny: true})
			} else {
				_, err = target.Publish(ctx, annotated)
			}
			closeErr := target.Close()
			if err == nil || closeErr != nil {
				t.Fatalf("non-Snapshot publication = %v, close = %v", err, closeErr)
			}
			files, err := os.ReadDir(output)
			if err != nil || len(files) != 0 {
				t.Fatalf("rejected publication changed destination: %v, %v", files, err)
			}
		}
	}
}

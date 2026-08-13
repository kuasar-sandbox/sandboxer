package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

type blockingSecondSnapshotFetcher struct {
	path    string
	mu      sync.Mutex
	opens   int
	started chan struct{}
	once    sync.Once
}

func (f *blockingSecondSnapshotFetcher) OpenManifest(ctx context.Context, _ store.ContentKey) (fetch.Stream, error) {
	f.mu.Lock()
	f.opens++
	openNumber := f.opens
	f.mu.Unlock()
	if openNumber == 1 {
		return fetch.OpenTarStream(f.path)
	}
	f.once.Do(func() { close(f.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRestoreDoesNotPublishLeaseWhileSnapshotOpenIsBlocked(t *testing.T) {
	dir := t.TempDir()
	snap := &SnapshotCfg{}
	snap.Resources.Capacity.CPU = 1
	snap.Resources.Capacity.Memory = "512MiB"
	snapshotPath := writePublishSnapshot(t, dir, snap)
	fetcher := &blockingSecondSnapshotFetcher{path: snapshotPath, started: make(chan struct{})}

	cgroup := filepath.Join(dir, "cgroup")
	if err := os.Mkdir(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "controller.sock")
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity = config.CapacityConfig{CPU: 1, Memory: "512MiB"}
	cfg.Resources.Allocatable = config.AllocatableConfig{CPU: 0.5, Memory: "128MiB"}
	cfg.Resources.Startup = &config.StartupConfig{Memory: "256MiB"}
	cfg.Resources.Control.Controller = socket
	cfg.Resources.Control.CgroupPath = cgroup
	cfg.ApplyDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	const sid = "blocked-restore"
	go func() {
		_, err := Run(ctx, Options{
			SnapshotManifestKey: strings.Repeat("a", 64),
			HostCfg:             cfg,
			Fetcher:             fetcher,
			SandboxID:           sid,
			RuntimeRoot:         filepath.Join(dir, "run"),
			BaseRoot:            filepath.Join(dir, "base"),
		})
		done <- err
	}()
	select {
	case <-fetcher.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("restore did not reach its second snapshot open")
	}
	if _, err := os.Stat(resource.LeasePath(socket, sid)); !errors.Is(err, os.ErrNotExist) {
		cancel()
		t.Fatalf("restore published a lease before snapshot metadata was available: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled restore error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled restore did not return")
	}
}

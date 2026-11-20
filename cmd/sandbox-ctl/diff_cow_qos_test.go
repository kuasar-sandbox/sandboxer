package main

import (
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffCOWQoSCLINormalizationPreservesZero(t *testing.T) {
	body := strings.Replace(skeletonCold, "diff_cow: { cache_size: 32MiB, max_dirty_size: 0, discard: auto }", `diff_cow: { cache_size: 32MiB, cache_idle_after: 10s, max_dirty_size: 0, discard: auto, read_bandwidth: 0, write_bandwidth: 0, read_iops: 0, write_iops: 0, max_dirty_size_per_disk: 0, read_bandwidth_per_disk: 0, write_bandwidth_per_disk: 0, read_iops_per_disk: 0, write_iops_per_disk: 0 }`, 1)
	path := filepath.Join(t.TempDir(), "sandbox.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"default", "restore"} {
		rc, out, stderr := captureInfoOutput(t, func() int { return configCmd([]string{"--config", path, "--mode", mode, "--check", "strict"}) })
		if rc != 0 || stderr != "" {
			t.Fatalf("mode=%s rc=%d: %s", mode, rc, stderr)
		}
		cfg, presence, err := config.LoadConfigBytesWithPresence([]byte(out))
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"max_dirty_size", "read_bandwidth", "write_bandwidth", "read_iops", "write_iops", "max_dirty_size_per_disk", "read_bandwidth_per_disk", "write_bandwidth_per_disk", "read_iops_per_disk", "write_iops_per_disk"} {
			if !presence.Has("resources.diff_cow." + field) {
				t.Fatalf("mode=%s lost explicit %s: %s", mode, field, out)
			}
		}
		limits, err := cfg.Resources.DiffCOW.Limits()
		if err != nil || limits != (config.DiffCOWLimits{}) {
			t.Fatalf("mode=%s zero changed: %+v %v", mode, limits, err)
		}
		if mode == "restore" && presence.Has("resources.diff_cow.discard") {
			t.Fatal("restore retained cold mount policy")
		}
	}
}

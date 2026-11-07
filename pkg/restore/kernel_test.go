package restore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// Exercise the public S -> E -> runtime binding path, stopping at an unrelated
// snapshot topology guard before run-directory or VM side effects. A kernel
// binding/open requirement would fail before that guard.
func TestRunMemoryRestoreDoesNotRequireExternalKernel(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "missing legacy path"}[supplied], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			digest := strings.Repeat("a", 64)
			portable := &config.PortableSandboxConfig{
				Version: config.PortableSandboxConfigVersion,
				Resources: config.PortableResourcesConfig{
					Capacity:    config.CapacityConfig{CPU: 1, Memory: "4KiB"},
					Allocatable: config.AllocatableConfig{CPU: 1, Memory: "4KiB"},
				},
				Boot: config.PortableBootConfig{
					Kernel:  "file://vmlinux-v1@digest:" + digest,
					Runtime: "file://runtime-v1.bundle@digest:" + digest,
					Root:    config.PortableRootConfig{Base: "self"},
				},
				Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
			}
			raw, err := config.MarshalPortableSandboxConfig(portable)
			if err != nil {
				t.Fatal(err)
			}
			ext4 := make([]byte, 4096)
			ext4[1024+0x38], ext4[1024+0x38+1] = 0x53, 0xef
			eLogical, err := sandboxfile.BuildSource(sparse.Dense(bytes.NewReader(ext4), uint64(len(ext4))), nil, raw)
			if err != nil {
				t.Fatal(err)
			}
			eRef, _, err := snapshot.NewFileSink(dir, "kernel-independent-e", nil, false, nil).AbsorbSandbox(ctx, eLogical)
			if err != nil {
				t.Fatal(err)
			}
			sConfig, err := snapshot.MarshalConfig(&snapshot.Config{
				Version: snapshot.SnapshotConfigVersion, SandboxRef: eRef,
			})
			if err != nil {
				t.Fatal(err)
			}
			sLogical, err := snapshotfile.BuildSource(
				sparse.Dense(bytes.NewReader(make([]byte, 4096)), 4096),
				[]byte(`{"cpus":{"boot_vcpus":2,"max_vcpus":2}}`), []byte("{}"), sConfig,
			)
			if err != nil {
				t.Fatal(err)
			}
			_, snapshotPath, err := snapshot.NewFileSink(dir, "kernel-independent-s", nil, false, nil).AbsorbSnapshot(ctx, sLogical)
			if err != nil {
				t.Fatal(err)
			}
			// The target supplies identical runtime content under a different
			// basename. Neither the source nor target kernel exists.
			runtimePath := filepath.Join(dir, "runtime-current.bundle")
			writeRestoreRuntime(t, runtimePath, digest)
			host := &config.SandboxConfig{Boot: config.BootConfig{Runtime: "file://" + runtimePath}}
			if supplied {
				host.Boot.Kernel = "file://" + filepath.Join(dir, "missing-target-vmlinux")
			}
			runtimeRoot := filepath.Join(dir, "run")
			_, err = Run(ctx, Options{
				SnapshotPath: snapshotPath, HostCfg: host,
				SandboxID: "kernel-independent", RuntimeRoot: runtimeRoot, BaseRoot: filepath.Join(dir, "base"),
			})
			if err == nil || !strings.Contains(err.Error(), "snapshot CPU topology mismatch") {
				t.Fatalf("restore did not reach snapshot topology validation without a kernel: %v", err)
			}
			if _, err := os.Stat(runtimeRoot); !os.IsNotExist(err) {
				t.Fatalf("topology guard created runtime state: %v", err)
			}
		})
	}
}

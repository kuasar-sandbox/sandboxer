package resctl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalCold = `
resources:
  capacity: { cpu: 2, memory: 2GiB }
  allocatable: { cpu: 2, memory: 1GiB }
network: { tap: tap0 }
boot:
  kernel: file:///opt/sandbox/vmlinux
  runtime: file:///opt/sandbox/sandbox-runtime.bundle
  cmdline: "console=hvc0"
  root:
    base: file:///container.erofs
    overlay: { diff: file:///run/sb/diff.ext4, size: 1GiB }
launch:
  exec: /usr/bin/echo
  args: ["hello", "world"]
`

func makeMinimalCfg() *config.SandboxConfig {
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 2, Memory: "4GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1.5, Memory: "2GiB"},
		},
		Network: config.NetworkConfig{TAP: "tap0"},
		Boot: config.BootConfig{
			Kernel:  "file:///vmlinux",
			Runtime: "file:///sandbox-runtime.bundle",
			Cmdline: "console=hvc0",
			Root: config.RootConfig{
				Base:    "file:///c.erofs",
				Overlay: &config.OverlayConfig{Diff: "file:///d.ext4", DiffSize: "1GiB"},
			},
		},
		Launch: config.LaunchConfig{Exec: "/usr/bin/echo", Args: []string{"hello", "world"}, Workdir: "/", Restart: "never"},
	}
	cfg.ApplyDefaults()
	return cfg
}

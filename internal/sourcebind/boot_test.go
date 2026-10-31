package sourcebind

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestApplyDefaultBootBindings(t *testing.T) {
	dir := t.TempDir()
	portable := &config.PortableSandboxConfig{Boot: config.PortableBootConfig{
		Kernel:  "file://vmlinux@digest:" + strings.Repeat("a", 64),
		Runtime: "file://runtime.bundle@digest:" + strings.Repeat("b", 64),
	}}
	host := &config.SandboxConfig{}
	ApplyDefaultBootBindings(host, portable, dir)
	if host.Boot.Kernel != "file://"+filepath.Join(dir, "vmlinux") || host.Boot.Runtime != "file://"+filepath.Join(dir, "runtime.bundle") {
		t.Fatalf("default host boot = %+v", host.Boot)
	}
	host.Boot.Kernel = "file:///explicit/kernel"
	ApplyDefaultBootBindings(host, portable, dir)
	if host.Boot.Kernel != "file:///explicit/kernel" {
		t.Fatal("explicit host binding was overwritten")
	}
	host = &config.SandboxConfig{}
	ApplyDefaultBootBindings(host, portable, "")
	if host.Boot.Kernel != "" || host.Boot.Runtime != "" {
		t.Fatal("manifest-only source acquired a local boot binding")
	}
}

// Package sourcebind applies host bindings shared by cold and restored sources.
package sourcebind

import (
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// ApplyDefaultBootBindings fills absent host kernel/runtime paths from the
// source directory. Explicit host bindings remain authoritative; portable
// content identities are retained separately in the portable configuration.
func ApplyDefaultBootBindings(host *config.SandboxConfig, portable *config.PortableSandboxConfig, relativeDir string) {
	if host == nil || portable == nil || relativeDir == "" {
		return
	}
	bind := func(current *string, raw string) {
		if *current != "" {
			return
		}
		ref, err := manifest.ParseRef(raw)
		if err != nil || ref.Scheme != manifest.RefSchemeFile {
			return
		}
		*current = "file://" + filepath.Join(relativeDir, ref.Path)
	}
	bind(&host.Boot.Kernel, portable.Boot.Kernel)
	bind(&host.Boot.Runtime, portable.Boot.Runtime)
}

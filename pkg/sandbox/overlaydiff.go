package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

// DefaultBaseRoot is the on-disk root for per-sandbox persistent state (the
// overlay diff). Distinct from the tmpfs run-dir (sockets / snap staging).
// Overridable via --base-root / SANDBOX_BASE_ROOT.
const DefaultBaseRoot = "/var/lib/sandbox"

// DefaultBaseDir returns the per-sandbox base directory under baseRoot. PathID
// is the host directory leaf and is independent from logical SandboxID.
func DefaultBaseDir(baseRoot, pathID string) string {
	return filepath.Join(baseRoot, pathID)
}

// DefaultDiffURI returns the auto-default overlay diff URI (on disk, under the
// already-derived baseDir). The filename retains logical SandboxID identity.
// Used when boot.root.overlay.diff is empty.
func DefaultDiffURI(baseDir, sandboxID string) string {
	return "file://" + filepath.Join(baseDir, sandboxID+".overlay.diff")
}

// DefaultDiskDiffURI returns an auto-default data-disk diff URI below the
// PathID-derived baseDir while retaining SandboxID in the filename.
func DefaultDiskDiffURI(baseDir, sandboxID, diskKey string) string {
	return "file://" + filepath.Join(baseDir, fmt.Sprintf("%s.%s.diff", sandboxID, diskKey))
}

// PrepareDiff inspects only enough state to describe how OpenBlockCOW should
// open or initialize the active diff. It never copies, truncates, or creates a
// target. For a fresh (absent/empty) diff it enforces the cold-boot ext4-source
// rule:
//
//   - existing non-empty diff → kept as-is (size = its on-disk size)
//   - existing empty diff     → rejected (it cannot be atomically provisioned)
//   - else diff_template set  → record its logical source path (its size wins)
//   - else base present       → blank diff sized to the base (vdb = base ext4)
//   - else                    → error (a blank diff is not a mountable ext4)
//
// baseSize is 0 when there is no overlay.base; templateURI is "" when unset.
func PrepareDiff(diffPath, templateURI string, baseSize, _ int64) (vhost.DiffInit, error) {
	if st, err := os.Stat(diffPath); err == nil {
		if st.Size() > 0 {
			return vhost.DiffInit{Existing: true}, nil
		}
		return vhost.DiffInit{}, fmt.Errorf("existing diff %s is empty; remove it or provide a formatted diff", diffPath)
	} else if !os.IsNotExist(err) {
		return vhost.DiffInit{}, fmt.Errorf("stat diff %s: %w", diffPath, err)
	}
	switch {
	case templateURI != "":
		scheme, templatePath, ok := config.SchemeAndPath(templateURI)
		if !ok || scheme != "file" {
			return vhost.DiffInit{}, fmt.Errorf("diff_template invalid URI: %s", templateURI)
		}
		return vhost.DiffInit{TemplatePath: templatePath}, nil
	case baseSize > 0:
		return vhost.DiffInit{CreateSize: baseSize}, nil
	default:
		return vhost.DiffInit{}, fmt.Errorf("no ext4 source for a fresh overlay diff: set boot.root.overlay.diff_template or boot.root.overlay.base, or pre-format the diff at %s", diffPath)
	}
}

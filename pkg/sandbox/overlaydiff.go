package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
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

// DefaultDiffURI returns the auto-default overlay diff URI below the logical
// SandboxID directory. It preserves the original baseRoot/SandboxID contract
// for callers that do not use a distinct PathID.
func DefaultDiffURI(baseRoot, sandboxID string) string {
	return DefaultDiffURIForBaseDir(DefaultBaseDir(baseRoot, sandboxID), sandboxID)
}

// DefaultDiffURIForBaseDir returns the auto-default overlay diff URI below an
// already-derived, PathID-specific baseDir. The filename retains logical
// SandboxID identity. Used when boot.root.overlay.diff is empty.
func DefaultDiffURIForBaseDir(baseDir, sandboxID string) string {
	return "file://" + filepath.Join(baseDir, sandboxID+".overlay.diff")
}

// DefaultDiskDiffURI returns an auto-default data-disk diff URI below the
// PathID-derived baseDir while retaining SandboxID in the filename.
func DefaultDiskDiffURI(baseDir, sandboxID, diskKey string) string {
	return "file://" + filepath.Join(baseDir, fmt.Sprintf("%s.%s.diff", sandboxID, diskKey))
}

// PrepareDiff inspects only enough state to describe how OpenBlockCOW should
// open or initialize the active diff. It never copies, truncates, or creates a
// target. Capacity comes from the selected source, never a separate limit:
//
//   - existing non-empty diff → kept as-is (its logical size is read on open)
//   - existing empty diff     → rejected (it cannot be atomically provisioned)
//   - else diff_template set  → record its logical source path (its size wins)
//   - else base present       → blank diff sized to the base (vdb = base ext4)
//   - else                    → error (a blank diff is not a mountable ext4)
//
// baseSize is the COW base's logical size, or 0 without a base;
// templateURI is "" when unset. No filesystem resize or quota is performed.
func PrepareDiff(diffPath, templateURI string, baseSize int64) (diffstore.DiffInit, error) {
	exists, err := existingDiff(diffPath)
	if err != nil {
		return diffstore.DiffInit{}, err
	}
	if exists {
		return diffstore.DiffInit{Existing: true}, nil
	}

	switch {
	case templateURI != "":
		scheme, templatePath, ok := config.SchemeAndPath(templateURI)
		if !ok || scheme != "file" {
			return diffstore.DiffInit{}, fmt.Errorf("diff_template invalid URI: %s", templateURI)
		}
		return diffstore.DiffInit{TemplatePath: templatePath}, nil
	case baseSize > 0:
		if err := diffstore.ValidateWritableSize(baseSize); err != nil {
			return diffstore.DiffInit{}, err
		}
		return diffstore.DiffInit{CreateSize: baseSize}, nil
	default:
		return diffstore.DiffInit{}, fmt.Errorf("no ext4 source for a fresh overlay diff: set boot.root.overlay.diff_template or boot.root.overlay.base, or pre-format the diff at %s", diffPath)
	}
}

func existingDiff(path string) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat diff %s: %w", path, err)
	}
	if info.Size() == 0 {
		return false, fmt.Errorf("existing diff %s is empty; remove it or provide a formatted diff", path)
	}
	return true, nil
}

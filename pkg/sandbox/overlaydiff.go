package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
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

// PrepareDiff preserves the local raw-template planning API. The baseSize is
// used only for a fresh, template-free COW layer. Existing targets take priority
// over both arguments, and a template is opened only by OpenDisk after its claim.
//
// Deprecated: Use PrepareDiffWithStorage for identified templates, generation,
// an explicit read context, and caller-owned artifact access.
func PrepareDiff(diffPath, templateURI string, baseSize int64) (diffstore.DiffInit, error) {
	exists, err := existingDiff(diffPath)
	if err != nil {
		return diffstore.DiffInit{}, err
	}
	if exists {
		return diffstore.DiffInit{Existing: true}, nil
	}
	if templateURI != "" {
		ref, err := config.ParseDiffTemplate(templateURI)
		if err != nil {
			return diffstore.DiffInit{}, err
		}
		if ref.Kind != config.DiffTemplateRaw || ref.Ref.Location != "" {
			return diffstore.DiffInit{}, errors.New("PrepareDiff requires a raw absolute file template; use PrepareDiffWithStorage for other sources")
		}
		return PrepareDiffWithStorage(context.Background(), diffPath, ref.String(), nil, nil, nil)
	}
	if baseSize > 0 {
		if err := diffstore.ValidateWritableSize(baseSize); err != nil {
			return diffstore.DiffInit{}, err
		}
		return diffstore.DiffInit{CreateSize: baseSize}, nil
	}
	return PrepareDiffWithStorage(context.Background(), diffPath, "", nil, nil, nil)
}

// PrepareDiffWithStorage plans an existing writer, a lazy template initializer, or a blank
// COW layer over base. It does not open a template or create a target. OpenDisk
// calls the factory only after acquiring the fresh-target creation claim.
//
// The caller keeps base and storage alive for the disk's lifetime. ctx must
// cover retained source reads, rather than only a short preflight operation.
func PrepareDiffWithStorage(ctx context.Context, diffPath, templateURI string, base diffstore.BlockReader, storage *artifact.ProcessStorage, locations config.RefLocations) (diffstore.DiffInit, error) {
	exists, err := existingDiff(diffPath)
	if err != nil {
		return diffstore.DiffInit{}, err
	}
	if exists {
		return diffstore.DiffInit{Existing: true}, nil
	}

	switch {
	case templateURI != "":
		ref, err := config.ParseDiffTemplate(templateURI)
		if err != nil {
			return diffstore.DiffInit{}, fmt.Errorf("diff_template: %w", err)
		}
		return diffstore.DiffInit{Template: func() (diffstore.TemplateSource, error) {
			if ref.Kind == config.DiffTemplateMkfs && base != nil {
				return nil, errors.New("mkfs template requires a writable device without a COW base")
			}
			return storage.OpenDiffTemplate(ctx, ref.String(), "", locations)
		}}, nil
	case base != nil && base.Size() > 0:
		if err := diffstore.ValidateWritableSize(base.Size()); err != nil {
			return diffstore.DiffInit{}, err
		}
		return diffstore.DiffInit{CreateSize: base.Size()}, nil
	default:
		return diffstore.DiffInit{}, fmt.Errorf("no ext4 source for a fresh overlay diff: set boot.root.overlay.diff_template or boot.root.overlay.base, or pre-format the diff at %s", diffPath)
	}
}

// TemplateCapacity reads the selected template's logical geometry and releases
// the probe. It never creates a DIFF or treats carrier byte length as capacity.
func TemplateCapacity(ctx context.Context, ref string, storage *artifact.ProcessStorage, locations config.RefLocations) (int64, error) {
	source, err := storage.OpenDiffTemplate(ctx, ref, "", locations)
	if err != nil {
		return 0, err
	}
	size := int64(source.Size())
	err = errors.Join(diffstore.ValidateWritableSize(size), source.Close())
	if err != nil {
		return 0, err
	}
	return size, nil
}

// ValidateDiffTemplate probes the effective ext4 view without publishing or
// fully materializing it. Full carrier verification remains a Ready barrier.
func ValidateDiffTemplate(ctx context.Context, ref string, base diffstore.BlockReader, storage *artifact.ProcessStorage, locations config.RefLocations) error {
	source, err := openValidatedDiffTemplate(ctx, ref, base, storage, locations)
	if err != nil {
		return err
	}
	return source.Close()
}

func openValidatedDiffTemplate(ctx context.Context, raw string, base diffstore.BlockReader, storage *artifact.ProcessStorage, locations config.RefLocations) (diffstore.TemplateSource, error) {
	ref, err := config.ParseDiffTemplate(raw)
	if err != nil {
		return nil, err
	}
	if ref.Kind == config.DiffTemplateMkfs && base != nil {
		return nil, errors.New("mkfs template requires a writable device without a COW base")
	}
	source, err := storage.OpenDiffTemplate(ctx, ref.String(), "", locations)
	if err != nil {
		return nil, err
	}
	if err = diffstore.ValidateDiffTemplateExt4(ctx, source, base); err != nil {
		return nil, errors.Join(err, source.Close())
	}
	return source, nil
}

func existingDiff(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat diff %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("existing diff %s: %w", path, err)
		}
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("existing diff %s is not a regular file", path)
	}
	if info.Size() == 0 {
		return false, fmt.Errorf("existing diff %s is empty; remove it or provide a formatted diff", path)
	}
	return true, nil
}

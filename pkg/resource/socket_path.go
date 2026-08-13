package resource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CanonicalSocketPath returns the stable filesystem identity used for a
// controller socket and its adjacent owner/lease inventory. Symlinks in the
// parent path are resolved even when one or more trailing directories have not
// been created yet. The socket entry itself must not be a symlink: following a
// final-component alias would let the server bind and inventory a different
// pathname depending on whether the target happened to exist at startup.
func CanonicalSocketPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("controller socket path is empty")
	}
	if strings.HasPrefix(path, "@") || strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("controller socket must be a filesystem Unix socket path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("controller socket %s is a symlink", abs)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect controller socket %s: %w", abs, err)
	}

	parent, err := canonicalExistingParent(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("resolve controller socket parent: %w", err)
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func canonicalExistingParent(path string) (string, error) {
	current := path
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

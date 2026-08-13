package resource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	parent, err := canonicalExistingParent(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("resolve controller socket parent: %w", err)
	}
	canonical := filepath.Join(parent, filepath.Base(abs))
	if info, err := os.Lstat(canonical); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("controller socket %s is a symlink", canonical)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return "", fmt.Errorf("inspect controller socket %s link count", canonical)
		}
		if stat.Nlink != 1 {
			return "", fmt.Errorf("controller socket %s has %d hard links", canonical, stat.Nlink)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect controller socket %s: %w", canonical, err)
	}
	return canonical, nil
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
		if info, lstatErr := os.Lstat(current); lstatErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("parent component %s is a dangling symlink", current)
			}
			return "", err
		} else if !errors.Is(lstatErr, os.ErrNotExist) {
			return "", lstatErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

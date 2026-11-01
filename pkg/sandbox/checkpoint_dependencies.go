package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
)

// Named captures stay local: collecting an immutable dependency must not change
// its content identity after the active DIFF has been bound to that base graph.
// A Bundle checkpoint may consequently reference neighboring tarstream files.
func preserveNamedCheckpointDependency(ctx context.Context, dep artifactDependency, opts RunOptions, known map[string]string, output string) (string, error) {
	ref, err := manifest.ParseRef(dep.raw)
	if err != nil {
		return "", err
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		physical, found, err := bundleManifestMergeRefWithLookup(ctx, dep.raw, opts, snapshotDependencyBundleLookup(dep, opts))
		if err != nil {
			return "", err
		}
		if !found {
			return dep.raw, nil
		}
		ref, err = manifest.ParseRef(physical)
		if err != nil {
			return "", err
		}
	}
	if ref.Location != "" {
		return ref.String(), nil
	}
	path, err := resolveBundleArtifactPath(ref, dep.raw, known, output, opts.RefLocations, bindingRelativeDirs(opts))
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	output, err = filepath.EvalSymlinks(output)
	if err != nil {
		return "", err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return "", err
	}
	ext := filepath.Ext(path)
	if ref.DigestScheme == "manifest" {
		ext = ".bundle"
	}
	if ext != ".sandbox" && ext != ".overlay" && ext != ".image" && ext != ".bundle" {
		if dep.role == dependencyRootImage {
			ext = ".image"
		} else {
			ext = ".overlay"
		}
	}
	name := ref.Digest + ext
	if ref.Digest == "" {
		return "", errors.New("checkpoint dependency has no immutable identity")
	}
	destination := filepath.Join(output, name)
	if path != destination {
		if err := copyCheckpointCarrier(ctx, path, destination, opts.SandboxID); err != nil {
			return "", err
		}
	}
	ref.Path = name
	return ref.String(), nil
}

func copyCheckpointCarrier(ctx context.Context, source, destination, sandboxID string) (retErr error) {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, src.Close()) }()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("checkpoint dependency is not a regular file")
	}
	if target, err := os.Lstat(destination); err == nil && !target.Mode().IsRegular() {
		return errors.New("checkpoint dependency destination is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if existing, err := os.Open(destination); err == nil {
		defer func() { retErr = errors.Join(retErr, existing.Close()) }()
		target, err := existing.Stat()
		if err != nil {
			return err
		}
		if os.SameFile(info, target) {
			return nil
		}
		if !target.Mode().IsRegular() || info.Size() != target.Size() {
			return errors.New("checkpoint dependency destination conflicts")
		}
		a, b := make([]byte, 64<<10), make([]byte, 64<<10)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, e := src.Read(a)
			if n > 0 {
				m, rerr := io.ReadFull(existing, b[:n])
				if rerr != nil || m != n || !bytes.Equal(a[:n], b[:n]) {
					return errors.New("checkpoint dependency destination content differs")
				}
			}
			if e == io.EOF {
				return nil
			}
			if e != nil {
				return e
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Immutable carriers on the same filesystem can share bytes with the
	// parent's retained dependency. Cross-filesystem inputs use the established
	// checked copy below; the active writable DIFF is never copied here.
	if err := os.Link(source, destination); err == nil {
		return nil
	}
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	role := filepath.Ext(destination)
	tmp, err := os.CreateTemp(filepath.Dir(destination), sandboxID+role+".*.partial")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, tmp.Close()); _ = os.Remove(tmp.Name()) }()
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, e := src.Read(buffer)
		if n > 0 {
			if _, err := tmp.Write(buffer[:n]); err != nil {
				return err
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	// link publishes without replacing any existing content-addressed carrier.
	if err := os.Link(tmp.Name(), destination); err != nil {
		return fmt.Errorf("commit checkpoint dependency: %w", err)
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

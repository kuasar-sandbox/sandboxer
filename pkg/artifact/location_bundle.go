package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

type locationBundleFile struct {
	source      string
	destination string
	root        store.ContentKey
}

func (t *locationPublishTarget) PutBundle(ctx context.Context, plan bundlePublishPlan) (string, error) {
	if plan.opened == nil || plan.opened.BundleReader() == nil {
		return "", errors.New("publish Bundle to location: source reader is unavailable")
	}
	if plan.keyFn == nil || plan.decryptor == nil || plan.exactRoot.Reader == nil {
		return "", errors.New("publish Bundle to location: exact verification is unavailable")
	}
	customerKey, err := plan.keyFn()
	if err != nil {
		return "", err
	}
	defer clear(customerKey[:])
	if err := manifestbundle.VerifyExactManifests(
		ctx, plan.exactRoot, plan.exactDependencies, customerKey, plan.decryptor, manifestbundle.VerifyOptions{},
	); err != nil {
		return "", fmt.Errorf("publish Bundle exact verification: %w", err)
	}
	rootName := manifest.HexKey(plan.root) + ".bundle"
	if filepath.Base(plan.path) != rootName {
		return "", fmt.Errorf("publish Bundle to location: source name %q does not match root Manifest", filepath.Base(plan.path))
	}

	files := make([]locationBundleFile, 0, len(plan.opened.BundleReader().Refs())+1)
	seen := make(map[string]struct{})
	for _, raw := range plan.opened.BundleReader().Refs() {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return "", err
		}
		if ref.Location != "" {
			continue
		}
		if ref.Scheme != manifest.RefSchemeFile || ref.Digest != "" {
			return "", fmt.Errorf("publish Bundle to location: invalid exact source ref %q", raw)
		}
		if ref.Path == rootName {
			return "", fmt.Errorf("publish Bundle to location: refs contain the current Bundle %q", raw)
		}
		root, err := bundleKeyFromName(ref.Path)
		if err != nil {
			return "", fmt.Errorf("publish Bundle to location: ref %q: %w", raw, err)
		}
		if _, duplicate := seen[ref.Path]; duplicate {
			continue
		}
		seen[ref.Path] = struct{}{}
		files = append(files, locationBundleFile{
			source:      filepath.Join(filepath.Dir(plan.path), ref.Path),
			destination: filepath.Join(t.directory, ref.Path),
			root:        root,
		})
	}
	files = append(files, locationBundleFile{
		source: plan.path, destination: filepath.Join(t.directory, rootName), root: plan.root,
	})

	// Validate every source before exposing any new target file. Dependencies
	// are then committed before the root, so a failed operation never returns a
	// root ref whose same-directory Bundle sources were not processed first.
	for _, file := range files {
		if err := t.validateBundleFile(file.source, file.root); err != nil {
			return "", fmt.Errorf("publish Bundle source %s: %w", filepath.Base(file.source), err)
		}
	}
	for _, file := range files {
		if err := t.putBundleFile(ctx, file); err != nil {
			return "", fmt.Errorf("publish Bundle file %s: %w", filepath.Base(file.destination), err)
		}
	}
	if err := t.verifyLocatedBundlePlan(ctx, plan, files, customerKey); err != nil {
		return "", fmt.Errorf("verify published Bundle graph: %w", err)
	}

	ref := manifest.Ref{
		Scheme: manifest.RefSchemeFile, Path: rootName,
		DigestScheme: "manifest", Digest: manifest.HexKey(plan.root), Location: t.location,
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	t.logf("publish: exact Bundle %s -> %s (dependencies=%d)", plan.role, ref.String(), len(files)-1)
	return ref.String(), nil
}

type openedLocationBundle struct {
	path   string
	file   locationReadFile
	info   os.FileInfo
	reader *manifestbundle.Reader
}

func (t *locationPublishTarget) verifyLocatedBundlePlan(
	ctx context.Context,
	plan bundlePublishPlan,
	files []locationBundleFile,
	customerKey [32]byte,
) (retErr error) {
	opened := make([]openedLocationBundle, 0, len(files))
	defer func() {
		for index := len(opened) - 1; index >= 0; index-- {
			retErr = errors.Join(retErr, opened[index].reader.Close(), opened[index].file.Close())
		}
	}()
	for _, item := range files {
		file, err := t.fs.openNoFollow(item.destination)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: mode %s", errLocationFinalNonRegular, info.Mode())
		}
		reader, err := manifestbundle.NewReader(file, info.Size())
		if err != nil {
			_ = file.Close()
			return classifyLocationContentError(err)
		}
		opened = append(opened, openedLocationBundle{path: item.destination, file: file, info: info, reader: reader})
	}
	if len(opened) == 0 {
		return errors.New("published Bundle graph is empty")
	}
	// files is dependencies-first/root-last for publication. Bundle source
	// selection is current-root first, then refs order, so verification uses the
	// inverse view without changing the commit order.
	rootReader := opened[len(opened)-1].reader
	exactRoot := manifestbundle.ExactManifest{Key: plan.root, Reader: rootReader}
	exactDependencies := make([]manifestbundle.ExactManifest, 0, len(plan.exactDependencies))
	for _, selected := range plan.exactDependencies {
		var reader *manifestbundle.Reader
		if rootReader.HasManifest(selected.Key) {
			reader = rootReader
		} else {
			for index := 0; index < len(opened)-1; index++ {
				if opened[index].reader.HasManifest(selected.Key) {
					reader = opened[index].reader
					break
				}
			}
		}
		if reader == nil {
			return fmt.Errorf("published Bundle graph omits Manifest %s", manifest.HexKey(selected.Key))
		}
		exactDependencies = append(exactDependencies, manifestbundle.ExactManifest{Key: selected.Key, Reader: reader})
	}
	if err := manifestbundle.VerifyExactManifests(
		ctx, exactRoot, exactDependencies, customerKey, plan.decryptor, manifestbundle.VerifyOptions{},
	); err != nil {
		return err
	}
	for _, item := range opened {
		current, err := t.fs.lstat(item.path)
		if err != nil {
			return err
		}
		if !os.SameFile(item.info, current) {
			return fmt.Errorf("%w: Bundle path changed during verification", errLocationFinalVanished)
		}
	}
	return nil
}

func bundleKeyFromName(name string) (store.ContentKey, error) {
	if filepath.Base(name) != name || filepath.Ext(name) != ".bundle" {
		return store.ContentKey{}, errors.New("Bundle source must be a <64hex>.bundle basename")
	}
	return manifest.ParseHexKey(name[:len(name)-len(".bundle")])
}

func (t *locationPublishTarget) validateBundleFile(path string, root store.ContentKey) (retErr error) {
	file, err := t.fs.openNoFollow(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: mode %s", errLocationFinalNonRegular, info.Mode())
	}
	reader, err := manifestbundle.NewReader(file, info.Size())
	if err != nil {
		return classifyLocationContentError(err)
	}
	defer reader.Close()
	if !reader.HasManifest(root) {
		return fmt.Errorf("%w: root Manifest %s is absent", errLocationFinalMismatch, manifest.HexKey(root))
	}
	return nil
}

func (t *locationPublishTarget) putBundleFile(ctx context.Context, file locationBundleFile) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		created, err := t.fs.createExclusive(file.destination)
		if err != nil {
			if !os.IsExist(err) {
				return fmt.Errorf("create final exclusively: %w", err)
			}
			err = t.reuseExistingBundle(ctx, file)
			if errors.Is(err, errLocationFinalVanished) {
				continue
			}
			return err
		}
		return t.publishFreshBundle(ctx, created, file)
	}
}

func (t *locationPublishTarget) publishFreshBundle(ctx context.Context, created locationWriteFile, item locationBundleFile) error {
	return t.commitFreshLocationFile(
		created,
		item.destination,
		func(destination locationWriteFile) error {
			source, err := t.fs.openNoFollow(item.source)
			if err != nil {
				return fmt.Errorf("open source: %w", err)
			}
			sourceInfo, err := source.Stat()
			if err == nil && !sourceInfo.Mode().IsRegular() {
				err = fmt.Errorf("source is not a regular file")
			}
			if err == nil {
				err = source.Sync()
			}
			var copied int64
			if err == nil {
				copied, err = copyLocationFile(ctx, destination, source)
			}
			if err == nil && copied != sourceInfo.Size() {
				err = io.ErrUnexpectedEOF
			}
			if err := errors.Join(err, source.Close()); err != nil {
				return fmt.Errorf("copy source: %w", err)
			}
			return nil
		},
		func(destination locationReadFile, info os.FileInfo) error {
			if err := t.validateOpenedBundleCopy(ctx, item, destination, info, false); err != nil {
				return fmt.Errorf("validate copied final: %w", err)
			}
			return nil
		},
	)
}

func (t *locationPublishTarget) reuseExistingBundle(ctx context.Context, file locationBundleFile) error {
	started := time.Now()
	delay := max(t.retry.initial, time.Millisecond)
	maximum := max(t.retry.maximum, delay)
	window := max(t.retry.window, delay)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := t.validateBundleCopy(ctx, file, true)
		if err == nil {
			if err := t.fs.syncDirectory(t.directory); err != nil {
				return fmt.Errorf("sync parent directory after reuse: %w", err)
			}
			return nil
		}
		if os.IsNotExist(err) || errors.Is(err, errLocationFinalVanished) {
			return errors.Join(errLocationFinalVanished, err)
		}
		if errors.Is(err, errLocationFinalNonRegular) || !isRetryableLocationMismatch(err) {
			return fmt.Errorf("validate existing content-addressed final: %w", err)
		}
		lastErr = err
		remaining := window - time.Since(started)
		if remaining <= 0 {
			return stableInvalidLocationFinal(lastErr)
		}
		timer := time.NewTimer(min(delay, remaining))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, maximum)
	}
}

func (t *locationPublishTarget) validateBundleCopy(ctx context.Context, file locationBundleFile, syncFile bool) (retErr error) {
	destination, err := t.fs.openNoFollow(file.destination)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, destination.Close()) }()
	destinationInfo, err := destination.Stat()
	if err != nil {
		return err
	}
	if err := t.validateOpenedBundleCopy(ctx, file, destination, destinationInfo, syncFile); err != nil {
		return err
	}
	current, err := t.fs.lstat(file.destination)
	if err != nil {
		return err
	}
	if !os.SameFile(destinationInfo, current) {
		return fmt.Errorf("%w: path changed during validation", errLocationFinalVanished)
	}
	return nil
}

func (t *locationPublishTarget) validateOpenedBundleCopy(ctx context.Context, file locationBundleFile, destination locationReadFile, destinationInfo os.FileInfo, syncFile bool) (retErr error) {
	source, err := t.fs.openNoFollow(file.source)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	sourceInfo, err := source.Stat()
	if err != nil {
		return err
	}
	if !sourceInfo.Mode().IsRegular() || !destinationInfo.Mode().IsRegular() {
		return fmt.Errorf("%w: source or destination is not regular", errLocationFinalNonRegular)
	}
	if sourceInfo.Size() != destinationInfo.Size() {
		return fmt.Errorf("%w: size differs from source Bundle", errLocationFinalMismatch)
	}
	reader, err := manifestbundle.NewReader(destination, destinationInfo.Size())
	if err != nil {
		return classifyLocationContentError(err)
	}
	if !reader.HasManifest(file.root) {
		_ = reader.Close()
		return fmt.Errorf("%w: root Manifest %s is absent", errLocationFinalMismatch, manifest.HexKey(file.root))
	}
	if err := reader.Close(); err != nil {
		return classifyLocationContentError(err)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := compareLocationFiles(ctx, source, destination); err != nil {
		return classifyLocationContentError(err)
	}
	if syncFile {
		if err := destination.Sync(); err != nil {
			return fmt.Errorf("sync existing final: %w", err)
		}
	}
	return nil
}

func copyLocationFile(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			if err := writeLocationFull(destination, buffer[:n]); err != nil {
				return total, err
			}
			total += int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
}

func writeLocationFull(destination io.Writer, body []byte) error {
	for len(body) > 0 {
		n, err := destination.Write(body)
		if n < 0 || n > len(body) {
			return fmt.Errorf("invalid write count %d", n)
		}
		body = body[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func compareLocationFiles(ctx context.Context, left, right io.Reader) error {
	leftBuffer := make([]byte, 128*1024)
	rightBuffer := make([]byte, len(leftBuffer))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		leftN, leftErr := io.ReadFull(left, leftBuffer)
		rightN, rightErr := io.ReadFull(right, rightBuffer)
		if leftN != rightN || !bytes.Equal(leftBuffer[:leftN], rightBuffer[:rightN]) {
			return fmt.Errorf("%w: bytes differ from source Bundle", errLocationFinalMismatch)
		}
		leftDone := errors.Is(leftErr, io.EOF) || errors.Is(leftErr, io.ErrUnexpectedEOF)
		rightDone := errors.Is(rightErr, io.EOF) || errors.Is(rightErr, io.ErrUnexpectedEOF)
		if leftDone || rightDone {
			if leftDone && rightDone {
				return nil
			}
			return fmt.Errorf("%w: size differs from source Bundle", errLocationFinalMismatch)
		}
		if leftErr != nil || rightErr != nil {
			return errors.Join(leftErr, rightErr)
		}
	}
}

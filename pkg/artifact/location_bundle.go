package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

type locationBundleFile struct {
	source      string
	destination string
	root        store.ContentKey
	verifyRoot  bool

	pinned       locationReadFile
	pinnedInfo   os.FileInfo
	pinnedReader *manifestbundle.Reader
}

func (t *locationPublishTarget) PutBundle(ctx context.Context, plan bundlePublishPlan) (refString string, retErr error) {
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
	files, unavailable, pinnedPlan, err := t.pinBundlePlan(plan)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := closePinnedBundleFiles(files); closeErr != nil {
			refString = ""
			retErr = errors.Join(retErr, fmt.Errorf("close pinned Bundle sources: %w", closeErr))
		}
	}()
	if err := manifestbundle.VerifyExactManifests(
		ctx, pinnedPlan.exactRoot, pinnedPlan.exactDependencies, customerKey, plan.decryptor, manifestbundle.VerifyOptions{},
	); err != nil {
		return "", fmt.Errorf("publish Bundle exact verification: %w", err)
	}
	if err := t.verifyUnavailableBundleTargets(unavailable); err != nil {
		return "", err
	}
	rootName := manifest.HexKey(plan.root) + ".bundle"

	// Dependencies are committed before the root, so a failed operation never
	// returns a root ref whose available same-directory Bundle sources were not
	// processed first. Missing refs retain normal Bundle fallback semantics.
	for index, file := range files {
		if index == len(files)-1 {
			// Recheck immediately before publishing the root: a concurrently
			// materialized earlier fallback must not become reachable through it.
			if err := t.verifyUnavailableBundleTargets(unavailable); err != nil {
				return "", err
			}
		}
		if err := t.putBundleFile(ctx, file); err != nil {
			return "", fmt.Errorf("publish Bundle file %s: %w", filepath.Base(file.destination), err)
		}
	}
	if err := t.verifyLocatedBundlePlan(ctx, pinnedPlan, files, customerKey); err != nil {
		return "", fmt.Errorf("verify published Bundle graph: %w", err)
	}
	if err := t.verifyUnavailableBundleTargets(unavailable); err != nil {
		return "", err
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

func (t *locationPublishTarget) pinBundlePlan(plan bundlePublishPlan) ([]locationBundleFile, []string, bundlePublishPlan, error) {
	rootName := manifest.HexKey(plan.root) + ".bundle"
	expectedRefs := plan.opened.BundleReader().Refs()
	files := make([]locationBundleFile, 0, len(expectedRefs)+1)
	unavailable := make([]string, 0)
	readers := make(map[string]*manifestbundle.Reader, len(expectedRefs))
	seen := make(map[string]struct{}, len(expectedRefs))
	fail := func(err error) ([]locationBundleFile, []string, bundlePublishPlan, error) {
		return nil, nil, bundlePublishPlan{}, errors.Join(err, closePinnedBundleFiles(files))
	}

	for _, raw := range expectedRefs {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return fail(err)
		}
		if ref.Location != "" {
			continue
		}
		if ref.Scheme != manifest.RefSchemeFile || ref.Digest != "" {
			return fail(fmt.Errorf("publish Bundle to location: invalid exact source ref %q", raw))
		}
		if ref.Path == rootName {
			return fail(fmt.Errorf("publish Bundle to location: refs contain the current Bundle %q", raw))
		}
		if _, duplicate := seen[ref.Path]; duplicate {
			continue
		}
		seen[ref.Path] = struct{}{}
		file, err := pinLocationBundleFile(
			filepath.Join(filepath.Dir(plan.path), ref.Path),
			filepath.Join(t.directory, ref.Path),
			store.ContentKey{},
			false,
		)
		if os.IsNotExist(err) {
			// An unavailable Bundle source is a clean fallback miss. Do not turn
			// a valid later-ref or remote selection into a publication failure.
			unavailable = append(unavailable, filepath.Join(t.directory, ref.Path))
			continue
		}
		if err != nil {
			return fail(fmt.Errorf("publish Bundle source %s: %w", ref.Path, err))
		}
		files = append(files, file)
		readers[raw] = file.pinnedReader
	}

	rootFile, err := pinLocationBundleFile(plan.path, filepath.Join(t.directory, rootName), plan.root, true)
	if err != nil {
		return fail(fmt.Errorf("publish Bundle root source: %w", err))
	}
	files = append(files, rootFile)
	if !slices.Equal(rootFile.pinnedReader.Refs(), expectedRefs) {
		return fail(errors.New("publish Bundle: root source changed while constructing the publication plan"))
	}

	pinnedPlan := plan
	pinnedPlan.exactRoot.Reader = rootFile.pinnedReader
	pinnedPlan.exactDependencies = append([]manifestbundle.ExactManifest(nil), plan.exactDependencies...)
	for index := range pinnedPlan.exactDependencies {
		selected := &pinnedPlan.exactDependencies[index]
		raw, ok := plan.selectedSources[selected.Key]
		if !ok {
			return fail(fmt.Errorf("publish Bundle: selected source for Manifest %s is unavailable", manifest.HexKey(selected.Key)))
		}
		if _, located := plan.locatedExact[selected.Key]; located {
			continue
		}
		if raw == "" {
			selected.Reader = rootFile.pinnedReader
			continue
		}
		reader := readers[raw]
		if reader == nil {
			return fail(fmt.Errorf(
				"publish Bundle: selected source %q for Manifest %s became unavailable",
				raw, manifest.HexKey(selected.Key),
			))
		}
		selected.Reader = reader
	}
	return files, unavailable, pinnedPlan, nil
}

func (t *locationPublishTarget) verifyUnavailableBundleTargets(paths []string) error {
	for _, path := range paths {
		_, err := t.fs.lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check unavailable Bundle target %s: %w", filepath.Base(path), err)
		}
		return fmt.Errorf(
			"publish Bundle: unavailable source %s is present in the target and would change fallback selection; cleanup is required",
			filepath.Base(path),
		)
	}
	return nil
}

func pinLocationBundleFile(source, destination string, root store.ContentKey, verifyRoot bool) (locationBundleFile, error) {
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return locationBundleFile{}, err
	}
	file, err := (osLocationFileSystem{}).openNoFollow(resolved)
	if err != nil {
		return locationBundleFile{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return locationBundleFile{}, errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() {
		return locationBundleFile{}, errors.Join(
			fmt.Errorf("%w: source mode %s", errLocationFinalNonRegular, info.Mode()),
			file.Close(),
		)
	}
	reader, err := manifestbundle.NewReader(file, info.Size())
	if err != nil {
		return locationBundleFile{}, errors.Join(classifyLocationContentError(err), file.Close())
	}
	if verifyRoot && !reader.HasManifest(root) {
		return locationBundleFile{}, errors.Join(
			fmt.Errorf("%w: root Manifest %s is absent", errLocationFinalMismatch, manifest.HexKey(root)),
			reader.Close(), file.Close(),
		)
	}
	return locationBundleFile{
		source: source, destination: destination, root: root, verifyRoot: verifyRoot,
		pinned: file, pinnedInfo: info, pinnedReader: reader,
	}, nil
}

func closePinnedBundleFiles(files []locationBundleFile) error {
	var closeErr error
	for index := len(files) - 1; index >= 0; index-- {
		if files[index].pinnedReader != nil {
			closeErr = errors.Join(closeErr, files[index].pinnedReader.Close())
		}
		if files[index].pinned != nil {
			closeErr = errors.Join(closeErr, files[index].pinned.Close())
		}
	}
	return closeErr
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
		if _, located := plan.locatedExact[selected.Key]; located {
			// Preserve the source selected by current -> refs order. A later
			// same-directory Bundle may contain the same Manifest, but it is
			// shadowed by this earlier named-location source and must not change
			// post-copy verification semantics.
			reader = selected.Reader
		} else if rootReader.HasManifest(selected.Key) {
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
			source, sourceInfo, closeSource, err := t.openBundleSource(item)
			if err != nil {
				return fmt.Errorf("open source: %w", err)
			}
			err = source.Sync()
			var copied int64
			if err == nil {
				copied, err = copyLocationFile(ctx, destination, source)
			}
			if err == nil && copied != sourceInfo.Size() {
				err = io.ErrUnexpectedEOF
			}
			if err := errors.Join(err, closeSource()); err != nil {
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
	source, sourceInfo, closeSource, err := t.openBundleSource(file)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, closeSource()) }()
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
	if file.verifyRoot && !reader.HasManifest(file.root) {
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

func (t *locationPublishTarget) openBundleSource(file locationBundleFile) (locationReadFile, os.FileInfo, func() error, error) {
	if file.pinned != nil {
		if _, err := file.pinned.Seek(0, io.SeekStart); err != nil {
			return nil, nil, nil, err
		}
		return file.pinned, file.pinnedInfo, func() error { return nil }, nil
	}
	source, err := t.fs.openNoFollow(file.source)
	if err != nil {
		return nil, nil, nil, err
	}
	info, err := source.Stat()
	if err != nil {
		return nil, nil, nil, errors.Join(err, source.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, nil, nil, errors.Join(
			fmt.Errorf("source is not a regular file"),
			source.Close(),
		)
	}
	return source, info, source.Close, nil
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

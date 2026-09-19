package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// ValidateSingleRootBundlePublication verifies the Manifest configuration,
// customer-key resolver, and fixed write admission without reading a logical
// source or creating a Bundle. Callers can run it before image scanning or VM
// work, then pass the same admission to NewSingleRootBundlePublisher.
func ValidateSingleRootBundlePublication(cfg *config.ManifestConfig, keyFn ingest.CustomerKeyFunc, admission store.WriteAdmission) error {
	if cfg == nil {
		return errors.New("single-root Bundle: manifest configuration is required")
	}
	if keyFn == nil {
		return errors.New("single-root Bundle: customer key resolver is required")
	}
	key, err := keyFn()
	if err != nil {
		return fmt.Errorf("single-root Bundle customer key: %w", err)
	}
	clear(key[:])
	writer, err := manifestbundle.NewWriter(io.Discard, admission, manifestbundle.WriterOptions{})
	if err != nil {
		return fmt.Errorf("single-root Bundle admission: %w", err)
	}
	if _, err := cfg.NewIngesterWithWriter(keyFn, nil, writer); err != nil {
		return fmt.Errorf("single-root Bundle manifest configuration: %w", err)
	}
	return nil
}

// NewSingleRootBundlePublisher creates a direct image/Sandbox source
// publisher for one named location. Each Publisher accepts exactly one root,
// emits only <manifest-key>.bundle, and never creates an image/sandbox
// tarstream or a semantic alias. admission must have been acquired by the
// caller, normally with cfg.WriteAdmission before expensive build work. The
// source must support repeated reads: an identity pass precedes final-path
// streaming, with no intermediate file or retained payload copy.
func NewSingleRootBundlePublisher(
	cfg *config.ManifestConfig,
	keyFn ingest.CustomerKeyFunc,
	admission store.WriteAdmission,
	location string,
	directory string,
	logf func(string, ...any),
) (*Publisher, error) {
	if err := ValidateSingleRootBundlePublication(cfg, keyFn, admission); err != nil {
		return nil, err
	}
	if location == "" {
		return nil, errors.New("single-root Bundle location name is required")
	}
	probe := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: "probe.bundle", Location: location}
	if err := probe.Validate(); err != nil {
		return nil, fmt.Errorf("single-root Bundle location: %w", err)
	}
	if !filepath.IsAbs(directory) {
		return nil, fmt.Errorf("single-root Bundle directory must be absolute: %q", directory)
	}
	if err := ensurePublishDirectory(directory); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	target := &singleRootBundleTarget{
		cfg: cfg, keyFn: keyFn, admission: admission,
		location: location, directory: filepath.Clean(directory), logf: logf,
		retry: defaultLocationRetryPolicy, fs: osLocationFileSystem{},
	}
	return &Publisher{target: target, logf: logf}, nil
}

type singleRootBundleTarget struct {
	cfg       *config.ManifestConfig
	keyFn     ingest.CustomerKeyFunc
	admission store.WriteAdmission
	location  string
	directory string
	logf      func(string, ...any)
	retry     locationRetryPolicy
	fs        locationFileSystem
	// beforeCommit is a deterministic pathname-race hook used only by tests.
	beforeCommit func(string)

	mu   sync.Mutex
	used bool
}

func (t *singleRootBundleTarget) Put(ctx context.Context, role LogicalRole, source sparse.Source) (refString string, retErr error) {
	t.mu.Lock()
	if t.used {
		t.mu.Unlock()
		return "", errors.New("single-root Bundle publisher already consumed its root")
	}
	t.used = true
	t.mu.Unlock()

	switch role {
	case RoleImage, RoleSandbox, RoleSnapshot:
	default:
		return "", fmt.Errorf("single-root Bundle: unsupported root role %q", role)
	}
	if source == nil {
		return "", fmt.Errorf("single-root Bundle %s source is nil", role)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Resolve the key once for both deterministic encoding passes and validation.
	key, err := t.keyFn()
	if err != nil {
		return "", fmt.Errorf("single-root Bundle customer key: %w", err)
	}
	defer clear(key[:])
	keyFn := func() ([32]byte, error) { return key, nil }
	cfg := *t.cfg
	_, decryptor, err := manifestcrypto.New(cfg.Crypto)
	if err != nil {
		return "", err
	}
	encode := func(dst io.Writer) (*ingest.Result, error) {
		writer, err := manifestbundle.NewWriter(dst, t.admission, manifestbundle.WriterOptions{})
		if err != nil {
			return nil, err
		}
		ingester, err := cfg.NewIngesterWithWriter(keyFn, nil, writer)
		if err != nil {
			return nil, err
		}
		result, err := ingester.Ingest(ctx, source, ingest.IngestOption{})
		if err != nil {
			return nil, err
		}
		if err := writer.Finalize(result.ManifestKey); err != nil {
			return nil, err
		}
		return result, nil
	}
	// Discard physical bytes as they are encoded; only bounded active chunks
	// and the Bundle's format metadata survive this identity pass.
	physicalIdentity := sha256.New()
	planned, err := encode(physicalIdentity)
	if err != nil {
		return "", fmt.Errorf("single-root Bundle identify %s: %w", role, err)
	}
	expectedPhysical := physicalIdentity.Sum(nil)
	root := planned.ManifestKey
	basename := manifest.HexKey(root) + ".bundle"
	destination := filepath.Join(t.directory, basename)
	commit := newLocationPublishTarget(t.location, t.directory, nil, false, t.logf)
	commit.retry = t.retry
	if t.fs != nil {
		commit.fs = t.fs
	}
	validate := func(file locationReadFile, info os.FileInfo) (retErr error) {
		if !info.Mode().IsRegular() {
			return errLocationFinalNonRegular
		}
		reader, err := manifestbundle.NewReader(file, info.Size())
		if err != nil {
			return classifyLocationContentError(err)
		}
		defer func() { retErr = errors.Join(retErr, reader.Close()) }()
		if len(reader.Refs()) != 0 {
			return fmt.Errorf("%w: single-root Bundle contains external refs", errLocationFinalMismatch)
		}
		if reader.Admission() != t.admission {
			return fmt.Errorf("%w: recorded Bundle admission differs", errLocationFinalMismatch)
		}
		if err := reader.FullVerify(ctx, root, key, decryptor, manifestbundle.VerifyOptions{
			ExpectedManifests: []store.ContentKey{root},
		}); err != nil {
			return classifyLocationContentError(err)
		}
		actual := sha256.New()
		if _, err := copyLocationFile(ctx, actual, io.NewSectionReader(file, 0, info.Size())); err != nil {
			return err
		}
		if !bytes.Equal(actual.Sum(nil), expectedPhysical) {
			return fmt.Errorf("%w: physical Bundle bytes differ", errLocationFinalMismatch)
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		created, err := commit.fs.createExclusive(destination)
		if os.IsExist(err) {
			err = t.reuseFinal(ctx, commit, destination, validate)
			if errors.Is(err, errLocationFinalVanished) {
				continue
			}
			if err != nil {
				return "", err
			}
			break
		}
		if err != nil {
			return "", fmt.Errorf("single-root Bundle create final: %w", err)
		}
		err = commit.commitFreshLocationFile(created, destination, func(dst locationWriteFile) error {
			written, err := encode(dst)
			if err != nil {
				return fmt.Errorf("single-root Bundle write final: %w", err)
			}
			if written.ManifestKey != root {
				return errors.New("single-root Bundle source changed between identity and publication")
			}
			if t.beforeCommit != nil {
				t.beforeCommit(destination)
			}
			return ctx.Err()
		}, validate)
		if err != nil {
			return "", fmt.Errorf("single-root Bundle commit %s: %w", role, err)
		}
		break
	}
	ref := manifest.Ref{
		Scheme: manifest.RefSchemeFile, Path: basename,
		DigestScheme: "manifest", Digest: manifest.HexKey(root), Location: t.location,
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	t.logf("publish: %s -> %s (Bundle stored=%d dedup=%d)", role, ref.String(), planned.StoredChunks, planned.DedupChunks)
	return ref.String(), nil
}

// reuseFinal waits for a concurrent direct writer to finish. Every read pins
// the opened inode; ownership is checked again after validating its content.
func (t *singleRootBundleTarget) reuseFinal(
	ctx context.Context, target *locationPublishTarget, path string,
	validate func(locationReadFile, os.FileInfo) error,
) error {
	check := func() (retErr error) {
		file, err := target.fs.openNoFollow(path)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, file.Close()) }()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if err := validate(file, info); err != nil {
			return err
		}
		current, err := target.fs.lstat(path)
		if err != nil {
			return err
		}
		if !os.SameFile(info, current) {
			return errLocationFinalVanished
		}
		return nil
	}
	started := time.Now()
	delay := max(t.retry.initial, time.Millisecond)
	maximum := max(t.retry.maximum, delay)
	window := max(t.retry.window, delay)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := check()
		if err == nil {
			return nil
		}
		if os.IsNotExist(err) || errors.Is(err, errLocationFinalVanished) {
			return errors.Join(errLocationFinalVanished, err)
		}
		if errors.Is(err, errLocationFinalNonRegular) || !isRetryableLocationMismatch(err) {
			return fmt.Errorf("validate existing content-addressed final: %w", err)
		}
		remaining := window - time.Since(started)
		if remaining <= 0 {
			return stableInvalidLocationFinal(err)
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

func (*singleRootBundleTarget) Close() error { return nil }

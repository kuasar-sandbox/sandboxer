package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

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
// caller, normally with cfg.WriteAdmission before expensive build work.
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
		retry: defaultLocationRetryPolicy,
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
	case RoleImage, RoleSandbox:
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

	staged, err := os.CreateTemp(t.directory, ".manifest-bundle-*.partial")
	if err != nil {
		return "", fmt.Errorf("single-root Bundle temporary file: %w", err)
	}
	stagedPath := staged.Name()
	stagedInfo, err := staged.Stat()
	if err != nil {
		return "", errors.Join(
			fmt.Errorf("single-root Bundle stat temporary file: %w", err),
			staged.Close(), os.Remove(stagedPath),
		)
	}
	stagedOpen := true
	removeStaged := true
	defer func() {
		if stagedOpen {
			retErr = errors.Join(retErr, staged.Close())
		}
		if removeStaged {
			if err := removeOwnedSingleRootStaging(stagedPath, stagedInfo); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
		if retErr != nil {
			refString = ""
		}
	}()
	if err := staged.Chmod(0o644); err != nil {
		return "", fmt.Errorf("single-root Bundle temporary permissions: %w", err)
	}
	writer, err := manifestbundle.NewWriter(staged, t.admission, manifestbundle.WriterOptions{})
	if err != nil {
		return "", err
	}
	ingester, err := t.cfg.NewIngesterWithWriter(t.keyFn, nil, writer)
	if err != nil {
		return "", err
	}
	result, err := ingester.Ingest(ctx, source, ingest.IngestOption{})
	if err != nil {
		return "", fmt.Errorf("single-root Bundle ingest %s: %w", role, err)
	}
	root := result.ManifestKey
	if err := writer.Finalize(root); err != nil {
		return "", fmt.Errorf("single-root Bundle finalize %s: %w", role, err)
	}
	if err := staged.Close(); err != nil {
		return "", fmt.Errorf("single-root Bundle close staged file: %w", err)
	}
	stagedOpen = false

	basename := manifest.HexKey(root) + ".bundle"
	destination := filepath.Join(t.directory, basename)
	pinned, err := pinLocationBundleFile(stagedPath, destination, root, true)
	if err != nil {
		return "", fmt.Errorf("single-root Bundle pin staged file: %w", err)
	}
	if !os.SameFile(stagedInfo, pinned.pinnedInfo) {
		return "", errors.Join(
			fmt.Errorf("single-root Bundle staged path changed before verification: %w", errLocationFinalVanished),
			closePinnedBundleFiles([]locationBundleFile{pinned}),
		)
	}
	pinnedOpen := true
	defer func() {
		if pinnedOpen {
			retErr = errors.Join(retErr, closePinnedBundleFiles([]locationBundleFile{pinned}))
		}
	}()
	if err := t.verifyStaged(ctx, pinned.pinnedReader, root); err != nil {
		return "", fmt.Errorf("single-root Bundle verify staged file: %w", err)
	}
	if t.beforeCommit != nil {
		t.beforeCommit(stagedPath)
	}
	commit := newLocationPublishTarget(t.location, t.directory, nil, false, t.logf)
	commit.retry = t.retry
	if err := commit.putBundleFile(ctx, pinned); err != nil {
		return "", fmt.Errorf("single-root Bundle commit %s: %w", role, err)
	}
	if err := closePinnedBundleFiles([]locationBundleFile{pinned}); err != nil {
		return "", fmt.Errorf("single-root Bundle close pinned staged file: %w", err)
	}
	pinnedOpen = false
	if err := removeOwnedSingleRootStaging(stagedPath, stagedInfo); err != nil {
		return "", err
	}
	removeStaged = false

	ref := manifest.Ref{
		Scheme: manifest.RefSchemeFile, Path: basename,
		DigestScheme: "manifest", Digest: manifest.HexKey(root), Location: t.location,
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	t.logf("publish: %s -> %s (Bundle stored=%d dedup=%d)", role, ref.String(), result.StoredChunks, result.DedupChunks)
	return ref.String(), nil
}

func (t *singleRootBundleTarget) verifyStaged(ctx context.Context, reader *manifestbundle.Reader, root store.ContentKey) error {
	if reader == nil {
		return errors.New("staged Bundle reader is required")
	}
	if reader.Admission() != t.admission {
		return errors.New("recorded admission differs")
	}
	key, err := t.keyFn()
	if err != nil {
		return err
	}
	defer clear(key[:])
	_, decryptor, err := manifestcrypto.New(t.cfg.Crypto)
	if err != nil {
		return err
	}
	return reader.FullVerify(ctx, root, key, decryptor, manifestbundle.VerifyOptions{
		ExpectedManifests: []store.ContentKey{root},
	})
}

func removeOwnedSingleRootStaging(path string, owned os.FileInfo) error {
	current, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat single-root Bundle temporary file: %w", err)
	}
	if owned == nil || !os.SameFile(owned, current) {
		return fmt.Errorf("single-root Bundle temporary path changed; refusing cleanup: %w", errLocationFinalVanished)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove single-root Bundle temporary file: %w", err)
	}
	return nil
}

func (*singleRootBundleTarget) Close() error { return nil }

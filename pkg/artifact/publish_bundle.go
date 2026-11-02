package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

var errBundleCheckpoint = errors.New("bundle contains local disk Checkpoint")

func (p *Publisher) publishBundle(
	ctx context.Context,
	rootRef string,
	scope, rootScope publishScope,
	first *OpenedFile,
) (result PublishResult, retErr error) {
	root, ok := first.RootManifestKey()
	if !ok {
		_ = first.Close()
		return PublishResult{}, errors.New("publish Bundle: carrier has no root Manifest")
	}
	role, dependencies, sandboxRef, err := p.inspectBundleRoot(ctx, rootRef, scope, rootScope, first, root)
	if errors.Is(err, errBundleCheckpoint) {
		return p.publishCheckpointBundle(ctx, rootRef, scope)
	}
	if err != nil {
		return PublishResult{}, err
	}

	target, ok := p.target.(bundlePublishTarget)
	if !ok {
		return PublishResult{}, errors.New("publish Bundle: target has no exact Bundle path")
	}
	stream, _, err := p.open(ctx, rootRef, scope)
	if err != nil {
		return PublishResult{}, err
	}
	opened, ok := stream.(*OpenedFile)
	if !ok || opened.Format() != FileFormatManifestBundle {
		_ = stream.Close()
		return PublishResult{}, errors.New("publish Bundle: carrier format changed during publication")
	}
	defer func() {
		retErr = errors.Join(retErr, opened.Close())
		if retErr != nil {
			result = PublishResult{}
		}
	}()
	currentRoot, ok := opened.RootManifestKey()
	if !ok || currentRoot != root {
		return PublishResult{}, errors.New("publish Bundle: root Manifest changed during publication")
	}
	exactRoot, exactDependencies, selectedSources, locatedExact, remoteDependencies, err := selectExactBundleSources(ctx, opened, root, dependencies)
	if err != nil {
		return PublishResult{}, err
	}
	if p.storage.cfg == nil || p.storage.CustomerKeyFunc() == nil {
		return PublishResult{}, errors.New("publish Bundle: manifest configuration and customer key are required")
	}
	_, decryptor, err := manifestcrypto.New(p.storage.cfg.Crypto)
	if err != nil {
		return PublishResult{}, err
	}
	parsed, err := manifest.ParseRef(rootRef)
	if err != nil {
		return PublishResult{}, err
	}
	sourcePath, err := p.locations.ResolveFile(parsed, scope.relativeDir)
	if err != nil {
		return PublishResult{}, err
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return PublishResult{}, err
	}
	sourcePath, err = filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return PublishResult{}, fmt.Errorf("publish Bundle: resolve source: %w", err)
	}
	ref, err := target.PutBundle(ctx, bundlePublishPlan{
		path: sourcePath, role: role, root: root, opened: opened,
		exactRoot: exactRoot, exactDependencies: exactDependencies,
		selectedSources: selectedSources, locatedExact: locatedExact,
		remoteDependencies: remoteDependencies,
		keyFn:              p.storage.CustomerKeyFunc(), decryptor: decryptor,
	})
	if err != nil {
		return PublishResult{}, err
	}
	// Exact publication preserves logical keys, but changes their source binding.
	// Selection metadata is already required by the transfer; reporting does not
	// re-open any member or consult an unselected fallback.
	for _, key := range dependencies {
		raw := "manifest://" + manifest.HexKey(key)
		before, after := raw, raw
		if selected, bundled := selectedSources[key]; bundled {
			carrier := selectedBundleCarrier(rootRef, selected)
			before = bundleMemberRef(carrier, key)
			if _, remote := p.target.(*manifestPublishTarget); !remote {
				finalCarrier := selectedBundleCarrier(ref, selected)
				after = bundleMemberRef(finalCarrier, key)
			}
		}
		p.original(before, publishScope{relativeDir: filepath.Dir(sourcePath)})
		p.retained(after, publishScope{})
		if sandboxRef == raw {
			sandboxRef = after
		}
	}
	return p.result(role, ref, sandboxRef), nil
}

func selectExactBundleSources(
	ctx context.Context,
	opened *OpenedFile,
	root store.ContentKey,
	dependencies []store.ContentKey,
) (manifestbundle.ExactManifest, []manifestbundle.ExactManifest, map[store.ContentKey]string, map[store.ContentKey]struct{}, int, error) {
	if opened == nil || opened.ManifestFetcher() == nil {
		return manifestbundle.ExactManifest{}, nil, nil, nil, 0, errors.New("publish Bundle: source selector is unavailable")
	}
	rootSource, err := opened.ManifestFetcher().SelectRoot(root)
	if err != nil {
		return manifestbundle.ExactManifest{}, nil, nil, nil, 0, err
	}
	if rootSource.Reader == nil {
		return manifestbundle.ExactManifest{}, nil, nil, nil, 0, errors.New("publish Bundle: root is not backed by the current Bundle")
	}
	exactRoot := manifestbundle.ExactManifest{Key: root, Reader: rootSource.Reader}
	exactDependencies := make([]manifestbundle.ExactManifest, 0, len(dependencies))
	selectedSources := make(map[store.ContentKey]string, len(dependencies))
	locatedExact := make(map[store.ContentKey]struct{})
	remoteDependencies := 0
	for _, key := range dependencies {
		var source manifestbundle.ManifestSource
		err := readretry.Do(ctx, func() error {
			var selectErr error
			source, selectErr = opened.ManifestFetcher().SelectManifest(ctx, key)
			return selectErr
		})
		if err != nil {
			return manifestbundle.ExactManifest{}, nil, nil, nil, 0, fmt.Errorf("publish Bundle dependency %s: %w", manifest.HexKey(key), err)
		}
		if source.Reader != nil {
			exactDependencies = append(exactDependencies, manifestbundle.ExactManifest{Key: key, Reader: source.Reader})
			selectedSources[key] = source.Ref
			if source.Ref != "" {
				ref, err := manifest.ParseRef(source.Ref)
				if err != nil {
					return manifestbundle.ExactManifest{}, nil, nil, nil, 0, fmt.Errorf("publish Bundle dependency %s source %q: %w", manifest.HexKey(key), source.Ref, err)
				}
				if ref.Location != "" {
					locatedExact[key] = struct{}{}
				}
			}
			continue
		}
		stream, err := readretry.Open(ctx, func() (fetch.Stream, error) { return source.OpenManifest(ctx, key) })
		if err != nil {
			return manifestbundle.ExactManifest{}, nil, nil, nil, 0, fmt.Errorf("publish Bundle remote dependency %s: %w", manifest.HexKey(key), err)
		}
		consumeErr := consumeLocationSource(ctx, stream)
		closeErr := stream.Close()
		if err := errors.Join(consumeErr, closeErr); err != nil {
			return manifestbundle.ExactManifest{}, nil, nil, nil, 0, fmt.Errorf("verify Bundle remote dependency %s: %w", manifest.HexKey(key), err)
		}
		remoteDependencies++
	}
	return exactRoot, exactDependencies, selectedSources, locatedExact, remoteDependencies, nil
}

func (p *Publisher) inspectBundleRoot(
	ctx context.Context,
	rootRef string,
	scope, rootScope publishScope,
	first *OpenedFile,
	rootKey store.ContentKey,
) (role LogicalRole, dependencies []store.ContentKey, sandboxRef string, retErr error) {
	if sandboxRoot, err := sandboxfile.Open(ctx, first); err == nil {
		defer func() { retErr = errors.Join(retErr, sandboxRoot.Close()) }()
		if sandboxRoot.Checkpoint != nil {
			return "", nil, "", errBundleCheckpoint
		}
		if err := p.retainLocatedDisks(ctx, sandboxRoot.Portable, rootScope); err != nil {
			return "", nil, "", err
		}
		dependencies, err := manifestDependenciesFromSandbox(sandboxRoot.Portable, rootKey)
		return RoleSandbox, dependencies, "", err
	} else if readretry.IsTerminal(err) || readerr.IsPermanent(err) {
		return "", nil, "", err
	}

	stream, childScope, err := p.open(ctx, rootRef, scope)
	if err != nil {
		return "", nil, "", err
	}
	snapshotRoot, err := snapshotfile.Open(ctx, stream)
	if err != nil {
		return "", nil, "", fmt.Errorf("publish Bundle: root is neither a strict Sandbox nor Snapshot: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, snapshotRoot.Close()) }()
	cfg, err := snapshot.ParseConfig(snapshotRoot.SnapshotConfig)
	if err != nil {
		return "", nil, "", fmt.Errorf("publish Bundle: snapshot.cfg: %w", err)
	}
	canonical, err := snapshot.MarshalConfig(cfg)
	if err != nil || !bytes.Equal(canonical, snapshotRoot.SnapshotConfig) {
		if err != nil {
			return "", nil, "", err
		}
		return "", nil, "", errors.New("publish Bundle: snapshot.cfg is not canonically encoded")
	}

	for _, raw := range cfg.FromRefs {
		if err := p.retainLocated(ctx, raw, childScope); err != nil {
			return "", nil, "", err
		}
	}
	seen := map[store.ContentKey]struct{}{rootKey: {}}
	dependencies = make([]store.ContentKey, 0, len(cfg.FromRefs)+1)
	for index, raw := range cfg.FromRefs {
		if err := appendManifestDependency(raw, fmt.Sprintf("snapshot from_refs[%d]", index), seen, &dependencies); err != nil {
			return "", nil, "", err
		}
	}
	// Inspect every selected E before the exact-copy shortcut, including a
	// located file outside this Bundle. A Checkpoint must always materialize.
	sandboxStream, sandboxScope, err := p.open(ctx, cfg.SandboxRef, childScope)
	if err != nil {
		return "", nil, "", err
	}
	sandboxRoot, err := sandboxfile.Open(ctx, sandboxStream)
	if err != nil {
		return "", nil, "", err
	}
	defer func() { retErr = errors.Join(retErr, sandboxRoot.Close()) }()
	if sandboxRoot.Checkpoint != nil {
		return "", nil, "", errBundleCheckpoint
	}
	if err := p.retainLocated(ctx, cfg.SandboxRef, childScope); err != nil {
		return "", nil, "", err
	}
	if err := p.retainLocatedDisks(ctx, sandboxRoot.Portable, sandboxScope); err != nil {
		return "", nil, "", err
	}
	if err := appendManifestDependency(cfg.SandboxRef, "snapshot sandbox_ref", seen, &dependencies); err != nil {
		return "", nil, "", err
	}
	parsedE, err := manifest.ParseRef(cfg.SandboxRef)
	if err != nil {
		return "", nil, "", err
	}
	if parsedE.Scheme == manifest.RefSchemeManifest {
		for _, dependency := range portableDiskRefs(sandboxRoot.Portable) {
			if err := appendManifestDependency(dependency.raw, "Sandbox disk dependency", seen, &dependencies); err != nil {
				return "", nil, "", err
			}
		}
	}
	return RoleSnapshot, dependencies, cfg.SandboxRef, nil
}

func manifestDependenciesFromSandbox(cfg *config.PortableSandboxConfig, root store.ContentKey) ([]store.ContentKey, error) {
	seen := map[store.ContentKey]struct{}{root: {}}
	dependencies := make([]store.ContentKey, 0)
	for _, dependency := range portableDiskRefs(cfg) {
		if err := appendManifestDependency(dependency.raw, "Sandbox disk dependency", seen, &dependencies); err != nil {
			return nil, err
		}
	}
	return dependencies, nil
}

func appendManifestDependency(raw, label string, seen map[store.ContentKey]struct{}, dependencies *[]store.ContentKey) error {
	if raw == "" || raw == "self" {
		return nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return fmt.Errorf("publish Bundle: %s %q: %w", label, raw, err)
	}
	if ref.Scheme == manifest.RefSchemeFile {
		if ref.Location == "" {
			return fmt.Errorf("publish Bundle: %s %q is node-local; exact publication requires a portable graph", label, raw)
		}
		return nil
	}
	key, err := manifest.ParseKeyRef(ref.Path)
	if err != nil {
		return fmt.Errorf("publish Bundle: %s %q: %w", label, raw, err)
	}
	if _, exists := seen[key]; exists {
		return nil
	}
	seen[key] = struct{}{}
	*dependencies = append(*dependencies, key)
	return nil
}

func bundleMemberRef(carrier string, key store.ContentKey) string {
	ref, _ := manifest.ParseRef(carrier)
	ref.DigestScheme, ref.Digest = "manifest", manifest.HexKey(key)
	return ref.String()
}

// A located payload is opened through the same identity-checked reader as
// restore. Logical parents are opaque here. Ordinary Manifest refs keep their
// existing source selection and need no extra fetch for tarstream policy.
func (p *Publisher) checkLocated(ctx context.Context, ref manifest.Ref, scope publishScope) error {
	if ref.Scheme != manifest.RefSchemeFile || ref.Location == "" {
		return nil
	}
	raw := ref.String()
	if _, seen := p.checkedLocated[scopeKey(scope, raw)]; seen {
		return nil
	}
	stream, _, err := p.open(ctx, raw, scope)
	if err != nil {
		return err
	}
	return stream.Close()
}

func (p *Publisher) retainLocated(ctx context.Context, raw string, scope publishScope) error {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return err
	}
	if ref.Scheme == manifest.RefSchemeFile && ref.Location != "" {
		if err := p.checkLocated(ctx, ref, scope); err != nil {
			return err
		}
		p.original(raw, scope)
		p.retained(raw, scope)
	}
	return nil
}
func (p *Publisher) retainLocatedDisks(ctx context.Context, cfg *config.PortableSandboxConfig, scope publishScope) error {
	for _, dep := range portableDiskRefs(cfg) {
		if err := p.retainLocated(ctx, dep.raw, scope); err != nil {
			return err
		}
	}
	return nil
}

// A relative Bundle source inherits the current carrier's already-known named
// location. It remains a different physical carrier; selectors must name it.
func selectedBundleCarrier(current, selected string) string {
	if selected == "" {
		return current
	}
	ref, err := manifest.ParseRef(selected)
	if err != nil {
		return selected
	}
	parent, err := manifest.ParseRef(current)
	if err == nil && ref.Scheme == manifest.RefSchemeFile && ref.Location == "" && filepath.Base(ref.Path) == ref.Path {
		ref.Location = parent.Location
	}
	return ref.String()
}

func (p *Publisher) publishCheckpointBundle(ctx context.Context, raw string, scope publishScope) (PublishResult, error) {
	stream, child, err := p.open(ctx, raw, scope)
	if err != nil {
		return PublishResult{}, err
	}
	root, err := sandboxfile.Open(ctx, stream)
	if err == nil {
		ref, err := p.publishSandboxRoot(ctx, raw, child, root)
		return p.result(RoleSandbox, ref, ref), err
	}
	stream, child, err = p.open(ctx, raw, scope)
	if err != nil {
		return PublishResult{}, err
	}
	snap, err := snapshotfile.Open(ctx, stream)
	if err != nil {
		return PublishResult{}, err
	}
	ref, e, err := p.publishSnapshotRoot(ctx, raw, child, snap)
	return p.result(RoleSnapshot, ref, e), err
}

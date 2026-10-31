package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/internal/sourcebind"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

// RunSource is an opened Sandbox artifact and its scoped lookup/provenance.
// It owns the root stream and borrows the ProcessStorage passed to OpenRunSource.
// Keep both open until every runtime using an ordinary Apply result has exited
// and Wait reports its result, or Close returns. Canceling a Wait does not end
// this borrowing. ReplaceBoot results do not borrow the source and can outlive it.
// Opening or applying a source does not start a VM or acquire checkpoint writers.
// A RunSource must not be copied after first use.
type RunSource struct {
	mu     sync.Mutex
	closed bool

	root          *sandboxfile.Root
	storage       *artifact.ProcessStorage
	locations     config.RefLocations
	portableRef   string
	runtimeRef    string
	relativeDir   string
	fetcher       fetch.Fetcher
	bundleReader  *manifestbundle.Reader
	bundleFetcher *manifestbundle.ManifestFetcher
	bundleSource  *BundleSourceBinding
}

// Close releases the source root exactly once without closing borrowed storage.
// It is safe to call repeatedly or on a nil RunSource. Apply after Close fails.
func (s *RunSource) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.root.Close()
}

// Apply applies the shared run-from rules to a copy of host, preserving inherited
// descriptors and presence-aware overrides. For ordinary inheritance the config
// keeps its source graph unbound; Run or Launch binds it during preparation.
// The LaunchSpec carries the workload and complete source access, while stdio,
// forwards and RuntimeSpec host settings remain the caller's responsibility.
// ReplaceBoot requires a complete host boot definition and retains neither
// source provenance nor source-scoped selectors. Its Storage is still borrowed.
func (s *RunSource) Apply(host *config.SandboxConfig, presence config.FieldPresence, options config.ApplyFromOptions) (*config.SandboxConfig, LaunchSpec, error) {
	if s == nil {
		return nil, LaunchSpec{}, fmt.Errorf("run source: %w", os.ErrClosed)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.root == nil {
		return nil, LaunchSpec{}, fmt.Errorf("run source: %w", os.ErrClosed)
	}
	host, err := cloneSDKConfig(host)
	if err != nil {
		return nil, LaunchSpec{}, err
	}
	if !options.ReplaceBoot {
		sourcebind.ApplyDefaultBootBindings(host, s.root.Portable, s.relativeDir)
	}
	cfg, portable, err := config.ApplyFromRules(s.root.Portable, host, presence, options)
	if err != nil {
		return nil, LaunchSpec{}, err
	}
	launch := LaunchSpecFromConfig(cfg)
	launch.Storage = s.storage
	launch.RefLocations = maps.Clone(s.locations)
	if options.ReplaceBoot {
		return cfg, launch, nil
	}
	launch.ManifestConfig = s.storage.ManifestConfig()
	launch.Fetcher = s.fetcher
	launch.BundleReader = s.bundleReader
	launch.BundleFetcher = s.bundleFetcher
	launch.PortableConfig = portable
	launch.SourceBinding = &RunSourceBinding{
		SandboxRef: s.portableRef, RuntimeRef: s.runtimeRef, RelativeDir: s.relativeDir,
	}
	if s.root.Checkpoint != nil {
		checkpoint := *s.root.Checkpoint
		checkpoint.Disks = append([]sandboxfile.CheckpointNamedDisk(nil), checkpoint.Disks...)
		launch.SourceBinding.Checkpoint = &checkpoint
	}
	if s.bundleSource != nil {
		binding := *s.bundleSource
		binding.Refs = append([]string(nil), binding.Refs...)
		launch.SourceBinding.BundleSource = &binding
	}
	return cfg, launch, nil
}

// OpenRunSource opens a local path, file:// ref or manifest:// ref as a Sandbox
// source. Tarstreams, encrypted tarstreams, Manifest Bundles and checkpoint
// sources use the same carrier and read-recovery rules as the CLI. storage is
// borrowed and required; locations is copied for later Apply results.
func OpenRunSource(ctx context.Context, raw string, storage *artifact.ProcessStorage, locations config.RefLocations) (*RunSource, error) {
	if storage == nil {
		return nil, errors.New("process artifact storage is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	locations = maps.Clone(locations)
	ref, path, err := resolveRunSourceRef(raw, locations)
	if err != nil {
		return nil, err
	}
	source := &RunSource{storage: storage, locations: locations, fetcher: storage.Fetcher()}
	var stream fetch.Stream
	switch ref.Scheme {
	case manifest.RefSchemeManifest:
		if source.fetcher == nil {
			return nil, errors.New("manifest:// Sandbox requires artifact storage with a manifest fetcher")
		}
		key, err := manifest.ParseKeyRef(ref.Path)
		if err != nil {
			return nil, fmt.Errorf("Sandbox manifest ref: %w", err)
		}
		stream, err = readretry.Open(ctx, func() (fetch.Stream, error) { return source.fetcher.OpenManifest(ctx, key) })
		if err != nil {
			return nil, fmt.Errorf("open Sandbox manifest: %w", err)
		}
		source.portableRef = ref.String()
		source.runtimeRef = ref.String()
	case manifest.RefSchemeFile:
		// Located aliases must be checked against their original mapped path.
		// Bind all later provenance to the path actually selected by the opener.
		opened, err := storage.OpenFileWithLocations(ctx, path, ref, locations)
		if err != nil {
			return nil, fmt.Errorf("open Sandbox file: %w", err)
		}
		stream = opened
		path = opened.ResolvedPath()
		resolvedRef := ref
		if resolvedRef.Location == "" {
			resolvedRef.Path = path
		} else {
			resolvedRef.Path = filepath.Base(path)
		}
		source.relativeDir = filepath.Dir(path)
		if rootKey, bundle := opened.RootManifestKey(); bundle {
			resolvedRef.DigestScheme = "manifest"
			resolvedRef.Digest = manifest.HexKey(rootKey)
			portable := resolvedRef
			portable.Path = filepath.Base(path)
			source.portableRef = portable.String()
			source.fetcher = opened.ScopedFetcher()
			source.bundleReader = opened.BundleReader()
			source.bundleFetcher = opened.ManifestFetcher()
			source.bundleSource, err = runBundleSourceBinding(opened, ref)
			if err != nil {
				_ = opened.Close()
				return nil, err
			}
			source.bundleSource.Reader = source.bundleReader
			source.bundleSource.Fetcher = source.bundleFetcher
		} else {
			digester, ok := any(opened).(tarstream.Digester)
			if !ok {
				_ = opened.Close()
				return nil, errors.New("local Sandbox has no declared content identity")
			}
			scheme, digest := digester.Digest()
			if scheme == "" || digest == "" {
				_ = opened.Close()
				return nil, errors.New("local Sandbox has an empty content identity")
			}
			resolvedRef.DigestScheme, resolvedRef.Digest = scheme, digest
			portable := resolvedRef
			portable.Path = digest + ".sandbox"
			source.portableRef = portable.String()
		}
		source.runtimeRef = resolvedRef.String()
	default:
		return nil, fmt.Errorf("unsupported Sandbox source scheme %q", ref.Scheme)
	}

	root, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		return nil, err
	}
	source.root = root
	return source, nil
}

func runBundleSourceBinding(opened *artifact.OpenedFile, ref manifest.Ref) (*BundleSourceBinding, error) {
	reader := opened.BundleReader()
	if reader == nil {
		return nil, errors.New("Sandbox Bundle reader is unavailable")
	}
	realPath := opened.ResolvedPath()
	ref.Path = filepath.Base(realPath)
	ref.DigestScheme, ref.Digest = "", ""
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if _, err := manifestbundle.EncodeRefs([]string{ref.String()}); err != nil {
		return nil, err
	}
	return &BundleSourceBinding{RootRef: ref.String(), RootPath: realPath, Refs: reader.Refs()}, nil
}

func resolveRunSourceRef(raw string, locations config.RefLocations) (manifest.Ref, string, error) {
	if raw == "" {
		return manifest.Ref{}, "", errors.New("empty Sandbox source")
	}
	if !strings.HasPrefix(raw, "file://") && !strings.HasPrefix(raw, "manifest://") {
		path, err := filepath.Abs(raw)
		if err != nil {
			return manifest.Ref{}, "", err
		}
		return manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path}, path, nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return manifest.Ref{}, "", err
	}
	if ref.Scheme != manifest.RefSchemeFile {
		return ref, "", nil
	}
	path, err := locations.ResolveFile(ref, "")
	if err != nil {
		return manifest.Ref{}, "", err
	}
	if !filepath.IsAbs(path) {
		path, err = filepath.Abs(path)
		if err != nil {
			return manifest.Ref{}, "", err
		}
	}
	return ref, path, nil
}

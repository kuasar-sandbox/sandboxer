package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync/atomic"

	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

// coldPreparation contains the immutable metadata learned by workload preflight.
// It owns no readers, disk writers, checkpoint locks, or runtime resources.
// Run and Launch apply its image defaults at their existing lifecycle boundary;
// a portable source's C0 is never reprojected from those defaults.
type coldPreparation struct {
	imageDefaults *ImageConfig
	sealed        *sealedColdPreparation
}

// sealedColdPreparation is a one-use proof for Run's already-canonicalized
// launch. The fingerprint freezes values without retaining another copy of
// credential/config payloads. Access handles remain borrowed, not transferable.
type sealedColdPreparation struct {
	inputs [sha256.Size]byte
	access coldPreparationAccess
	used   atomic.Bool
}

type coldPreparationAccess struct {
	legacy        *RunOptions
	storage       *artifact.ProcessStorage
	fetcher       fetch.Fetcher
	effective     fetch.Fetcher
	codec         tarstream.Codec
	bundleReader  *manifestbundle.Reader
	bundleFetcher *manifestbundle.ManifestFetcher
	sourceReader  *manifestbundle.Reader
	sourceFetcher *manifestbundle.ManifestFetcher
}

func preparationAccess(spec LaunchSpec) coldPreparationAccess {
	a := coldPreparationAccess{
		legacy: spec.legacyAccess, storage: spec.Storage, fetcher: spec.Fetcher,
		effective: launchFetcher(spec), codec: launchCodec(spec),
		bundleReader: spec.BundleReader, bundleFetcher: spec.BundleFetcher,
	}
	if spec.SourceBinding != nil && spec.SourceBinding.BundleSource != nil {
		a.sourceReader = spec.SourceBinding.BundleSource.Reader
		a.sourceFetcher = spec.SourceBinding.BundleSource.Fetcher
	}
	return a
}

func (a coldPreparationAccess) matches(b coldPreparationAccess) bool {
	return a.legacy == b.legacy && a.storage == b.storage &&
		a.bundleReader == b.bundleReader && a.bundleFetcher == b.bundleFetcher &&
		a.sourceReader == b.sourceReader && a.sourceFetcher == b.sourceFetcher &&
		samePreparationHandle(a.fetcher, b.fetcher) && samePreparationHandle(a.effective, b.effective) &&
		samePreparationHandle(a.codec, b.codec)
}

func samePreparationHandle(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	x, y := reflect.ValueOf(a), reflect.ValueOf(b)
	// Non-comparable value capabilities conservatively repeat preflight. Do
	// not guess equality by inspecting mutable implementation state.
	return x.Type() == y.Type() && x.Comparable() && y.Comparable() && a == b
}

func (p *coldPreparation) seal(shape RuntimeSpec, launch *LaunchSpec) error {
	if p == nil || p.sealed != nil || launch == nil || launch.legacyAccess == nil || launch.Storage == nil || launch.Credentials != nil {
		return errors.New("cold preparation requires one complete Run launch")
	}
	pathID, err := ResolvePathID(shape.SandboxID, shape.PathID)
	if err != nil {
		return err
	}
	cfg, err := launch.config(shape.config())
	if err != nil {
		return err
	}
	inputs, err := coldPreparationInputs(shape, *launch, cfg, shape.SandboxID, pathID, DefaultBaseDir(shape.BaseRoot, pathID))
	if err != nil {
		return err
	}
	// Detach the private access capsule from Run's options before sealing it.
	// Function-valued key providers are bound by this capsule's identity, never
	// by a function code address (which cannot distinguish closures).
	access := *launch.legacyAccess
	launch.legacyAccess = &access
	p.sealed = &sealedColdPreparation{inputs: inputs, access: preparationAccess(*launch)}
	launch.coldPreparation = p
	return nil
}

func (p *coldPreparation) reuse(r *Runtime, spec LaunchSpec, cfg *config.SandboxConfig) bool {
	if p == nil || p.sealed == nil || spec.Credentials != nil || !p.sealed.access.matches(preparationAccess(spec)) {
		return false
	}
	inputs, err := coldPreparationInputs(r.spec, spec, cfg, r.sandboxID, r.pathID, r.baseDir)
	return err == nil && inputs == p.sealed.inputs && p.sealed.used.CompareAndSwap(false, true)
}

func coldPreparationInputs(shape RuntimeSpec, spec LaunchSpec, cfg *config.SandboxConfig, sandboxID, pathID, baseDir string) ([sha256.Size]byte, error) {
	var source *RunSourceBinding
	if spec.SourceBinding != nil {
		copy := *spec.SourceBinding
		source = &copy
		if source.BundleSource != nil {
			bundle := *source.BundleSource
			bundle.Reader, bundle.Fetcher = nil, nil
			source.BundleSource = &bundle
		}
	}
	raw, err := json.Marshal(struct {
		Config                     *config.SandboxConfig
		CgroupFD                   int
		Portable                   *config.PortableSandboxConfig
		Source                     *RunSourceBinding
		Locations                  config.RefLocations
		Manifest, StorageManifest  *config.ManifestConfig
		Disks                      []RuntimeDiskSpec
		SandboxID, PathID, BaseDir string
		RuntimeRoot, BaseRoot      string
		Checkpoint                 bool
		CheckpointPlan             *CheckpointDiskPlan
		LocalRequired              bool
	}{
		Config: cfg, CgroupFD: cfg.Resources.Control.CgroupFD, Portable: spec.PortableConfig,
		Source: source, Locations: spec.RefLocations, Manifest: spec.ManifestConfig, StorageManifest: spec.Storage.ManifestConfig(),
		Disks: shape.Disks, SandboxID: sandboxID, PathID: pathID, BaseDir: baseDir,
		RuntimeRoot: shape.RuntimeRoot, BaseRoot: shape.BaseRoot,
		Checkpoint: shape.EnableCheckpoint, CheckpointPlan: shape.checkpointPlan, LocalRequired: launchRequired(spec),
	})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer clear(raw)
	return sha256.Sum256(raw), nil
}

func preflightColdArtifacts(
	ctx context.Context,
	cfg *config.SandboxConfig,
	fetcher fetch.Fetcher,
	locations config.RefLocations,
	baseDir, sandboxID string,
	codec tarstream.Codec,
	diffCustomerKey [32]byte,
	required bool,
	opener FileStreamOpener,
	checkpointPlans ...*CheckpointDiskPlan,
) (*coldPreparation, error) {
	var plan *CheckpointDiskPlan
	if len(checkpointPlans) > 0 {
		plan = checkpointPlans[0]
	}
	binding := func(i int) *CheckpointDiskBinding {
		if plan == nil || i >= len(plan.Disks) {
			return nil
		}
		return &plan.Disks[i]
	}
	imageDefaults := &ImageConfig{}
	root := &cfg.Boot.Root
	if root.Base != "" && !cfg.SingleDisk() {
		reader, _, err := OpenRootImageBlockReaderWithOpener(ctx, root.Base, fetcher, locations, codec, required, opener)
		if err != nil {
			return nil, fmt.Errorf("preflight boot.root.base: %w", err)
		}
		imageCfg, imageErr := LoadImageConfigFrom(reader, reader.Size())
		if imageErr == nil {
			_, imageErr = MergeLaunch(imageCfg, cfg.Launch)
		}
		closeErr := reader.Close()
		if imageErr != nil {
			return nil, errors.Join(fmt.Errorf("preflight root image/launch: %w", imageErr), closeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("preflight boot.root.base close: %w", closeErr)
		}
		imageDefaults = imageCfg
	}
	if err := preflightWritableExt4(ctx, root, "boot.root", DefaultDiffURIForBaseDir(baseDir, sandboxID),
		fetcher, locations, codec, diffCustomerKey, required, opener, binding(0)); err != nil {
		return nil, err
	}
	for i := range cfg.Boot.Disks {
		disk := &cfg.Boot.Disks[i].RootConfig
		if !disk.Single() {
			reader, _, err := OpenRootImageBlockReaderWithOpener(ctx, disk.Base, fetcher, locations, codec, required, opener)
			if err != nil {
				return nil, fmt.Errorf("preflight boot.disks[%d].base: %w", i, err)
			}
			if err := reader.Close(); err != nil {
				return nil, fmt.Errorf("preflight boot.disks[%d].base close: %w", i, err)
			}
		}
		defaultDiff := DefaultDiskDiffURI(baseDir, sandboxID, fmt.Sprintf("disk%d", i))
		if err := preflightWritableExt4(ctx, disk, fmt.Sprintf("boot.disks[%d]", i), defaultDiff,
			fetcher, locations, codec, diffCustomerKey, required, opener, binding(i+1)); err != nil {
			return nil, err
		}
	}
	if cfg.SingleDisk() {
		if _, err := MergeLaunch(&ImageConfig{}, cfg.Launch); err != nil {
			return nil, fmt.Errorf("preflight launch: %w", err)
		}
	}
	return &coldPreparation{imageDefaults: imageDefaults}, nil
}

const ext4SuperblockMagicOffset = int64(1024 + 0x38)

func preflightWritableExt4(
	ctx context.Context,
	root *config.RootConfig,
	field, defaultDiffURI string,
	fetcher fetch.Fetcher,
	locations config.RefLocations,
	codec tarstream.Codec,
	diffCustomerKey [32]byte,
	required bool,
	opener FileStreamOpener,
	checkpointBindings ...*CheckpointDiskBinding,
) (retErr error) {
	var baseRefs []string
	diffURI, templateURI := root.Diff, root.DiffTemplate
	if root.Single() {
		if root.Base != "" {
			baseRefs = append([]string{root.Base}, root.BaseFromRefs...)
		}
	} else {
		diffURI, templateURI = root.Overlay.Diff, root.Overlay.DiffTemplate
		if root.Overlay.Base != "" {
			baseRefs = append([]string{root.Overlay.Base}, root.Overlay.BaseFromRefs...)
		}
	}

	var base diffstore.BlockReader
	if len(baseRefs) != 0 {
		reader, _, err := OpenLayeredBlockReaderWithOpener(ctx, baseRefs, fetcher, locations, codec, required, opener)
		if err != nil {
			return fmt.Errorf("preflight %s immutable ext4 layers: %w", field, err)
		}
		base = reader
		defer func() {
			retErr = errors.Join(retErr, base.Close())
		}()
	}

	if diffURI == "" {
		diffURI = defaultDiffURI
	}
	scheme, diffPath, ok := config.SchemeAndPath(diffURI)
	if !ok || scheme != "file" {
		return fmt.Errorf("preflight %s writable diff invalid URI: %s", field, diffURI)
	}
	var cowOptions []diffstore.DiskOption
	if len(checkpointBindings) > 0 && checkpointBindings[0] != nil {
		b := checkpointBindings[0]
		cowOptions = append(cowOptions, diffstore.WithCheckpoint(b.DiffID, b.BaseID))
	}
	if codec != nil {
		cowOptions = append(cowOptions, diffstore.WithDiffEncryption(diffCustomerKey, required))
	}
	info, statErr := os.Stat(diffPath)
	switch {
	case statErr == nil:
		if info.Size() == 0 {
			return fmt.Errorf("preflight %s writable diff %s is empty; provide a formatted ext4 diff", field, diffPath)
		}
		if err := diffstore.ValidateExistingDiffExt4(ctx, diffPath, base, cowOptions...); err != nil {
			return fmt.Errorf("preflight %s writable diff: %w", field, err)
		}
		return nil
	case !os.IsNotExist(statErr):
		return fmt.Errorf("preflight %s writable diff stat: %w", field, statErr)
	case templateURI != "":
		templateScheme, templatePath, templateOK := config.SchemeAndPath(templateURI)
		if !templateOK || templateScheme != "file" {
			return fmt.Errorf("preflight %s diff_template invalid URI: %s", field, templateURI)
		}
		if err := diffstore.ValidateDiffTemplateExt4(ctx, templatePath, base, cowOptions...); err != nil {
			return fmt.Errorf("preflight %s diff_template: %w", field, err)
		}
		return nil
	case base != nil:
		if err := validateExt4BlockReader(ctx, base); err != nil {
			return fmt.Errorf("preflight %s immutable ext4 layers: %w", field, err)
		}
		return nil
	default:
		return fmt.Errorf("preflight %s has no formatted ext4 source", field)
	}
}

func validateExt4BlockReader(ctx context.Context, reader diffstore.BlockReader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader.Size() < ext4SuperblockMagicOffset+2 {
		return errors.New("effective writable disk is too small for an ext4 superblock")
	}
	var magic [2]byte
	n, err := reader.ReadAt(magic[:], ext4SuperblockMagicOffset)
	if err != nil && (readretry.IsTerminal(err) || readerr.IsPermanent(err) || !(errors.Is(err, io.EOF) && n == len(magic))) {
		return fmt.Errorf("read ext4 superblock magic: %w", err)
	}
	if n != len(magic) {
		return io.ErrUnexpectedEOF
	}
	if magic != [2]byte{0x53, 0xef} {
		return errors.New("effective writable disk is not a formatted ext4 filesystem")
	}
	return ctx.Err()
}

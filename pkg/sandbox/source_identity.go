package sandbox

import (
	"errors"
	"fmt"
	"maps"
	"reflect"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

// runSourceIdentity is minted only by RunSource.Apply after opening E. It freezes
// the source identity and portable boot identities without retaining a source,
// storage, reader, writer, or checkpoint owner. Carrying the original LaunchSpec
// preserves this record; reconstructing its public fields does not recreate it.
type runSourceIdentity struct {
	boot   config.PortableProjection
	source runSourceBindingIdentity
}

type runSourceBindingIdentity struct {
	sandboxRef, runtimeRef, relativeDir string
	checkpoint                          *sandboxfile.Checkpoint
	locations                           config.RefLocations
	bundle                              *runSourceBundleIdentity
}

type runSourceBundleIdentity struct {
	rootRef, rootPath string
	refs              []string
}

func sourceBindingIdentity(source *RunSourceBinding, locations config.RefLocations) runSourceBindingIdentity {
	identity := runSourceBindingIdentity{
		sandboxRef: source.SandboxRef, runtimeRef: source.RuntimeRef, relativeDir: source.RelativeDir,
		locations: maps.Clone(locations),
	}
	if source.Checkpoint != nil {
		checkpoint := *source.Checkpoint
		checkpoint.Disks = append([]sandboxfile.CheckpointNamedDisk(nil), checkpoint.Disks...)
		identity.checkpoint = &checkpoint
	}
	if source.BundleSource != nil {
		bundle := source.BundleSource
		identity.bundle = &runSourceBundleIdentity{
			rootRef: bundle.RootRef, rootPath: bundle.RootPath, refs: append([]string(nil), bundle.Refs...),
		}
	}
	return identity
}

func newRunSourceIdentity(launch LaunchSpec) *runSourceIdentity {
	return &runSourceIdentity{
		boot: config.PortableProjection{
			KernelRef: launch.PortableConfig.Boot.Kernel, RuntimeRef: launch.PortableConfig.Boot.Runtime,
		},
		source: sourceBindingIdentity(launch.SourceBinding, launch.RefLocations),
	}
}

func (identity *runSourceIdentity) validate(launch LaunchSpec) error {
	if identity == nil || launch.sourceProof != identity {
		return errors.New("source identity requires the LaunchSpec returned by RunSource.Apply")
	}
	if launch.PortableConfig == nil || launch.PortableConfig.Boot.Kernel != identity.boot.KernelRef ||
		launch.PortableConfig.Boot.Runtime != identity.boot.RuntimeRef {
		return errors.New("portable boot identity changed after RunSource.Apply")
	}
	if err := launch.PortableConfig.Validate(); err != nil {
		return fmt.Errorf("source portable config: %w", err)
	}
	if launch.SourceBinding == nil || !reflect.DeepEqual(identity.source, sourceBindingIdentity(launch.SourceBinding, launch.RefLocations)) {
		return errors.New("source provenance changed after RunSource.Apply")
	}
	return nil
}

// deriveSourceHostIdentity records the host bindings chosen for this shape. It
// does not inspect host files: shape inspection is useful before deployment, and
// StartRuntime must observe the sidecar/footer present at actual startup time.
func deriveSourceHostIdentity(cfg *config.SandboxConfig, launch LaunchSpec) (*preparedHostIdentity, error) {
	if launch.sourceProof == nil {
		return nil, nil
	}
	if err := launch.sourceProof.validate(launch); err != nil {
		return nil, err
	}
	return &preparedHostIdentity{
		kernel: cfg.Boot.Kernel, bundle: cfg.Boot.Runtime, sourceProof: launch.sourceProof,
	}, nil
}

// preparePortableHostIdentity is the shared portable cold-boot policy (#158).
// A present sidecar must match C0. Without one, the readable kernel binding is
// checked but its bytes are not hashed or claimed as independently verified.
// Runtime Bundle identity always comes from its footer and must match C0.
func preparePortableHostIdentity(cfg *config.SandboxConfig, expected config.PortableProjection) (preparedHostIdentity, error) {
	prepared := preparedHostIdentity{kernel: cfg.Boot.Kernel, bundle: cfg.Boot.Runtime}
	if err := VerifyKernelArtifact(cfg.Boot.Kernel); err != nil {
		return prepared, fmt.Errorf("boot.kernel identity: %w", err)
	}
	kernelRef, present, err := optionalKernelRef(cfg.Boot.Kernel)
	if err != nil {
		return prepared, fmt.Errorf("boot.kernel identity: %w", err)
	}
	if present && expected.KernelRef != kernelRef {
		return prepared, fmt.Errorf("boot.kernel identity mismatch: portable %s, host %s", expected.KernelRef, kernelRef)
	}
	runtimeRef, err := ResolveRuntimeProjection(cfg)
	if err != nil {
		return prepared, err
	}
	if expected.RuntimeRef != runtimeRef {
		return prepared, fmt.Errorf("boot.runtime identity mismatch: portable %s, host %s", expected.RuntimeRef, runtimeRef)
	}
	prepared.projection = config.PortableProjection{KernelRef: kernelRef, RuntimeRef: runtimeRef}
	prepared.unknownKernel = !present
	return prepared, nil
}

func resolvePreparedHostIdentity(cfg *config.SandboxConfig, prepared *preparedHostIdentity) (*preparedHostIdentity, error) {
	if prepared == nil {
		return nil, nil
	}
	if prepared.kernel != cfg.Boot.Kernel || prepared.bundle != cfg.Boot.Runtime {
		return nil, errors.New("prepared host identity bindings differ from runtime spec")
	}
	if prepared.sourceProof == nil {
		return prepared, nil
	}
	resolved, err := preparePortableHostIdentity(cfg, prepared.sourceProof.boot)
	if err != nil {
		return nil, err
	}
	resolved.sourceProof = prepared.sourceProof
	return &resolved, nil
}

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/util"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

const (
	// SandboxRuntimeConfigName is the only portable runtime-config filename,
	// both in a live run directory and in a Sandbox artifact's trailing ZIP.
	SandboxRuntimeConfigName = "sandbox.runtime.cfg"

	PortableSandboxConfigVersion = 1

	MaxPortableConfigBytes      = 1 << 20
	MaxPortableFiles            = 256
	MaxPortableFileContentBytes = 256 << 10
	MaxPortableInlineContent    = 1 << 20
	MaxPortableEnvEntries       = 512
	MaxPortableMounts           = 128
	MaxPortableInit             = 64
	MaxPortablePlugins          = 64
	MaxPortableMetadataEntries  = 256
	MaxPortableScalarBytes      = 64 << 10
	MaxPortableLayerRefs        = 64
	MaxPortableArtifactRefs     = 256
)

// PortableSandboxConfig is the strict, versioned cold-start contract stored
// in sandbox.runtime.cfg. It deliberately uses dedicated types instead of
// serializing SandboxConfig, so host bindings and instance-only input cannot
// accidentally cross the artifact boundary.
type PortableSandboxConfig struct {
	Version   int                     `yaml:"version"`
	Resources PortableResourcesConfig `yaml:"resources"`
	Network   PortableNetworkConfig   `yaml:"network"`
	Boot      PortableBootConfig      `yaml:"boot"`
	Launch    PortableLaunchConfig    `yaml:"launch"`
	Mounts    []MountConfig           `yaml:"mounts,omitempty"`
	Files     []FileConfig            `yaml:"files,omitempty"`
	Init      []InitConfig            `yaml:"init,omitempty"`
	Metadata  map[string]string       `yaml:"metadata,omitempty"`
}

type PortableResourcesConfig struct {
	Capacity    CapacityConfig    `yaml:"capacity"`
	Allocatable AllocatableConfig `yaml:"allocatable"`
}

// PortableNetworkConfig records guest topology only. Provider, L3 identity,
// MAC and hostname remain host/instance bindings.
type PortableNetworkConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Interface string `yaml:"interface,omitempty"`
}

type PortableBootConfig struct {
	Kernel  string               `yaml:"kernel"`
	Cmdline string               `yaml:"cmdline,omitempty"`
	Runtime string               `yaml:"runtime"`
	Root    PortableRootConfig   `yaml:"root"`
	Disks   []PortableDiskConfig `yaml:"disks,omitempty"`
}

type PortableDiskConfig struct {
	Name               string `yaml:"name"`
	PortableRootConfig `yaml:",inline"`
}

type PortableRootConfig struct {
	Base         string                 `yaml:"base,omitempty"`
	BaseFromRefs []string               `yaml:"base_from_refs,omitempty"`
	Overlay      *PortableOverlayConfig `yaml:"overlay,omitempty"`
}

func (r *PortableRootConfig) Single() bool { return r == nil || r.Overlay == nil }

type PortableOverlayConfig struct {
	Base         string   `yaml:"base,omitempty"`
	BaseFromRefs []string `yaml:"base_from_refs,omitempty"`
}

// PortableLaunchConfig mirrors the guest-visible/persistent LaunchConfig
// fields. StartTimeout and EphemeralEnv are intentionally absent.
type PortableLaunchConfig struct {
	Exec            string            `yaml:"exec,omitempty"`
	Args            []string          `yaml:"args,omitempty"`
	Env             map[string]string `yaml:"env,omitempty"`
	Workdir         string            `yaml:"workdir,omitempty"`
	Restart         string            `yaml:"restart,omitempty"`
	CgroupControl   bool              `yaml:"cgroup_control,omitempty"`
	Placeholder     bool              `yaml:"placeholder,omitempty"`
	PIDNamespace    string            `yaml:"pid_namespace,omitempty"`
	Plugin          []PluginConfig    `yaml:"plugin,omitempty"`
	User            string            `yaml:"user,omitempty"`
	StopSignal      string            `yaml:"stop_signal,omitempty"`
	StopGracePeriod string            `yaml:"stop_grace_period,omitempty"`
}

// PortableProjection supplies identities whose source paths are host-only.
// Every value is already a canonical portable ref (basename + identity).
type PortableProjection struct {
	KernelRef  string
	RuntimeRef string
}

// ProjectPortableCold creates the immutable C0 graph for an explicit cold
// run. The root active writable diff occupies the reserved self position;
// configured immutable upper lowers are retained beneath it. Data active
// diffs remain runtime bindings until an export replaces their graph tops.
// Callers must canonicalize every configured immutable disk ref first.
func ProjectPortableCold(cfg *SandboxConfig, identities PortableProjection) (*PortableSandboxConfig, error) {
	if cfg == nil {
		return nil, errors.New("portable config: nil sandbox config")
	}
	p := &PortableSandboxConfig{
		Version: PortableSandboxConfigVersion,
		Resources: PortableResourcesConfig{
			Capacity:    cfg.Resources.Capacity,
			Allocatable: cloneAllocatable(cfg.Resources.Allocatable),
		},
		Network: PortableNetworkConfig{
			Enabled:   cfg.Network.hasSource(),
			Interface: cfg.Network.Interface,
		},
		Boot: PortableBootConfig{
			Kernel:  identities.KernelRef,
			Cmdline: cfg.Boot.Cmdline,
			Runtime: identities.RuntimeRef,
		},
		Launch:   portableLaunch(cfg.Launch),
		Mounts:   cloneMounts(cfg.Mounts),
		Files:    cloneFiles(cfg.Files),
		Init:     cloneInit(cfg.Init),
		Metadata: cloneMap(cfg.Metadata),
	}

	root, err := projectColdRoot(&cfg.Boot.Root, true)
	if err != nil {
		return nil, fmt.Errorf("portable boot.root: %w", err)
	}
	p.Boot.Root = root
	p.Boot.Disks = make([]PortableDiskConfig, len(cfg.Boot.Disks))
	for i := range cfg.Boot.Disks {
		root, err := projectColdRoot(&cfg.Boot.Disks[i].RootConfig, false)
		if err != nil {
			return nil, fmt.Errorf("portable boot.disks[%d]: %w", i, err)
		}
		p.Boot.Disks[i] = PortableDiskConfig{Name: cfg.Boot.Disks[i].Name, PortableRootConfig: root}
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func projectColdRoot(root *RootConfig, isRoot bool) (PortableRootConfig, error) {
	if root == nil {
		return PortableRootConfig{}, errors.New("nil disk config")
	}
	if root.Overlay != nil {
		base, err := portableArtifactRef(root.Base)
		if err != nil {
			return PortableRootConfig{}, fmt.Errorf("base: %w", err)
		}
		upperBase, err := portableArtifactRef(root.Overlay.Base)
		if err != nil {
			return PortableRootConfig{}, fmt.Errorf("overlay.base: %w", err)
		}
		chain, err := portableArtifactRefs(root.Overlay.BaseFromRefs)
		if err != nil {
			return PortableRootConfig{}, fmt.Errorf("overlay.base_from_refs: %w", err)
		}
		if upperBase != "" {
			chain = append([]string{upperBase}, chain...)
		}
		upper := &PortableOverlayConfig{BaseFromRefs: chain}
		if isRoot {
			upper.Base = "self"
		}
		return PortableRootConfig{Base: base, Overlay: upper}, nil
	}

	base, err := portableArtifactRef(root.Base)
	if err != nil {
		return PortableRootConfig{}, fmt.Errorf("base: %w", err)
	}
	chain, err := portableArtifactRefs(root.BaseFromRefs)
	if err != nil {
		return PortableRootConfig{}, fmt.Errorf("base_from_refs: %w", err)
	}
	if isRoot {
		if base != "" {
			chain = append([]string{base}, chain...)
		}
		return PortableRootConfig{Base: "self", BaseFromRefs: chain}, nil
	}
	return PortableRootConfig{Base: base, BaseFromRefs: chain}, nil
}

// NewPortableEROFS constructs the C0/C1 graph for an offline flattened EROFS
// source. The non-nil empty overlay encodes the normal two-device cold layout;
// the destination host must bind a formatted writable upper.
func NewPortableEROFS(cfg *SandboxConfig, identities PortableProjection) (*PortableSandboxConfig, error) {
	p, err := ProjectPortableCold(cfg, identities)
	if err != nil {
		return nil, err
	}
	p.Boot.Root = PortableRootConfig{Base: "self", Overlay: &PortableOverlayConfig{}}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// Exported returns C1 for one point-in-time disk capture. The new root payload
// is represented by self and therefore has no independently knowable ref;
// dataRefs contains boot.disks[] captures in order. parentSandboxRef is the
// current E bound to the old self, or empty for an explicit cold run. merged
// is root+data order; a true entry means the captured payload already absorbed
// its direct parent, so C1 retains only the parent's lower chain.
func (p *PortableSandboxConfig) Exported(parentSandboxRef string, dataRefs []string, merged []bool) (*PortableSandboxConfig, error) {
	if p == nil {
		return nil, errors.New("portable export: nil C0")
	}
	if len(dataRefs) != len(p.Boot.Disks) {
		return nil, fmt.Errorf("portable export: got %d data disk refs, want %d", len(dataRefs), len(p.Boot.Disks))
	}
	if len(merged) != 1+len(p.Boot.Disks) {
		return nil, fmt.Errorf("portable export: got %d merge decisions, want %d", len(merged), 1+len(p.Boot.Disks))
	}
	clone, err := p.Clone()
	if err != nil {
		return nil, err
	}
	parent, err := portableArtifactRef(parentSandboxRef)
	if err != nil {
		return nil, fmt.Errorf("portable export parent Sandbox: %w", err)
	}
	if clone.Boot.Root.Base == "self" && clone.Boot.Root.Overlay != nil && clone.Boot.Root.Overlay.Base == "" {
		// A direct EROFS E becomes the immutable lower; the captured ext4 upper
		// is the new E payload.
		if parent != "" && !merged[0] {
			clone.Boot.Root.Base = parent
		}
		clone.Boot.Root.Overlay.Base = "self"
	} else if clone.Boot.Root.Base == "self" {
		if parent != "" && !merged[0] {
			clone.Boot.Root.BaseFromRefs = prependUniqueRef(parent, clone.Boot.Root.BaseFromRefs)
		}
	} else if clone.Boot.Root.Overlay != nil && clone.Boot.Root.Overlay.Base == "self" {
		if parent != "" && !merged[0] {
			clone.Boot.Root.Overlay.BaseFromRefs = prependUniqueRef(parent, clone.Boot.Root.Overlay.BaseFromRefs)
		}
	} else {
		return nil, errors.New("portable export: C0 has no legal root self binding")
	}

	for i := range clone.Boot.Disks {
		captured, err := portableArtifactRef(dataRefs[i])
		if err != nil {
			return nil, fmt.Errorf("portable export data disk %d: %w", i, err)
		}
		if captured == "" || captured == "self" {
			return nil, fmt.Errorf("portable export data disk %d has no captured artifact", i)
		}
		disk := &clone.Boot.Disks[i].PortableRootConfig
		if disk.Overlay == nil {
			if disk.Base != "" && !merged[i+1] {
				disk.BaseFromRefs = prependUniqueRef(disk.Base, disk.BaseFromRefs)
			}
			disk.BaseFromRefs = withoutPortableRef(disk.BaseFromRefs, captured)
			disk.Base = captured
			continue
		}
		if disk.Overlay.Base != "" && !merged[i+1] {
			disk.Overlay.BaseFromRefs = prependUniqueRef(disk.Overlay.Base, disk.Overlay.BaseFromRefs)
		}
		disk.Overlay.BaseFromRefs = withoutPortableRef(disk.Overlay.BaseFromRefs, captured)
		disk.Overlay.Base = captured
	}
	if err := clone.Validate(); err != nil {
		return nil, err
	}
	return clone, nil
}

// RewriteDiskArtifactRefs returns a clone with explicit root/data dependency
// refs rewritten. self and kernel/runtime identities are never rewritten.
// Bundle planning uses this after every replacement Manifest is known but
// before the writer emits E metadata.
func (p *PortableSandboxConfig) RewriteDiskArtifactRefs(replacements map[string]string) (*PortableSandboxConfig, error) {
	clone, err := p.Clone()
	if err != nil {
		return nil, err
	}
	rewrite := func(raw *string) {
		if raw == nil || *raw == "" || *raw == "self" {
			return
		}
		if replacement := replacements[*raw]; replacement != "" {
			*raw = replacement
		}
	}
	rewriteRoot := func(root *PortableRootConfig) {
		rewrite(&root.Base)
		for i := range root.BaseFromRefs {
			rewrite(&root.BaseFromRefs[i])
		}
		if root.Overlay != nil {
			rewrite(&root.Overlay.Base)
			for i := range root.Overlay.BaseFromRefs {
				rewrite(&root.Overlay.BaseFromRefs[i])
			}
		}
	}
	rewriteRoot(&clone.Boot.Root)
	for i := range clone.Boot.Disks {
		rewriteRoot(&clone.Boot.Disks[i].PortableRootConfig)
	}
	if err := clone.Validate(); err != nil {
		return nil, fmt.Errorf("portable disk ref rewrite: %w", err)
	}
	return clone, nil
}

func prependUniqueRef(ref string, refs []string) []string {
	if ref == "" {
		return append([]string(nil), refs...)
	}
	out := make([]string, 0, 1+len(refs))
	out = append(out, ref)
	for _, existing := range refs {
		if existing != ref {
			out = append(out, existing)
		}
	}
	return out
}

func withoutPortableRef(refs []string, excluded string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref != excluded {
			out = append(out, ref)
		}
	}
	return out
}

func portableLaunch(in LaunchConfig) PortableLaunchConfig {
	return PortableLaunchConfig{
		Exec: in.Exec, Args: append([]string(nil), in.Args...), Env: cloneMap(in.Env),
		Workdir: in.Workdir, Restart: in.Restart, CgroupControl: in.CgroupControl,
		Placeholder: in.Placeholder, PIDNamespace: in.PIDNamespace,
		Plugin: clonePlugins(in.Plugin), User: in.User, StopSignal: in.StopSignal,
		StopGracePeriod: in.StopGracePeriod,
	}
}

func cloneAllocatable(in AllocatableConfig) AllocatableConfig {
	out := in
	if in.DeflateOnOOM != nil {
		value := *in.DeflateOnOOM
		out.DeflateOnOOM = &value
	}
	return out
}

func cloneMounts(in []MountConfig) []MountConfig { return append([]MountConfig(nil), in...) }
func cloneFiles(in []FileConfig) []FileConfig    { return append([]FileConfig(nil), in...) }

func cloneInit(in []InitConfig) []InitConfig {
	out := make([]InitConfig, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Args = append([]string(nil), in[i].Args...)
		out[i].Env = cloneMap(in[i].Env)
	}
	return out
}

func clonePlugins(in []PluginConfig) []PluginConfig {
	out := make([]PluginConfig, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Args = append([]string(nil), in[i].Args...)
		out[i].Env = cloneMap(in[i].Env)
	}
	return out
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func portableArtifactRefs(refs []string) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]string, len(refs))
	for i, raw := range refs {
		ref, err := portableArtifactRef(raw)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out[i] = ref
	}
	return out, nil
}

func portableArtifactRef(raw string) (string, error) {
	if raw == "" || raw == "self" {
		return raw, nil
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", err
	}
	if ref.Scheme == manifest.RefSchemeFile {
		if ref.Digest == "" || ref.DigestScheme == "" {
			return "", errors.New("local immutable ref has no content identity")
		}
		ref.Path = filepath.Base(ref.Path)
		if ref.Path == "." || ref.Path == string(filepath.Separator) || ref.Path == "" {
			return "", errors.New("local immutable ref has no basename")
		}
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	return ref.String(), nil
}

// ParsePortableSandboxConfig performs bounded, duplicate-aware, strict YAML
// decoding and validates the semantic contract. Exactly one YAML document is
// accepted; aliases and merge keys are rejected.
func ParsePortableSandboxConfig(raw []byte) (*PortableSandboxConfig, error) {
	if len(raw) == 0 {
		return nil, errors.New("portable config is empty")
	}
	if len(raw) > MaxPortableConfigBytes {
		return nil, fmt.Errorf("portable config exceeds %d bytes", MaxPortableConfigBytes)
	}
	var node yaml.Node
	nodeDecoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := nodeDecoder.Decode(&node); err != nil {
		return nil, fmt.Errorf("portable config YAML: %w", err)
	}
	if len(node.Content) != 1 {
		return nil, errors.New("portable config must contain one document")
	}
	if err := validateStrictYAMLNode(node.Content[0], ""); err != nil {
		return nil, fmt.Errorf("portable config YAML: %w", err)
	}
	var extra yaml.Node
	if err := nodeDecoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("portable config contains multiple YAML documents")
		}
		return nil, fmt.Errorf("portable config trailing YAML: %w", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var cfg PortableSandboxConfig
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("portable config schema: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func validateStrictYAMLNode(node *yaml.Node, path string) error {
	if node == nil {
		return errors.New("nil YAML node")
	}
	if node.Kind == yaml.AliasNode || node.Alias != nil {
		return fmt.Errorf("%s: YAML aliases are not allowed", yamlPath(path))
	}
	switch node.Kind {
	case yaml.MappingNode:
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("%s: mapping keys must be strings", yamlPath(path))
			}
			if key.Value == "<<" {
				return fmt.Errorf("%s: YAML merge keys are not allowed", yamlPath(path))
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return fmt.Errorf("%s: duplicate field %q", yamlPath(path), key.Value)
			}
			seen[key.Value] = struct{}{}
			childPath := key.Value
			if path != "" {
				childPath = path + "." + key.Value
			}
			if err := validateStrictYAMLNode(node.Content[i+1], childPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			if err := validateStrictYAMLNode(child, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if len(node.Value) > MaxPortableScalarBytes {
			return fmt.Errorf("%s exceeds %d bytes", yamlPath(path), MaxPortableScalarBytes)
		}
	default:
		return fmt.Errorf("%s: unsupported YAML node kind %d", yamlPath(path), node.Kind)
	}
	return nil
}

func yamlPath(path string) string {
	if path == "" {
		return "document"
	}
	return path
}

// MarshalPortableSandboxConfig returns the sole canonical encoding. Parsing
// the result is part of the operation, guarding accidental renderer/schema
// drift.
func MarshalPortableSandboxConfig(cfg *PortableSandboxConfig) ([]byte, error) {
	if cfg == nil {
		return nil, errors.New("portable config: nil value")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal portable config: %w", err)
	}
	if len(raw) > MaxPortableConfigBytes {
		return nil, fmt.Errorf("portable config exceeds %d bytes", MaxPortableConfigBytes)
	}
	if _, err := ParsePortableSandboxConfig(raw); err != nil {
		return nil, fmt.Errorf("validate canonical portable config: %w", err)
	}
	return raw, nil
}

func (p *PortableSandboxConfig) Clone() (*PortableSandboxConfig, error) {
	raw, err := MarshalPortableSandboxConfig(p)
	if err != nil {
		return nil, err
	}
	return ParsePortableSandboxConfig(raw)
}

func (p *PortableSandboxConfig) Validate() error {
	if p == nil {
		return errors.New("portable config: nil value")
	}
	if p.Version != PortableSandboxConfigVersion {
		return fmt.Errorf("portable config: unsupported version %d", p.Version)
	}
	if p.Resources.Capacity.CPU <= 0 {
		return errors.New("portable resources.capacity.cpu must be > 0")
	}
	capacity, err := util.ParseSize(p.Resources.Capacity.Memory)
	if err != nil || capacity == 0 {
		return fmt.Errorf("portable resources.capacity.memory: %w", errOrValue(err))
	}
	if p.Resources.Allocatable.CPU <= 0 || p.Resources.Allocatable.CPU > float64(p.Resources.Capacity.CPU) {
		return errors.New("portable resources.allocatable.cpu must be > 0 and <= capacity.cpu")
	}
	allocatable, err := util.ParseSize(p.Resources.Allocatable.Memory)
	if err != nil || allocatable == 0 || allocatable > capacity {
		return fmt.Errorf("portable resources.allocatable.memory must be > 0 and <= capacity.memory: %w", errOrValue(err))
	}
	if p.Network.Enabled {
		if p.Network.Interface == "" {
			return errors.New("portable network.interface is required when enabled")
		}
	} else if p.Network.Interface != "" {
		return errors.New("portable network.interface requires enabled=true")
	}
	if err := validatePortableHostArtifactRef("boot.kernel", p.Boot.Kernel); err != nil {
		return err
	}
	if err := validatePortableHostArtifactRef("boot.runtime", p.Boot.Runtime); err != nil {
		return err
	}
	if len(p.Boot.Disks) > MaxDataDisks {
		return fmt.Errorf("portable boot.disks exceeds %d", MaxDataDisks)
	}
	if err := validatePortableRoot("boot.root", &p.Boot.Root, true); err != nil {
		return err
	}
	names := make(map[string]int, len(p.Boot.Disks))
	for i := range p.Boot.Disks {
		disk := &p.Boot.Disks[i]
		if disk.Name == "" {
			return fmt.Errorf("portable boot.disks[%d].name is required", i)
		}
		if previous, duplicate := names[disk.Name]; duplicate {
			return fmt.Errorf("portable boot.disks[%d].name duplicates boot.disks[%d]", i, previous)
		}
		names[disk.Name] = i
		if err := validatePortableRoot(fmt.Sprintf("boot.disks[%d]", i), &disk.PortableRootConfig, false); err != nil {
			return err
		}
	}
	if count := portableSelfCount(p); count != 1 {
		return fmt.Errorf("portable disk graph must contain self exactly once (got %d)", count)
	}
	if count := portableArtifactRefCount(p); count > MaxPortableArtifactRefs {
		return fmt.Errorf("portable disk graph exceeds %d artifact refs", MaxPortableArtifactRefs)
	}
	if len(p.Mounts) > MaxPortableMounts {
		return fmt.Errorf("portable mounts exceeds %d entries", MaxPortableMounts)
	}
	if err := validatePortableMounts(p.Mounts, names); err != nil {
		return err
	}
	if len(p.Files) > MaxPortableFiles {
		return fmt.Errorf("portable files exceeds %d entries", MaxPortableFiles)
	}
	if err := validateFiles("portable files", p.Files); err != nil {
		return err
	}
	totalInline := 0
	for i := range p.Files {
		content := len(p.Files[i].Content)
		if content > MaxPortableFileContentBytes {
			return fmt.Errorf("portable files[%d].content exceeds %d bytes", i, MaxPortableFileContentBytes)
		}
		totalInline += content
	}
	if totalInline > MaxPortableInlineContent {
		return fmt.Errorf("portable inline file content exceeds %d bytes", MaxPortableInlineContent)
	}
	if len(p.Init) > MaxPortableInit {
		return fmt.Errorf("portable init exceeds %d entries", MaxPortableInit)
	}
	if len(p.Launch.Plugin) > MaxPortablePlugins {
		return fmt.Errorf("portable launch.plugin exceeds %d entries", MaxPortablePlugins)
	}
	if len(p.Metadata) > MaxPortableMetadataEntries {
		return fmt.Errorf("portable metadata exceeds %d entries", MaxPortableMetadataEntries)
	}
	if err := validatePortableLaunch(p); err != nil {
		return err
	}
	return nil
}

func validatePortableHostArtifactRef(field, raw string) error {
	if err := validatePortableRef(field, raw, false); err != nil {
		return err
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return fmt.Errorf("portable %s: %w", field, err)
	}
	if ref.Scheme != manifest.RefSchemeFile || ref.Location != "" {
		return fmt.Errorf("portable %s must be an unlocated file:// basename identity", field)
	}
	return nil
}

func portableSelfCount(p *PortableSandboxConfig) int {
	countRoot := func(root *PortableRootConfig) int {
		if root == nil {
			return 0
		}
		count := 0
		if root.Base == "self" {
			count++
		}
		for _, ref := range root.BaseFromRefs {
			if ref == "self" {
				count++
			}
		}
		if root.Overlay != nil {
			if root.Overlay.Base == "self" {
				count++
			}
			for _, ref := range root.Overlay.BaseFromRefs {
				if ref == "self" {
					count++
				}
			}
		}
		return count
	}
	count := countRoot(&p.Boot.Root)
	for i := range p.Boot.Disks {
		count += countRoot(&p.Boot.Disks[i].PortableRootConfig)
	}
	return count
}

func portableArtifactRefCount(p *PortableSandboxConfig) int {
	countRoot := func(root *PortableRootConfig) int {
		if root == nil {
			return 0
		}
		count := len(root.BaseFromRefs)
		if root.Base != "" && root.Base != "self" {
			count++
		}
		if root.Overlay != nil {
			count += len(root.Overlay.BaseFromRefs)
			if root.Overlay.Base != "" && root.Overlay.Base != "self" {
				count++
			}
		}
		return count
	}
	count := countRoot(&p.Boot.Root)
	for i := range p.Boot.Disks {
		count += countRoot(&p.Boot.Disks[i].PortableRootConfig)
	}
	return count
}

func errOrValue(err error) error {
	if err != nil {
		return err
	}
	return errors.New("invalid value")
}

func validatePortableRef(field, raw string, allowSelf bool) error {
	if raw == "" {
		return fmt.Errorf("portable %s is required", field)
	}
	if raw == "self" {
		if allowSelf {
			return nil
		}
		return fmt.Errorf("portable %s cannot be self", field)
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return fmt.Errorf("portable %s: %w", field, err)
	}
	if ref.Scheme == manifest.RefSchemeFile {
		if filepath.IsAbs(ref.Path) || filepath.Base(ref.Path) != ref.Path || strings.ContainsAny(ref.Path, `/\\`) {
			return fmt.Errorf("portable %s local ref must use a basename", field)
		}
		if ref.DigestScheme == "" || ref.Digest == "" {
			return fmt.Errorf("portable %s local ref requires content identity", field)
		}
	}
	return nil
}

func validatePortableRoot(field string, root *PortableRootConfig, rootDisk bool) error {
	if root == nil {
		return fmt.Errorf("portable %s is required", field)
	}
	if root.Base != "" {
		if err := validatePortableRef(field+".base", root.Base, rootDisk); err != nil {
			return err
		}
	}
	if err := validatePortableRefList(field+".base_from_refs", root.BaseFromRefs); err != nil {
		return err
	}
	if root.Base != "" && containsPortableRef(root.BaseFromRefs, root.Base) {
		return fmt.Errorf("portable %s.base duplicates %s.base_from_refs", field, field)
	}
	if len(root.BaseFromRefs) != 0 && root.Base == "" {
		return fmt.Errorf("portable %s.base is required with base_from_refs", field)
	}
	if root.Overlay != nil {
		if len(root.BaseFromRefs) != 0 {
			return fmt.Errorf("portable %s.base_from_refs is single-disk only", field)
		}
		if root.Base == "" {
			return fmt.Errorf("portable %s.base is required in overlay mode", field)
		}
		if root.Overlay.Base != "" {
			if err := validatePortableRef(field+".overlay.base", root.Overlay.Base, rootDisk); err != nil {
				return err
			}
		}
		if err := validatePortableRefList(field+".overlay.base_from_refs", root.Overlay.BaseFromRefs); err != nil {
			return err
		}
		if root.Overlay.Base != "" && containsPortableRef(root.Overlay.BaseFromRefs, root.Overlay.Base) {
			return fmt.Errorf("portable %s.overlay.base duplicates %s.overlay.base_from_refs", field, field)
		}
		if len(root.Overlay.BaseFromRefs) != 0 && root.Overlay.Base == "" {
			return fmt.Errorf("portable %s.overlay.base is required with base_from_refs", field)
		}
		if rootDisk && root.Base == "self" && (root.Overlay.Base != "" || len(root.Overlay.BaseFromRefs) != 0) {
			return fmt.Errorf("portable %s.base=self requires an empty overlay graph", field)
		}
	}
	return nil
}

func containsPortableRef(refs []string, target string) bool {
	for _, ref := range refs {
		if ref == target {
			return true
		}
	}
	return false
}

func validatePortableRefList(field string, refs []string) error {
	if len(refs) > MaxPortableLayerRefs {
		return fmt.Errorf("portable %s exceeds %d entries", field, MaxPortableLayerRefs)
	}
	seen := make(map[string]int, len(refs))
	for i, ref := range refs {
		if err := validatePortableRef(fmt.Sprintf("%s[%d]", field, i), ref, false); err != nil {
			return err
		}
		if previous, duplicate := seen[ref]; duplicate {
			return fmt.Errorf("portable %s[%d] duplicates %s[%d]", field, i, field, previous)
		}
		seen[ref] = i
	}
	return nil
}

func validatePortableMounts(mounts []MountConfig, names map[string]int) error {
	mounted := make(map[string]int, len(names))
	for i, mount := range mounts {
		if !filepath.IsAbs(mount.Target) {
			return fmt.Errorf("portable mounts[%d].target must be absolute", i)
		}
		switch mount.Type {
		case "tmpfs", "empty":
			if mount.Source != "" {
				return fmt.Errorf("portable mounts[%d].source is valid only for type=disk", i)
			}
		case "disk":
			if _, exists := names[mount.Source]; !exists {
				return fmt.Errorf("portable mounts[%d].source %q names no data disk", i, mount.Source)
			}
			if previous, duplicate := mounted[mount.Source]; duplicate {
				return fmt.Errorf("portable mounts[%d] duplicates disk mount mounts[%d]", i, previous)
			}
			mounted[mount.Source] = i
		default:
			return fmt.Errorf("portable mounts[%d].type %q is unsupported", i, mount.Type)
		}
	}
	for name, diskIndex := range names {
		if _, ok := mounted[name]; !ok {
			return fmt.Errorf("portable boot.disks[%d] %q is not mounted", diskIndex, name)
		}
	}
	return nil
}

func validatePortableLaunch(p *PortableSandboxConfig) error {
	launch := &p.Launch
	if launch.Placeholder && launch.Exec != "" {
		return errors.New("portable launch.placeholder and launch.exec are mutually exclusive")
	}
	if !validRestart(launch.Restart) {
		return fmt.Errorf("portable launch.restart %q is invalid", launch.Restart)
	}
	switch launch.PIDNamespace {
	case "", "private", "shared":
	default:
		return fmt.Errorf("portable launch.pid_namespace %q is invalid", launch.PIDNamespace)
	}
	if launch.StopSignal != "" {
		if _, err := ParseStopSignal(launch.StopSignal); err != nil {
			return fmt.Errorf("portable launch.stop_signal: %w", err)
		}
	}
	if launch.StopGracePeriod != "" {
		if _, err := timeParseDurationPositiveOrZero(launch.StopGracePeriod); err != nil {
			return fmt.Errorf("portable launch.stop_grace_period: %w", err)
		}
	}
	for i, plugin := range launch.Plugin {
		if plugin.Exec == "" {
			return fmt.Errorf("portable launch.plugin[%d].exec is required", i)
		}
		if !validRestart(plugin.Restart) {
			return fmt.Errorf("portable launch.plugin[%d].restart %q is invalid", i, plugin.Restart)
		}
	}
	for i, init := range p.Init {
		if init.Exec == "" {
			return fmt.Errorf("portable init[%d].exec is required", i)
		}
		if init.Timeout != "" {
			if _, err := timeParseDurationPositiveOrZero(init.Timeout); err != nil {
				return fmt.Errorf("portable init[%d].timeout: %w", i, err)
			}
		}
	}
	envCount := len(launch.Env)
	for _, plugin := range launch.Plugin {
		envCount += len(plugin.Env)
	}
	for _, init := range p.Init {
		envCount += len(init.Env)
	}
	if envCount > MaxPortableEnvEntries {
		return fmt.Errorf("portable environment exceeds %d entries", MaxPortableEnvEntries)
	}
	return nil
}

// Kept as a small seam so portable.go does not duplicate duration rules with
// config.go's public schema while retaining field-specific errors.
func timeParseDurationPositiveOrZero(raw string) (int64, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, errors.New("duration must not be negative")
	}
	return d.Milliseconds(), nil
}

// WritePortableSandboxConfig atomically creates runDir/sandbox.runtime.cfg.
// RENAME_NOREPLACE enforces C0's write-once lifecycle; callers cannot mutate a
// live baseline by invoking the helper again.
func WritePortableSandboxConfig(runDir string, cfg *PortableSandboxConfig) ([]byte, error) {
	raw, err := MarshalPortableSandboxConfig(cfg)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(runDir) {
		return nil, fmt.Errorf("portable run directory must be absolute: %q", runDir)
	}
	tmp, err := os.CreateTemp(runDir, ".sandbox.runtime.cfg.*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create portable config temporary: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("chmod portable config temporary: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return nil, fmt.Errorf("write portable config temporary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("sync portable config temporary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close portable config temporary: %w", err)
	}
	final := filepath.Join(runDir, SandboxRuntimeConfigName)
	if err := unix.Renameat2(unix.AT_FDCWD, tmpPath, unix.AT_FDCWD, final, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("portable C0 already exists at %s", final)
		}
		return nil, fmt.Errorf("commit portable C0: %w", err)
	}
	committed = true
	dir, err := os.Open(runDir)
	if err != nil {
		return nil, fmt.Errorf("open portable config directory: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return nil, errors.Join(syncErr, closeErr)
	}
	return raw, nil
}

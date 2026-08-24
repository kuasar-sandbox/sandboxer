package restore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"github.com/kuasar-sandbox/sandboxer/pkg/util"
	"gopkg.in/yaml.v3"
)

// SnapshotCfg mirrors the on-disk snapshot.cfg schema (docs/sandbox.md
// §3.4). Parsed from the trailing-ZIP entry "snapshot.cfg" inside a
// <sid>.snapshot bundle.
type SnapshotCfg struct {
	Resources struct {
		Capacity struct {
			CPU    int    `yaml:"cpu"`
			Memory string `yaml:"memory"`
		} `yaml:"capacity"`
	} `yaml:"resources"`
	// Metadata is the opaque platform passthrough (config.SandboxConfig
	// .Metadata): never interpreted by the runtime, carried verbatim
	// through snapshot/upload re-rendering, surfaced by info --json.
	Metadata map[string]string `yaml:"metadata,omitempty"`
	FromRefs []string          `yaml:"from_refs"` // memory chain below this bundle (§3.5)
	Launch   struct {
		// CgroupControl is guest topology, not host restore policy. It must
		// survive restore so subsequent snapshots retain the same contract.
		CgroupControl bool `yaml:"cgroup_control"`
	} `yaml:"launch"`
	Boot struct {
		RuntimeRef string `yaml:"runtime_ref"`
		Root       struct {
			// overlay mode: BaseRef is the erofs image; Overlay carries the
			// captured upper diff + its chain.
			BaseRef string          `yaml:"base_ref,omitempty"`
			Overlay *SnapOverlayCfg `yaml:"overlay,omitempty"`
			// single-disk mode (Overlay nil): the captured root diff is the
			// child's read-only Base; BaseFromRefs is the disk chain below it.
			Base         string   `yaml:"base,omitempty"`
			BaseFromRefs []string `yaml:"base_from_refs,omitempty"`
		} `yaml:"root"`
		// Disks are the data-disk nodes (boot.disks[] order), each the same shape
		// as Root. Ordered, no name (ordinal-keyed; the restore host yaml supplies
		// names + mount targets, merged by index).
		Disks []SnapDiskNode `yaml:"disks,omitempty"`
	} `yaml:"boot"`
}

// SnapDiskNode is one data disk's snapshot.cfg node (same shape as boot.root).
type SnapDiskNode struct {
	BaseRef      string          `yaml:"base_ref,omitempty"`
	Overlay      *SnapOverlayCfg `yaml:"overlay,omitempty"`
	Base         string          `yaml:"base,omitempty"`
	BaseFromRefs []string        `yaml:"base_from_refs,omitempty"`
}

// single reports whether this disk node was captured in single-disk mode.
func (n *SnapDiskNode) single() bool { return n.Overlay == nil }

// SnapOverlayCfg is the overlay-mode disk sub-node of a snapshot.cfg.
type SnapOverlayCfg struct {
	Base         string   `yaml:"base"`
	BaseFromRefs []string `yaml:"base_from_refs"` // disk chain below base (§3.5)
}

// SingleDisk reports whether the snapshot was taken in single-disk mode (no
// boot.root.overlay node).
func (c *SnapshotCfg) SingleDisk() bool { return c.Boot.Root.Overlay == nil }

// ArtifactRefs returns the non-empty disk artifact refs named by this root
// snapshot.cfg. It covers the root disk and every data disk, including their
// flattened base_from_refs chains. It deliberately excludes FromRefs (the
// flattened memory-layer chain) and Boot.RuntimeRef (a node-provided platform
// artifact). Callers own deduplication, ordering, and limits.
func (c *SnapshotCfg) ArtifactRefs() []string {
	if c == nil {
		return nil
	}
	refs := make([]string, 0)
	appendRef := func(raw string) {
		if raw != "" {
			refs = append(refs, raw)
		}
	}
	appendNode := func(baseRef, base string, baseFromRefs []string, overlay *SnapOverlayCfg) {
		appendRef(baseRef)
		appendRef(base)
		for _, raw := range baseFromRefs {
			appendRef(raw)
		}
		if overlay != nil {
			appendRef(overlay.Base)
			for _, raw := range overlay.BaseFromRefs {
				appendRef(raw)
			}
		}
	}
	appendNode(c.Boot.Root.BaseRef, c.Boot.Root.Base, c.Boot.Root.BaseFromRefs, c.Boot.Root.Overlay)
	for i := range c.Boot.Disks {
		node := &c.Boot.Disks[i]
		appendNode(node.BaseRef, node.Base, node.BaseFromRefs, node.Overlay)
	}
	return refs
}

// ParseSnapshotCfg parses the YAML body of a snapshot.cfg ZIP entry.
func ParseSnapshotCfg(body []byte) (*SnapshotCfg, error) {
	var cfg SnapshotCfg
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("snapshot.cfg: %w", err)
	}
	return &cfg, nil
}

// ApplyRules merges host sandbox.yaml fields against the snapshot.cfg
// per docs/sandbox.md §11.0. Host-provided file:// URLs in
// boot.runtime / boot.root.base must match the snapshot.cfg ref's
// scheme + basename, and the artifact's embedded digest marker must match the
// ref. Host-empty fields are auto-filled from snapshot.cfg (basename
// interpreted relative to the snapshot bundle's directory).
//
// Capacity must match exactly when host provides it. Network may be omitted;
// Run separately verifies that its presence matches the device topology in
// config.json. boot.root.overlay.base in host yaml is silently ignored (always
// taken from snapshot.cfg). boot.kernel / boot.cmdline and launch fields other
// than snapshot-owned launch.cgroup_control are silently ignored.
//
// snapshotPath is the local file path of the <sid>.snapshot bundle
// (used to resolve runtime/base file basenames). Pass empty when the
// bundle came from manifest:// — host yaml must then provide all
// runtime/base ref fields explicitly.
//
// Returns the merged config.SandboxConfig the lifecycle should run with.
func ApplyRules(host *config.SandboxConfig, snap *SnapshotCfg, snapshotPath string, locations config.RefLocations, codec tarstream.Codec, required bool) (*config.SandboxConfig, error) {
	if host == nil {
		return nil, errors.New("ApplyRules: host config is nil")
	}
	if snap == nil {
		return nil, errors.New("ApplyRules: snapshot.cfg is nil")
	}
	out := *host // shallow copy
	out.Launch.CgroupControl = snap.Launch.CgroupControl
	out.SnapshotRefs = config.SnapshotRefs{RuntimeRef: snap.Boot.RuntimeRef}
	if len(snap.Boot.Disks) > 0 {
		out.SnapshotRefs.DiskBaseRefs = make([]string, len(snap.Boot.Disks))
	}

	// 1. capacity: exact match if host provides; otherwise copy.
	if host.Resources.Capacity.CPU != 0 || host.Resources.Capacity.Memory != "" {
		if host.Resources.Capacity.CPU != snap.Resources.Capacity.CPU ||
			host.Resources.Capacity.Memory != snap.Resources.Capacity.Memory {
			return nil, fmt.Errorf("capacity mismatch with snapshot.cfg: host=%dvCPU/%s snap=%dvCPU/%s",
				host.Resources.Capacity.CPU, host.Resources.Capacity.Memory,
				snap.Resources.Capacity.CPU, snap.Resources.Capacity.Memory)
		}
	} else {
		out.Resources.Capacity.CPU = snap.Resources.Capacity.CPU
		out.Resources.Capacity.Memory = snap.Resources.Capacity.Memory
	}
	// 2. network: zero or one host source. Run later matches zero/one against
	// the virtio-net topology captured in config.json.
	if host.Network.TAP != "" && host.Network.TapFD != nil {
		return nil, errors.New("network: `tap` and `tapfd` are mutually exclusive in restore mode")
	}
	if err := host.Network.TapFD.Validate("network.tapfd"); err != nil {
		return nil, err
	}

	// 3. boot.runtime: file:// only (cold + restore alike).
	snapRuntimeRef, err := parseSnapshotRuntimeRef(snap.Boot.RuntimeRef)
	if err != nil {
		return nil, fmt.Errorf("snapshot.cfg.runtime_ref: %w", err)
	}
	if host.Boot.Runtime != "" {
		hostRuntimeRef, err := manifest.ParseRef(host.Boot.Runtime)
		if err != nil || hostRuntimeRef.Scheme != manifest.RefSchemeFile {
			return nil, fmt.Errorf("boot.runtime: must be file://")
		}
		if hostRuntimeRef.Location != "" {
			return nil, fmt.Errorf("boot.runtime: named ref locations are not supported")
		}
	}
	resolvedRuntime, err := resolveBootFileRef(host.Boot.Runtime, snapRuntimeRef, snapshotPath, "boot.runtime", readRuntimeBundleDigest, locations)
	if err != nil {
		return nil, err
	}
	out.Boot.Runtime = resolvedRuntime

	// 4. boot.root disk layout, by snapshot mode.
	if snap.SingleDisk() {
		// Single-disk: the captured root diff (snap.boot.root.base) is the new
		// read-only base; the chain below it is base_from_refs. No erofs image,
		// no overlay. Used raw via OpenDiskStream (restore.go), like overlay.base.
		out.Boot.Root.Overlay = nil
		out.Boot.Root.Base = snap.Boot.Root.Base
		out.Boot.Root.BaseFromRefs = snap.Boot.Root.BaseFromRefs
		// boot.root.diff stays as host yaml (optional override); empty → restore
		// auto-defaults a fresh diff sized to the base.
	} else {
		// Overlay: base_ref is the erofs image (file:// or manifest://); the
		// captured upper diff (overlay.base) is used raw via OpenDiskStream.
		snapBaseRef, err := manifest.ParseRef(snap.Boot.Root.BaseRef)
		if err != nil {
			return nil, protectArtifactReadError(codec, "snapshot.cfg.base_ref", err)
		}
		resolvedBase, err := resolveAnyRef(host.Boot.Root.Base, snapBaseRef, snapshotPath, "boot.root.base", locations, codec, required)
		if err != nil {
			return nil, err
		}
		out.Boot.Root.Base = resolvedBase
		out.SnapshotRefs.BaseRef, err = effectiveSnapshotBaseRef(resolvedBase, snapBaseRef)
		if err != nil {
			return nil, fmt.Errorf("boot.root.base: %w", err)
		}
		// Fresh Overlay pointer so we never mutate host's (out is a shallow copy);
		// inherit the host's optional diff override, force overlay.base from the
		// snapshot (host yaml's overlay.base is ignored).
		ov := config.OverlayConfig{}
		if host.Boot.Root.Overlay != nil {
			ov = *host.Boot.Root.Overlay
		}
		ov.Base = snap.Boot.Root.Overlay.Base
		ov.BaseFromRefs = append([]string(nil), snap.Boot.Root.Overlay.BaseFromRefs...)
		out.Boot.Root.Overlay = &ov
	}

	// 5. boot.disks[]: merge each host data disk with the snapshot's captured
	// disk node by ordinal (same rule as root). Count/order must match — the
	// restore host yaml describes the same disks the snapshot was taken with.
	if len(host.Boot.Disks) != len(snap.Boot.Disks) {
		return nil, fmt.Errorf("boot.disks count mismatch with snapshot.cfg: host=%d snap=%d", len(host.Boot.Disks), len(snap.Boot.Disks))
	}
	if len(snap.Boot.Disks) > 0 {
		out.Boot.Disks = make([]config.DiskConfig, len(host.Boot.Disks))
		for i := range host.Boot.Disks {
			hd := host.Boot.Disks[i] // copy: keeps Name + host diff/diff_template
			sd := &snap.Boot.Disks[i]
			field := fmt.Sprintf("boot.disks[%d]", i)
			if sd.single() {
				hd.Overlay = nil
				hd.Base = sd.Base
				hd.BaseFromRefs = sd.BaseFromRefs
			} else {
				snapBaseRef, err := manifest.ParseRef(sd.BaseRef)
				if err != nil {
					return nil, protectArtifactReadError(codec, "snapshot.cfg "+field+".base_ref", err)
				}
				resolvedBase, err := resolveAnyRef(hd.Base, snapBaseRef, snapshotPath, field+".base", locations, codec, required)
				if err != nil {
					return nil, err
				}
				hd.Base = resolvedBase
				out.SnapshotRefs.DiskBaseRefs[i], err = effectiveSnapshotBaseRef(resolvedBase, snapBaseRef)
				if err != nil {
					return nil, fmt.Errorf("%s.base: %w", field, err)
				}
				ov := config.OverlayConfig{}
				if hd.Overlay != nil {
					ov = *hd.Overlay
				}
				ov.Base = sd.Overlay.Base
				ov.BaseFromRefs = append([]string(nil), sd.Overlay.BaseFromRefs...)
				hd.Overlay = &ov
			}
			out.Boot.Disks[i] = hd
		}
	}

	// boot.kernel / boot.cmdline and launch fields other than cgroup_control are
	// ignored — they stay as host yaml provided, but lifecycle.go won't use them
	// on the restore path. launch.cgroup_control is snapshot-owned guest topology
	// and was copied above so a later snapshot preserves it.

	if err := validateResolvedRestoreMemoryPolicy(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// validateResolvedRestoreMemoryPolicy applies the Capacity-relative bounds only
// after snapshot.cfg has supplied an omitted host Capacity. Startup is checked
// as schema input but is never used to reserve or size a restored VM.
func validateResolvedRestoreMemoryPolicy(c *config.SandboxConfig) error {
	capacity, err := c.CapacityMemoryBytes()
	if err != nil {
		return fmt.Errorf("resources.capacity.memory: %w", err)
	}
	if capacity == 0 {
		return errors.New("resources.capacity.memory must be > 0")
	}
	allocatable, err := c.AllocatableMemoryBytes()
	if err != nil {
		return fmt.Errorf("resources.allocatable.memory: %w", err)
	}
	if allocatable == 0 {
		return errors.New("resources.allocatable.memory must be > 0")
	}
	if allocatable > capacity {
		return errors.New("resources.allocatable.memory must be ≤ capacity.memory")
	}
	if c.Resources.Startup == nil {
		return nil
	}
	startup, err := util.ParseSize(c.Resources.Startup.Memory)
	if err != nil {
		return fmt.Errorf("resources.startup.memory: %w", err)
	}
	if startup == 0 {
		return errors.New("resources.startup.memory must be > 0")
	}
	if startup > capacity {
		return fmt.Errorf("resources.startup.memory (%d) must be ≤ capacity.memory (%d)", startup, capacity)
	}
	return nil
}

// resolveBootFileRef enforces the file:// rules for boot.runtime /
// boot.root.base when the snapshot.cfg ref is file:// type.
//
//   - host empty: build absolute path = filepath.Join(<bundle dir>, basename).
//   - host non-empty: parse host URL → file path and verify
//     basename(path) == ref.Basename.
//   - the embedded marker must match ref.Digest.
func resolveBootFileRef(hostURL string, snapRef manifest.Ref, snapshotPath, fieldName string, readDigest func(string) (string, string, error), locations config.RefLocations) (string, error) {
	snapScheme, snapDigest, err := normalizedRefIdentity(snapRef, nil, false)
	if err != nil {
		return "", fmt.Errorf("%s: snapshot ref uses an unsupported digest scheme: %w", fieldName, err)
	}
	if hostURL == "" {
		if snapshotPath == "" && snapRef.Location == "" && !filepath.IsAbs(snapRef.Path) {
			return "", fmt.Errorf("%s: snapshot.cfg ref is file:// but bundle is manifest-loaded; provide %s explicitly", fieldName, fieldName)
		}
		bundleDir := ""
		if snapshotPath != "" {
			bundleDir = filepath.Dir(snapshotPath)
		}
		abs, err := locations.ResolveFile(snapRef, bundleDir)
		if err != nil {
			return "", fmt.Errorf("%s: %w", fieldName, err)
		}
		if err := validateResolvedFileRef(abs, snapScheme, snapDigest, readDigest); err != nil {
			return "", fmt.Errorf("%s: auto-resolved %s: %w", fieldName, abs, err)
		}
		if snapRef.Location != "" {
			return snapRef.String(), nil
		}
		return "file://" + abs, nil
	}

	hostRef, err := manifest.ParseRef(hostURL)
	if err != nil || hostRef.Scheme != manifest.RefSchemeFile {
		return "", fmt.Errorf("%s: scheme mismatch with snapshot.cfg (want file://)", fieldName)
	}
	hostScheme, hostDigest, err := normalizedRefIdentity(hostRef, nil, false)
	if err != nil {
		return "", fmt.Errorf("%s: host ref uses an unsupported digest scheme: %w", fieldName, err)
	}
	hostPath, err := locations.ResolveFile(hostRef, "")
	if err != nil {
		return "", fmt.Errorf("%s: %w", fieldName, err)
	}
	if !filepath.IsAbs(hostPath) {
		return "", fmt.Errorf("%s: file:// must be absolute or located (got %q)", fieldName, hostURL)
	}
	if filepath.Base(hostPath) != snapRef.Path {
		return "", fmt.Errorf("%s: basename mismatch with snapshot.cfg (host=%q, snap=%q)", fieldName, filepath.Base(hostPath), snapRef.Path)
	}
	if err := validateResolvedFileRef(hostPath, snapScheme, snapDigest, readDigest); err != nil {
		return "", fmt.Errorf("%s: digest mismatch: %w", fieldName, err)
	}
	if hostDigest != "" {
		if err := validateResolvedFileRef(hostPath, hostScheme, hostDigest, readDigest); err != nil {
			return "", fmt.Errorf("%s: host ref digest mismatch: %w", fieldName, err)
		}
	}
	if hostRef.Location != "" {
		return hostRef.String(), nil
	}
	return "file://" + hostPath, nil
}

// resolveAnyRef handles either file:// or manifest:// refs (used by
// boot.root.base which accepts both).
func resolveAnyRef(hostURL string, snapRef manifest.Ref, snapshotPath, fieldName string, locations config.RefLocations, codec tarstream.Codec, required bool) (string, error) {
	switch snapRef.Scheme {
	case "file":
		return resolveTarFileRef(hostURL, snapRef, snapshotPath, fieldName, locations, codec, required)
	case "manifest":
		if hostURL == "" {
			return snapRef.String(), nil
		}
		if !strings.HasPrefix(hostURL, "manifest://") {
			return "", fmt.Errorf("%s: scheme mismatch with snapshot.cfg (host=%q, snap=manifest://)", fieldName, hostURL)
		}
		hostKey := strings.TrimPrefix(hostURL, "manifest://")
		if hostKey != snapRef.Path {
			return "", fmt.Errorf("%s: manifest key mismatch with snapshot.cfg (host=%q, snap=%q)", fieldName, hostKey, snapRef.Path)
		}
		return hostURL, nil
	default:
		return "", fmt.Errorf("%s: unsupported scheme %q in snapshot.cfg", fieldName, snapRef.Scheme)
	}
}

func resolveTarFileRef(hostURL string, snapRef manifest.Ref, snapshotPath, fieldName string, locations config.RefLocations, codec tarstream.Codec, required bool) (string, error) {
	bundleDir := ""
	if snapshotPath != "" {
		bundleDir = filepath.Dir(snapshotPath)
	}
	if hostURL == "" {
		if snapshotPath == "" && snapRef.Location == "" && !filepath.IsAbs(snapRef.Path) {
			return "", fmt.Errorf("%s: snapshot.cfg ref is file:// but bundle is manifest-loaded; provide %s explicitly", fieldName, fieldName)
		}
		path, err := locations.ResolveFile(snapRef, bundleDir)
		if err != nil {
			return "", fmt.Errorf("%s: %w", fieldName, err)
		}
		scheme, digest, err := validateTarFileRef(path, snapRef, locations, codec, required)
		if err != nil {
			return "", fmt.Errorf("%s: auto-resolved file artifact: %w", fieldName, err)
		}
		resolved := snapRef
		resolved.DigestScheme, resolved.Digest = scheme, digest
		if resolved.Location == "" {
			resolved.Path = path
		}
		return resolved.String(), nil
	}

	hostRef, err := manifest.ParseRef(hostURL)
	if err != nil || hostRef.Scheme != manifest.RefSchemeFile {
		return "", fmt.Errorf("%s: scheme mismatch with snapshot.cfg (want file://)", fieldName)
	}
	hostPath, err := locations.ResolveFile(hostRef, "")
	if err != nil {
		return "", fmt.Errorf("%s: %w", fieldName, err)
	}
	if !filepath.IsAbs(hostPath) {
		return "", fmt.Errorf("%s: file:// must be absolute or located", fieldName)
	}
	if filepath.Base(hostPath) != filepath.Base(snapRef.Path) {
		return "", fmt.Errorf("%s: basename mismatch with snapshot.cfg", fieldName)
	}
	if hostRef.Digest != "" {
		if _, _, err := validateTarFileRef(hostPath, hostRef, locations, codec, required); err != nil {
			return "", fmt.Errorf("%s: host ref: %w", fieldName, err)
		}
	}
	snapshotIdentity := snapRef
	snapshotIdentity.Location = ""
	snapshotIdentity.Path = hostPath
	scheme, digest, err := validateTarFileRef(hostPath, snapshotIdentity, nil, codec, required)
	if err != nil {
		return "", fmt.Errorf("%s: snapshot ref: %w", fieldName, err)
	}
	resolved := hostRef
	resolved.DigestScheme, resolved.Digest = scheme, digest
	if resolved.Location == "" {
		resolved.Path = hostPath
	}
	return resolved.String(), nil
}

func validateTarFileRef(path string, ref manifest.Ref, locations config.RefLocations, codec tarstream.Codec, required bool) (string, string, error) {
	openRef := ref
	if openRef.Location == "" {
		openRef.Path = path
	}
	stream, _, err := sandbox.OpenDiskStream(context.Background(), openRef.String(), nil, locations, codec, required)
	if err != nil {
		return "", "", err
	}
	scheme, digest, err := sourceDigest(stream)
	closeErr := stream.Close()
	if err != nil {
		return "", "", err
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	return scheme, digest, nil
}

func validateResolvedFileRef(path, wantScheme, wantDigest string, readDigest func(string) (string, string, error)) error {
	gotScheme, gotDigest, err := readDigest(path)
	if err != nil {
		return err
	}
	if wantDigest == "" {
		return nil
	}
	return matchDigest(gotScheme, gotDigest, wantScheme, wantDigest)
}

func parseSnapshotRuntimeRef(raw string) (manifest.Ref, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return manifest.Ref{}, err
	}
	if ref.Scheme != manifest.RefSchemeFile {
		return manifest.Ref{}, fmt.Errorf("scheme %q unexpected (boot.runtime is file:// only)", ref.Scheme)
	}
	if ref.Location != "" {
		return manifest.Ref{}, fmt.Errorf("named ref locations are not supported")
	}
	if ref.DigestScheme != tarstream.DigestSchemeSHA256 || ref.Digest == "" {
		return manifest.Ref{}, fmt.Errorf("sha256 digest qualifier is required")
	}
	return ref, nil
}

func effectiveSnapshotBaseRef(raw string, snapRef manifest.Ref) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", err
	}
	if ref.Scheme != snapRef.Scheme {
		return "", fmt.Errorf("effective scheme %q does not match snapshot scheme %q", ref.Scheme, snapRef.Scheme)
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		return snapRef.String(), nil
	}
	ref.Path = filepath.Base(ref.Path)
	if ref.Digest == "" {
		return "", fmt.Errorf("effective file ref has no digest identity")
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	return ref.String(), nil
}

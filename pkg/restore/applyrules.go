package restore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/internal/tartransition"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
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
	Boot     struct {
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
// taken from snapshot.cfg). boot.kernel / boot.cmdline / launch.* are silently
// ignored.
//
// snapshotPath is the local file path of the <sid>.snapshot bundle
// (used to resolve runtime/base file basenames). Pass empty when the
// bundle came from manifest:// — host yaml must then provide all
// runtime/base ref fields explicitly.
//
// Returns the merged config.SandboxConfig the lifecycle should run with.
func ApplyRules(host *config.SandboxConfig, snap *SnapshotCfg, snapshotPath string, locations config.RefLocations) (*config.SandboxConfig, error) {
	if host == nil {
		return nil, errors.New("ApplyRules: host config is nil")
	}
	if snap == nil {
		return nil, errors.New("ApplyRules: snapshot.cfg is nil")
	}
	out := *host // shallow copy
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
			return nil, fmt.Errorf("snapshot.cfg.base_ref: %w", err)
		}
		resolvedBase, err := resolveAnyRef(host.Boot.Root.Base, snapBaseRef, snapshotPath, "boot.root.base", locations)
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
					return nil, fmt.Errorf("snapshot.cfg.%s.base_ref: %w", field, err)
				}
				resolvedBase, err := resolveAnyRef(hd.Base, snapBaseRef, snapshotPath, field+".base", locations)
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

	// boot.kernel / boot.cmdline / launch.* silently ignored — fields
	// stay as host yaml provided, but lifecycle.go won't use them on
	// the restore path.

	return &out, nil
}

// resolveBootFileRef enforces the file:// rules for boot.runtime /
// boot.root.base when the snapshot.cfg ref is file:// type.
//
//   - host empty: build absolute path = filepath.Join(<bundle dir>, basename).
//   - host non-empty: parse host URL → file path and verify
//     basename(path) == ref.Basename.
//   - the embedded marker must match ref.Digest.
func resolveBootFileRef(hostURL string, snapRef manifest.Ref, snapshotPath, fieldName string, readDigest func(string) (string, error), locations config.RefLocations) (string, error) {
	snapDigest, err := tartransition.SHA256RefDigest(snapRef)
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
		if err := validateResolvedFileRef(abs, snapRef, snapDigest, readDigest, locations); err != nil {
			return "", fmt.Errorf("%s: auto-resolved %s: %w", fieldName, abs, err)
		}
		if snapRef.Location != "" {
			return snapRef.String(), nil
		}
		return "file://" + abs, nil
	}

	hostRef, err := manifest.ParseRef(hostURL)
	if err != nil || hostRef.Scheme != manifest.RefSchemeFile {
		return "", fmt.Errorf("%s: scheme mismatch with snapshot.cfg (host=%q, snap=file://)", fieldName, hostURL)
	}
	hostDigest, err := tartransition.SHA256RefDigest(hostRef)
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
	if err := validateResolvedFileRef(hostPath, hostRef, snapDigest, readDigest, locations); err != nil {
		return "", fmt.Errorf("%s: digest mismatch: %w", fieldName, err)
	}
	if hostDigest != "" {
		if err := validateResolvedFileRef(hostPath, hostRef, hostDigest, readDigest, locations); err != nil {
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
func resolveAnyRef(hostURL string, snapRef manifest.Ref, snapshotPath, fieldName string, locations config.RefLocations) (string, error) {
	switch snapRef.Scheme {
	case "file":
		return resolveBootFileRef(hostURL, snapRef, snapshotPath, fieldName, readTarArtifactDigest, locations)
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

func validateResolvedFileRef(path string, ref manifest.Ref, want string, readDigest func(string) (string, error), locations config.RefLocations) error {
	var (
		got string
		err error
	)
	if ref.Location == "" {
		got, err = readDigest(path)
	} else {
		stream, _, openErr := sandbox.OpenDiskStream(context.Background(), ref.String(), nil, locations)
		if openErr != nil {
			return openErr
		}
		var ok bool
		got, ok, err = tartransition.Digest(stream)
		if err != nil {
			_ = stream.Close()
			return fmt.Errorf("file artifact %s has invalid declared digest: %w", path, err)
		}
		if !ok {
			_ = stream.Close()
			return fmt.Errorf("file artifact %s has no declared digest", path)
		}
		err = stream.Close()
	}
	if err != nil {
		return err
	}
	return matchDigest(got, want)
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
	digest, err := tartransition.SHA256RefDigest(snapRef)
	if err != nil {
		return "", err
	}
	ref.Path = filepath.Base(ref.Path)
	return tartransition.SHA256RefString(ref, digest)
}

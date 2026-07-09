package restore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// Ref is a parsed `file://<basename>@sha256:<digest>` or
// `manifest://<key>` reference.
type Ref struct {
	Scheme   string // "file" or "manifest"
	Basename string // file mode: filename only; manifest mode: empty
	Digest   string // file mode: sha256 hex (lowercase); manifest mode: empty
	Key      string // manifest mode: hex content key; file mode: empty
}

// FileRefPolicy controls how restore treats local file:// refs recorded in
// snapshot.cfg.
type FileRefPolicy string

const (
	FileRefPolicyVerify FileRefPolicy = "verify"
	FileRefPolicyTrust  FileRefPolicy = "trust"
)

// ApplyOptions tunes restore snapshot.cfg/host-yaml merge rules.
type ApplyOptions struct {
	FileRefs FileRefPolicy
}

// ParseFileRefPolicy parses the public verify|trust spelling. Empty means the
// default strict mode.
func ParseFileRefPolicy(s string) (FileRefPolicy, error) {
	switch FileRefPolicy(strings.TrimSpace(s)) {
	case "", FileRefPolicyVerify:
		return FileRefPolicyVerify, nil
	case FileRefPolicyTrust:
		return FileRefPolicyTrust, nil
	default:
		return "", fmt.Errorf("restore file refs %q (want verify|trust)", s)
	}
}

func (p FileRefPolicy) normalized() (FileRefPolicy, error) {
	return ParseFileRefPolicy(string(p))
}

// String reconstructs the canonical form. Inverse of ParseRef.
func (r Ref) String() string {
	switch r.Scheme {
	case "file":
		return "file://" + r.Basename + "@sha256:" + r.Digest
	case "manifest":
		return "manifest://" + r.Key
	default:
		return ""
	}
}

// ParseRef parses runtime_ref / base_ref strings.
func ParseRef(s string) (Ref, error) {
	switch {
	case strings.HasPrefix(s, "manifest://"):
		key := strings.TrimPrefix(s, "manifest://")
		if key == "" {
			return Ref{}, errors.New("manifest:// ref: empty key")
		}
		return Ref{Scheme: "manifest", Key: key}, nil
	case strings.HasPrefix(s, "file://"):
		body := strings.TrimPrefix(s, "file://")
		// Expect <basename>@sha256:<hex>
		idx := strings.LastIndex(body, "@sha256:")
		if idx < 0 {
			return Ref{}, fmt.Errorf("file:// ref %q: missing @sha256:<digest>", s)
		}
		base := body[:idx]
		dig := body[idx+len("@sha256:"):]
		if base == "" || dig == "" {
			return Ref{}, fmt.Errorf("file:// ref %q: empty basename or digest", s)
		}
		return Ref{Scheme: "file", Basename: base, Digest: dig}, nil
	default:
		return Ref{}, fmt.Errorf("ref %q: missing file:// or manifest:// scheme", s)
	}
}

// ApplyRules merges host sandbox.yaml fields against the snapshot.cfg
// per docs/sandbox.md §11.0. Host-provided file:// URLs in
// boot.runtime / boot.root.base must match the snapshot.cfg ref's
// scheme + basename. In verify mode, the local file content must also
// match the ref's SHA256. In trust mode, restore skips that content hash
// and only checks that the local file exists. Host-empty fields are
// auto-filled from snapshot.cfg (basename interpreted relative to the
// snapshot bundle's directory).
//
// Capacity must match exactly when host provides it. Network must be
// provided. boot.root.overlay.base in host yaml is silently ignored
// (always taken from snapshot.cfg). boot.kernel / boot.cmdline /
// launch.* are silently ignored.
//
// snapshotPath is the local file path of the <sid>.snapshot bundle
// (used to resolve runtime/base file basenames). Pass empty when the
// bundle came from manifest:// — host yaml must then provide all
// runtime/base ref fields explicitly.
//
// Returns the merged config.SandboxConfig the lifecycle should run with.
func ApplyRules(host *config.SandboxConfig, snap *SnapshotCfg, snapshotPath string, opts ApplyOptions) (*config.SandboxConfig, error) {
	if host == nil {
		return nil, errors.New("ApplyRules: host config is nil")
	}
	if snap == nil {
		return nil, errors.New("ApplyRules: snapshot.cfg is nil")
	}
	fileRefs, err := opts.FileRefs.normalized()
	if err != nil {
		return nil, err
	}
	out := *host // shallow copy

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

	// 2. network: exactly one source required (same rule as cold start).
	if (host.Network.TAP == "") == (host.Network.TapFD == nil) {
		return nil, errors.New("network: exactly one of `tap` or `tapfd` is required in restore mode")
	}
	if err := host.Network.TapFD.Validate("network.tapfd"); err != nil {
		return nil, err
	}

	// 3. boot.runtime: file:// only (cold + restore alike).
	snapRuntimeRef, err := ParseRef(snap.Boot.RuntimeRef)
	if err != nil {
		return nil, fmt.Errorf("snapshot.cfg.runtime_ref: %w", err)
	}
	if snapRuntimeRef.Scheme != "file" {
		return nil, fmt.Errorf("snapshot.cfg.runtime_ref: scheme %q unexpected (boot.runtime is file:// only)", snapRuntimeRef.Scheme)
	}
	resolvedRuntime, err := resolveBootFileRef(host.Boot.Runtime, snapRuntimeRef, snapshotPath, "boot.runtime", fileRefs)
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
		snapBaseRef, err := ParseRef(snap.Boot.Root.BaseRef)
		if err != nil {
			return nil, fmt.Errorf("snapshot.cfg.base_ref: %w", err)
		}
		resolvedBase, err := resolveAnyRef(host.Boot.Root.Base, snapBaseRef, snapshotPath, "boot.root.base", fileRefs)
		if err != nil {
			return nil, err
		}
		out.Boot.Root.Base = resolvedBase
		// Fresh Overlay pointer so we never mutate host's (out is a shallow copy);
		// inherit the host's optional diff override, force overlay.base from the
		// snapshot (host yaml's overlay.base is ignored).
		ov := config.OverlayConfig{}
		if host.Boot.Root.Overlay != nil {
			ov = *host.Boot.Root.Overlay
		}
		ov.Base = snap.Boot.Root.Overlay.Base
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
				snapBaseRef, err := ParseRef(sd.BaseRef)
				if err != nil {
					return nil, fmt.Errorf("snapshot.cfg.%s.base_ref: %w", field, err)
				}
				resolvedBase, err := resolveAnyRef(hd.Base, snapBaseRef, snapshotPath, field+".base", fileRefs)
				if err != nil {
					return nil, err
				}
				hd.Base = resolvedBase
				ov := config.OverlayConfig{}
				if hd.Overlay != nil {
					ov = *hd.Overlay
				}
				ov.Base = sd.Overlay.Base
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
//   - verify mode hashes the local file and matches ref.Digest; trust mode
//     only checks that the local file exists.
func resolveBootFileRef(hostURL string, snapRef Ref, snapshotPath, fieldName string, fileRefs FileRefPolicy) (string, error) {
	if hostURL == "" {
		if snapshotPath == "" {
			return "", fmt.Errorf("%s: snapshot.cfg ref is file:// but bundle is manifest-loaded; provide %s explicitly", fieldName, fieldName)
		}
		bundleDir := filepath.Dir(snapshotPath)
		abs := filepath.Join(bundleDir, snapRef.Basename)
		if err := validateFileRef(abs, snapRef.Digest, fileRefs); err != nil {
			return "", fmt.Errorf("%s: auto-resolved %s: %w", fieldName, abs, err)
		}
		return "file://" + abs, nil
	}

	if !strings.HasPrefix(hostURL, "file://") {
		return "", fmt.Errorf("%s: scheme mismatch with snapshot.cfg (host=%q, snap=file://)", fieldName, hostURL)
	}
	hostPath := strings.TrimPrefix(hostURL, "file://")
	if !filepath.IsAbs(hostPath) {
		return "", fmt.Errorf("%s: file:// must be absolute (got %q)", fieldName, hostURL)
	}
	if filepath.Base(hostPath) != snapRef.Basename {
		return "", fmt.Errorf("%s: basename mismatch with snapshot.cfg (host=%q, snap=%q)", fieldName, filepath.Base(hostPath), snapRef.Basename)
	}
	if err := validateFileRef(hostPath, snapRef.Digest, fileRefs); err != nil {
		if fileRefs != FileRefPolicyVerify {
			return "", fmt.Errorf("%s: file ref: %w", fieldName, err)
		}
		return "", fmt.Errorf("%s: digest mismatch: %w", fieldName, err)
	}
	return hostURL, nil
}

// resolveAnyRef handles either file:// or manifest:// refs (used by
// boot.root.base which accepts both).
func resolveAnyRef(hostURL string, snapRef Ref, snapshotPath, fieldName string, fileRefs FileRefPolicy) (string, error) {
	switch snapRef.Scheme {
	case "file":
		return resolveBootFileRef(hostURL, snapRef, snapshotPath, fieldName, fileRefs)
	case "manifest":
		if hostURL == "" {
			return snapRef.String(), nil
		}
		if !strings.HasPrefix(hostURL, "manifest://") {
			return "", fmt.Errorf("%s: scheme mismatch with snapshot.cfg (host=%q, snap=manifest://)", fieldName, hostURL)
		}
		hostKey := strings.TrimPrefix(hostURL, "manifest://")
		if hostKey != snapRef.Key {
			return "", fmt.Errorf("%s: manifest key mismatch with snapshot.cfg (host=%q, snap=%q)", fieldName, hostKey, snapRef.Key)
		}
		return hostURL, nil
	default:
		return "", fmt.Errorf("%s: unsupported scheme %q in snapshot.cfg", fieldName, snapRef.Scheme)
	}
}

func validateFileRef(path, want string, policy FileRefPolicy) error {
	if policy == FileRefPolicyVerify {
		return verifyFileDigest(path, want)
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	return nil
}

// verifyFileDigest streams the file at path, computes SHA256 (sparse
// holes read as 0), and returns nil iff hex(hash) == want.
func verifyFileDigest(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("sha256 mismatch (got %s, want %s)", got, want)
	}
	return nil
}

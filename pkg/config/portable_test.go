package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testSHA  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testSHA2 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func validPortableConfig() *PortableSandboxConfig {
	return &PortableSandboxConfig{
		Version: PortableSandboxConfigVersion,
		Resources: PortableResourcesConfig{
			Capacity:    CapacityConfig{CPU: 2, Memory: "1GiB"},
			Allocatable: AllocatableConfig{CPU: 1.5, Memory: "768MiB"},
		},
		Boot: PortableBootConfig{
			Kernel:  "file://vmlinux@sha256:" + testSHA,
			Runtime: "file://sandbox-runtime.bundle@sha256:" + testSHA2,
			Root:    PortableRootConfig{Base: "self"},
		},
		Launch: PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
	}
}

func TestPortableMarshalDeterministicAndStrictRoundTrip(t *testing.T) {
	cfg := validPortableConfig()
	cfg.Launch.Env = map[string]string{"Z": "last", "A": "first"}
	cfg.Metadata = map[string]string{"z.example": "last", "a.example": "first"}
	one, err := MarshalPortableSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	two, err := MarshalPortableSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) {
		t.Fatalf("canonical bytes changed:\n%s\n---\n%s", one, two)
	}
	parsed, err := ParsePortableSandboxConfig(one)
	if err != nil {
		t.Fatal(err)
	}
	three, err := MarshalPortableSandboxConfig(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, three) {
		t.Fatalf("round-trip is not canonical:\n%s\n---\n%s", one, three)
	}
}

func TestPortableParserRejectsUnknownDuplicateVersionAndEphemeral(t *testing.T) {
	raw, err := MarshalPortableSandboxConfig(validPortableConfig())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		raw     []byte
		wantErr string
	}{
		{name: "unknown", raw: append(append([]byte(nil), raw...), []byte("unknown: true\n")...), wantErr: "field unknown not found"},
		{name: "ephemeral files", raw: append(append([]byte(nil), raw...), []byte("ephemeral_files: []\n")...), wantErr: "field ephemeral_files not found"},
		{name: "duplicate", raw: append([]byte("version: 1\n"), raw...), wantErr: "duplicate field \"version\""},
		{name: "version", raw: bytes.Replace(raw, []byte("version: 1"), []byte("version: 9"), 1), wantErr: "unsupported version 9"},
		{name: "multiple documents", raw: append(append([]byte(nil), raw...), []byte("---\nversion: 1\n")...), wantErr: "multiple YAML documents"},
		{name: "alias", raw: []byte("version: &v 1\nresources: *v\n"), wantErr: "aliases are not allowed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePortableSandboxConfig(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParsePortableSandboxConfig error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestPortableSelfExactlyOnceAndOnlyAtRootTop(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*PortableSandboxConfig)
		wantErr string
	}{
		{name: "missing", mutate: func(c *PortableSandboxConfig) { c.Boot.Root.Base = "file://root.overlay@sha256:" + testSHA }, wantErr: "exactly once"},
		{name: "twice", mutate: func(c *PortableSandboxConfig) {
			c.Boot.Root.Overlay = &PortableOverlayConfig{Base: "self"}
		}, wantErr: "exactly once"},
		{name: "root chain", mutate: func(c *PortableSandboxConfig) {
			c.Boot.Root.BaseFromRefs = []string{"self"}
		}, wantErr: "base_from_refs[0] cannot be self"},
		{name: "data top", mutate: func(c *PortableSandboxConfig) {
			c.Boot.Disks = []PortableDiskConfig{{Name: "data", PortableRootConfig: PortableRootConfig{Base: "self"}}}
			c.Mounts = []MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
		}, wantErr: "boot.disks[0].base cannot be self"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPortableConfig()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestPortableLayerRefLimitsAndDuplicates(t *testing.T) {
	ref := "file://lower.overlay@sha256:" + testSHA
	cfg := validPortableConfig()
	cfg.Boot.Root.BaseFromRefs = []string{ref, ref}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("duplicate layer error = %v", err)
	}

	cfg = validPortableConfig()
	cfg.Boot.Root.BaseFromRefs = make([]string, MaxPortableLayerRefs+1)
	for i := range cfg.Boot.Root.BaseFromRefs {
		cfg.Boot.Root.BaseFromRefs[i] = ref
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds 64 entries") {
		t.Fatalf("layer limit error = %v", err)
	}
}

func TestProjectPortableColdExcludesHostAndEphemeralFields(t *testing.T) {
	cfg := &SandboxConfig{
		Resources: ResourcesConfig{
			Capacity:    CapacityConfig{CPU: 2, Memory: "1GiB"},
			Allocatable: AllocatableConfig{CPU: 1, Memory: "512MiB"},
			Control:     ControlConfig{CgroupPath: "/sys/fs/cgroup/task", Controller: "/run/controller.sock"},
			Overhead:    &OverheadConfig{Memory: "64MiB"},
		},
		Network: NetworkConfig{TAP: "tap0", IP: "192.0.2.2/24", MAC: "02:00:00:00:00:01", Hostname: "instance", Interface: "eth0"},
		Boot: BootConfig{
			Kernel:  "file:///node/vmlinux",
			Runtime: "file:///node/sandbox-runtime.bundle",
			Root: RootConfig{
				Base: "file:///images/root.erofs@sha256:" + testSHA,
				Overlay: &OverlayConfig{
					Base:         "file:///layers/old.overlay@sha256:" + testSHA2,
					BaseFromRefs: []string{"manifest://" + testSHA},
					Diff:         "file:///var/lib/sandbox/active.diff",
					DiffTemplate: "file:///node/upper.ext4",
				},
			},
		},
		Launch: LaunchConfig{
			Exec: "/bin/app", Env: map[string]string{"PERSISTENT": "yes"},
			EphemeralEnv: map[string]string{"SECRET": "current"}, Workdir: "/", Restart: "never",
			StartTimeout: "30s",
		},
		Files:          []FileConfig{{Path: "/etc/persistent", Content: "saved"}},
		EphemeralFiles: []FileConfig{{Path: "/etc/secret", Content: "not-saved"}},
		Metadata:       map[string]string{"tenant": "portable"},
		Timeouts:       TimeoutsConfig{Restore: "2m"},
		Restore:        RestoreConfig{Prefetch: "memory"},
	}
	p, err := ProjectPortableCold(cfg, PortableProjection{
		KernelRef:  "file://vmlinux@sha256:" + testSHA,
		RuntimeRef: "file://sandbox-runtime.bundle@sha256:" + testSHA2,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalPortableSandboxConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{
		"/node/", "/var/lib/", "active.diff", "diff_template", "cgroup_path", "controller.sock",
		"192.0.2.2", "02:00:00", "instance", "prefetch", "start_timeout", "SECRET", "not-saved",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("portable config leaked %q:\n%s", forbidden, text)
		}
	}
	if p.Boot.Root.Overlay == nil || p.Boot.Root.Overlay.Base != "self" {
		t.Fatalf("root self = %#v", p.Boot.Root.Overlay)
	}
	wantOld := "file://old.overlay@sha256:" + testSHA2
	if len(p.Boot.Root.Overlay.BaseFromRefs) != 2 || p.Boot.Root.Overlay.BaseFromRefs[0] != wantOld {
		t.Fatalf("root upper chain = %v", p.Boot.Root.Overlay.BaseFromRefs)
	}
}

func TestPortableExportedSeparatesRootSelfAndDataRefs(t *testing.T) {
	cfg := validPortableConfig()
	cfg.Boot.Root = PortableRootConfig{
		Base: "file://root.erofs@sha256:" + testSHA,
		Overlay: &PortableOverlayConfig{
			Base:         "self",
			BaseFromRefs: []string{"file://old.overlay@sha256:" + testSHA2},
		},
	}
	cfg.Boot.Disks = []PortableDiskConfig{{
		Name: "data",
		PortableRootConfig: PortableRootConfig{
			Base:         "file://data-old.overlay@sha256:" + testSHA,
			BaseFromRefs: []string{"manifest://" + testSHA2},
		},
	}}
	cfg.Mounts = []MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	exported, err := cfg.Exported(
		"file://parent.sandbox@sha256:"+testSHA,
		[]string{"file://new-data.overlay@sha256:" + testSHA2},
		[]bool{false, false},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootChain := exported.Boot.Root.Overlay.BaseFromRefs
	if len(rootChain) != 2 || rootChain[0] != "file://parent.sandbox@sha256:"+testSHA {
		t.Fatalf("root chain = %v", rootChain)
	}
	data := exported.Boot.Disks[0].PortableRootConfig
	if data.Base != "file://new-data.overlay@sha256:"+testSHA2 {
		t.Fatalf("data top = %q", data.Base)
	}
	if len(data.BaseFromRefs) != 2 || data.BaseFromRefs[0] != "file://data-old.overlay@sha256:"+testSHA {
		t.Fatalf("data chain = %v", data.BaseFromRefs)
	}
}

func TestWritePortableSandboxConfigIsAtomicWriteOnce0600(t *testing.T) {
	runDir := t.TempDir()
	raw, err := WritePortableSandboxConfig(runDir, validPortableConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runDir, SandboxRuntimeConfigName)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("run-dir C0 bytes differ from canonical bytes")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("C0 mode = %o, want 600", info.Mode().Perm())
	}
	changed := validPortableConfig()
	changed.Metadata = map[string]string{"changed": "true"}
	if _, err := WritePortableSandboxConfig(runDir, changed); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second C0 write error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, raw) {
		t.Fatal("second write changed C0 bytes")
	}
}

func TestEffectivePersistentAndEphemeralOverrides(t *testing.T) {
	cfg := &SandboxConfig{
		Files: []FileConfig{
			{Path: "/a", Content: "persistent-a"},
			{Path: "/b", Content: "persistent-b"},
		},
		EphemeralFiles: []FileConfig{
			{Path: "/b", Content: "ephemeral-b"},
			{Path: "/c", Content: "ephemeral-c"},
		},
		Launch: LaunchConfig{
			Env:          map[string]string{"A": "persistent", "B": "persistent"},
			EphemeralEnv: map[string]string{"B": "ephemeral", "C": "ephemeral"},
		},
	}
	files := cfg.EffectiveFiles()
	if len(files) != 3 || files[0].Path != "/a" || files[1].Content != "ephemeral-b" || files[2].Path != "/c" {
		t.Fatalf("effective files = %#v", files)
	}
	env := cfg.EffectiveLaunchEnv()
	if env["A"] != "persistent" || env["B"] != "ephemeral" || env["C"] != "ephemeral" {
		t.Fatalf("effective env = %#v", env)
	}
	if cfg.Files[1].Content != "persistent-b" || cfg.Launch.Env["B"] != "persistent" {
		t.Fatal("effective merge mutated persistent input")
	}
}

package config

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDiffCOWDefaultsValidationAndMerge(t *testing.T) {
	for _, tc := range []struct {
		input        string
		total, dirty uint64
		invalid      bool
	}{
		{"{}", 32 << 20, 16 << 20, false},
		{"resources: {diff_cow: {cache_size: 64MiB}}", 64 << 20, 16 << 20, false},
		{"resources: {diff_cow: {max_dirty_size: 4KiB}}", 32 << 20, 4096, false},
		{"resources: {diff_cow: {cache_size: 4KiB, max_dirty_size: 4KiB}}", 4096, 4096, false},
		{"resources: {diff_cow: {cache_size: 4KiB}}", 0, 0, true},
		{"resources: {diff_cow: {max_dirty_size: 64MiB}}", 0, 0, true},
		{"resources: {diff_cow: {cache_size: 0}}", 0, 0, true},
		{"resources: {diff_cow: {max_dirty_size: 1KiB}}", 0, 0, true},
		{"resources: {diff_cow: {cache_size: ''}}", 0, 0, true},
		{"resources: {diff_cow: {max_dirty_size: null}}", 0, 0, true},
		{"resources: {diff_cow: {cache_size: -4096}}", 0, 0, true},
		{"resources: {diff_cow: {dirty_size: 4KiB}}", 0, 0, true},
		{"resources: {diff_cow: {cache_size: 8MiB, cache_size: 16MiB}}", 0, 0, true},
		{"resources: {diff_cow: {cache_size: 18446744073709551616}}", 0, 0, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			cfg, _, err := LoadConfigBytesWithPresence([]byte(tc.input))
			var total, dirty uint64
			if err == nil {
				total, dirty, err = cfg.Resources.DiffCOW.Bytes()
			}
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid cache policy accepted")
				}
				return
			}
			if err != nil || total != tc.total || dirty != tc.dirty {
				t.Fatalf("got %d/%d %v", total, dirty, err)
			}
		})
	}
	cfg, _, err := LoadMergedWithPresence([]string{writeYAML(t, "resources: {diff_cow: {cache_size: 8MiB, max_dirty_size: 4MiB}}"), writeYAML(t, "resources: {diff_cow: {max_dirty_size: 2MiB}}")})
	if err != nil {
		t.Fatal(err)
	}
	total, dirty, err := cfg.Resources.DiffCOW.Bytes()
	if err != nil || total != 8<<20 || dirty != 2<<20 {
		t.Fatalf("merged %d/%d %v", total, dirty, err)
	}
	for _, validate := range []func() error{cfg.ValidateCold, cfg.ValidateRestoreHostConfig} {
		cfg.Resources.DiffCOW.MaxDirtySize = "12MiB"
		if err := validate(); err == nil || !strings.Contains(err.Error(), "diff_cow") {
			t.Fatalf("missing cache validation: %v", err)
		}
	}
}
func TestDiffCOWHostOwnershipAndPortableBoundary(t *testing.T) {
	artifact := validPortableConfig()
	host := restoreHostConfig()
	host.Resources.DiffCOW = DiffCOWConfig{CacheSize: "8MiB", MaxDirtySize: "4MiB"}
	runtime, c0, err := ApplyRestoreRules(artifact, host, FieldPresence{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Resources.DiffCOW != host.Resources.DiffCOW {
		t.Fatal("restore lost current host cache")
	}
	raw, err := MarshalPortableSandboxConfig(c0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("diff_cow")) {
		t.Fatal("portable leaked cache")
	}
	host.Network.TAP = "tap0"
	host.Boot.Root = RootConfig{Overlay: &OverlayConfig{DiffTemplate: "file:///node/root.ext4"}}
	runtime, c0, err = ApplyFromRules(portableFromFixture(), host, FieldPresence{}, ApplyFromOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Resources.DiffCOW != host.Resources.DiffCOW {
		t.Fatal("run --from lost host cache")
	}
	runtime.Resources.DiffCOW.CacheSize = "16MiB"
	if host.Resources.DiffCOW.CacheSize != "8MiB" {
		t.Fatal("host cache aliased")
	}
	raw, err = MarshalPortableSandboxConfig(c0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("diff_cow")) {
		t.Fatal("run-from C0 leaked cache")
	}
	host.Resources.Startup = &StartupConfig{Memory: "256MiB"}
	host.Boot.Root = RootConfig{Overlay: &OverlayConfig{}}
	raw, err = yaml.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("cache_size: 8MiB")) {
		t.Fatal("host projection dropped cache")
	}
	var decoded SandboxConfig
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Resources.DiffCOW != host.Resources.DiffCOW {
		t.Fatal("host projection roundtrip lost cache")
	}
}

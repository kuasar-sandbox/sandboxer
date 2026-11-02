package config

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDiffCOWDefaultsAndYAMLValidation(t *testing.T) {
	for _, tc := range []struct {
		input        string
		total, dirty uint64
		invalid      bool
	}{
		{"{}", 32 << 20, 16 << 20, false},
		{"resources: {diff_cow: {cache_size: 64MiB}}", 64 << 20, 32 << 20, false},
		{"resources: {diff_cow: {cache_size: 128MiB}}", 128 << 20, 64 << 20, false},
		{"resources: {diff_cow: {cache_size: 4KiB}}", 4096, 4096, false},
		{"resources: {diff_cow: {cache_size: 12KiB}}", 12 << 10, 4096, false},
		{"resources: {diff_cow: {max_dirty_size: 4KiB}}", 32 << 20, 4096, false},
		{"resources: {diff_cow: {cache_size: 64MiB, max_dirty_size: 16MiB}}", 64 << 20, 16 << 20, false},
		{"resources: {diff_cow: {cache_size: 64MiB, max_dirty_size: 48MiB}}", 64 << 20, 48 << 20, false},
		{"resources: {diff_cow: {cache_size: 4KiB, max_dirty_size: 4KiB}}", 4096, 4096, false},
		{"resources: {diff_cow: {max_dirty_size: 64MiB}}", 0, 0, true},
		{"resources: {diff_cow: {cache_size: 0}}", 0, 0, true},
		{"resources: {diff_cow: {max_dirty_size: 1KiB}}", 0, 0, true},
		{"resources: {diff_cow: {cache_size: ''}}", 0, 0, true},
		{"resources: {diff_cow: {max_dirty_size: null}}", 0, 0, true},
		{"resources: {diff_cow: {cache_size: -4096}}", 0, 0, true},
		{"resources: {diff_cow: {dirty_size: 4KiB}}", 0, 0, true},
		{"resources: {diff_cow: {cache_size: 8MiB, cache_size: 16MiB}}", 0, 0, true},
		{"resources: {diff_cow: {cache_size: 18446744073709551616}}", 0, 0, true},
		{fmt.Sprintf("resources: {diff_cow: {cache_size: %d}}", uint64(^uint(0)>>1)+1), 0, 0, true},
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
			before := cfg.Resources.DiffCOW
			resolved, err := before.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if resolved.CacheSize == "" || resolved.MaxDirtySize == "" {
				t.Fatalf("unresolved fields: %+v", resolved)
			}
			total, dirty, err = resolved.Bytes()
			if err != nil || total != tc.total || dirty != tc.dirty {
				t.Fatalf("resolved %d/%d %v", total, dirty, err)
			}
			if cfg.Resources.DiffCOW != before {
				t.Fatal("resolution changed source config")
			}
			if before.MaxDirtySize != "" && resolved.MaxDirtySize != before.MaxDirtySize {
				t.Fatal("resolution changed explicit dirty size")
			}
		})
	}
}

func TestDiffCOWValidateExplicitAndResolve(t *testing.T) {
	for _, tc := range []struct {
		name            string
		cfg             DiffCOWConfig
		invalidExplicit bool
		invalidResolved bool
	}{
		{"omitted", DiffCOWConfig{}, false, false},
		{"dirty-only patch", DiffCOWConfig{MaxDirtySize: "64MiB"}, false, true},
		{"complete patch", DiffCOWConfig{CacheSize: "128MiB", MaxDirtySize: "64MiB"}, false, false},
		{"invalid pair", DiffCOWConfig{CacheSize: "32MiB", MaxDirtySize: "64MiB"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if err := cfg.ValidateExplicit(); (err != nil) != tc.invalidExplicit {
				t.Fatalf("ValidateExplicit() = %v", err)
			}
			resolved, err := cfg.Resolve()
			if (err != nil) != tc.invalidResolved {
				t.Fatalf("Resolve() = %+v, %v", resolved, err)
			}
			if _, _, err := cfg.Bytes(); (err != nil) != tc.invalidResolved {
				t.Fatalf("Bytes() = %v", err)
			}
			if cfg != tc.cfg {
				t.Fatalf("helpers changed source: %+v, want %+v", cfg, tc.cfg)
			}
			if !tc.invalidResolved {
				if err := resolved.ValidateExplicit(); err != nil {
					t.Fatalf("resolved config is invalid: %v", err)
				}
				if tc.cfg.CacheSize != "" && resolved.CacheSize != tc.cfg.CacheSize ||
					tc.cfg.MaxDirtySize != "" && resolved.MaxDirtySize != tc.cfg.MaxDirtySize {
					t.Fatalf("resolution changed explicit fields: %+v", resolved)
				}
			}
		})
	}
	for _, value := range []string{"0", "-4096", "1KiB", "4.001KiB", " ", "18446744073709551616", fmt.Sprint(uint64(^uint(0)>>1) + 1)} {
		for _, cfg := range []DiffCOWConfig{{CacheSize: value}, {MaxDirtySize: value}} {
			t.Run(fmt.Sprintf("invalid/%+v", cfg), func(t *testing.T) {
				before := cfg
				if err := cfg.ValidateExplicit(); err == nil {
					t.Fatal("ValidateExplicit accepted an invalid size")
				}
				if _, err := cfg.Resolve(); err == nil {
					t.Fatal("Resolve accepted an invalid size")
				}
				if _, _, err := cfg.Bytes(); err == nil {
					t.Fatal("Bytes accepted an invalid size")
				}
				if cfg != before {
					t.Fatal("validation changed source config")
				}
			})
		}
	}
}

func TestDiffCOWApplyDefaultsBeforeCacheOverride(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			input, wantSourceDirty := "{}", ""
			if explicit {
				input = "resources: {diff_cow: {max_dirty_size: 16MiB}}"
				wantSourceDirty = "16MiB"
			}
			cfg, err := LoadConfigBytes([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			for _, cacheMiB := range []uint64{32, 64, 128, 8} {
				cfg.ApplyDefaults()
				if cfg.Resources.DiffCOW.MaxDirtySize != wantSourceDirty {
					t.Fatalf("defaults changed dirty size: %+v", cfg.Resources.DiffCOW)
				}
				cfg.Resources.DiffCOW.CacheSize = fmt.Sprintf("%dMiB", cacheMiB)
				before := cfg.Resources.DiffCOW
				total, dirty, err := cfg.Resources.DiffCOW.Bytes()
				_, resolveErr := cfg.Resources.DiffCOW.Resolve()
				if explicit && cacheMiB == 8 {
					if err == nil || resolveErr == nil {
						t.Fatal("cache override silently clamped explicit dirty size")
					}
				} else {
					wantDirty := cacheMiB << 19
					if explicit {
						wantDirty = 16 << 20
					}
					if err != nil || resolveErr != nil || total != cacheMiB<<20 || dirty != wantDirty {
						t.Fatalf("cache %dMiB: got %d/%d, %v, %v", cacheMiB, total, dirty, err, resolveErr)
					}
				}
				if cfg.Resources.DiffCOW != before {
					t.Fatal("resolution changed source before next override")
				}
			}
		})
	}
}

func TestDiffCOWMergedFinalPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		layers       []string
		total, dirty uint64
		sourceDirty  string
		invalid      bool
	}{
		{"cache override", []string{"cache_size: 32MiB", "cache_size: 64MiB"}, 64 << 20, 32 << 20, "", false},
		{"larger cache override", []string{"cache_size: 64MiB", "cache_size: 128MiB"}, 128 << 20, 64 << 20, "", false},
		{"explicit dirty retained", []string{"cache_size: 32MiB, max_dirty_size: 16MiB", "cache_size: 64MiB"}, 64 << 20, 16 << 20, "16MiB", false},
		{"explicit dirty override", []string{"cache_size: 8MiB, max_dirty_size: 4MiB", "max_dirty_size: 2MiB"}, 8 << 20, 2 << 20, "2MiB", false},
		{"dirty before cache", []string{"max_dirty_size: 64MiB", "cache_size: 128MiB"}, 128 << 20, 64 << 20, "64MiB", false},
		{"invalid initial pair repaired", []string{"cache_size: 32MiB, max_dirty_size: 64MiB", "cache_size: 128MiB"}, 128 << 20, 64 << 20, "64MiB", false},
		{"invalid intermediate pair repaired", []string{"cache_size: 128MiB, max_dirty_size: 64MiB", "cache_size: 32MiB", "max_dirty_size: 16MiB"}, 32 << 20, 16 << 20, "16MiB", false},
		{"invalid final pair", []string{"cache_size: 64MiB, max_dirty_size: 48MiB", "cache_size: 32MiB"}, 0, 0, "48MiB", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := []string{writeYAML(t, minimalCold)}
			for _, layer := range tc.layers {
				paths = append(paths, writeYAML(t, "resources: {diff_cow: {"+layer+"}}"))
			}
			for name, load := range map[string]func([]string) (*SandboxConfig, error){
				"merged": LoadMerged,
				"presence": func(paths []string) (*SandboxConfig, error) {
					cfg, _, err := LoadMergedWithPresence(paths)
					return cfg, err
				},
			} {
				t.Run(name, func(t *testing.T) {
					cfg, err := load(paths)
					if err != nil {
						t.Fatalf("per-file validation rejected merge: %v", err)
					}
					if cfg.Resources.DiffCOW.MaxDirtySize != tc.sourceDirty {
						t.Fatalf("merge changed dirty presence/value: %+v", cfg.Resources.DiffCOW)
					}
					total, dirty, err := cfg.Resources.DiffCOW.Bytes()
					if (err != nil) != tc.invalid || total != tc.total || dirty != tc.dirty {
						t.Fatalf("merged %d/%d %v", total, dirty, err)
					}
					if err := cfg.ValidateCold(); (err != nil) != tc.invalid {
						t.Fatalf("final cold validation: %v", err)
					}
					if tc.invalid {
						for _, validate := range []func() error{cfg.ValidateCold, cfg.ValidateRestoreHostConfig} {
							if err := validate(); err == nil || !strings.Contains(err.Error(), "diff_cow") {
								t.Fatalf("missing cache validation: %v", err)
							}
						}
					}
				})
			}
		})
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

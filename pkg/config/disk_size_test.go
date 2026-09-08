package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRemovedDiskSizeKeysRejectedByAllByteLoaders(t *testing.T) {
	shapes := []struct {
		path, style, yaml string
	}{
		{"boot.root", "flow", "boot: {root: {%s: %s}}"},
		{"boot.root.overlay", "flow", "boot: {root: {overlay: {%s: %s}}}"},
		{"boot.disks[0]", "flow", "boot: {disks: [{name: data, %s: %s}]}"},
		{"boot.disks[0].overlay", "flow", "boot: {disks: [{name: data, overlay: {%s: %s}}]}"},
		{"boot.root", "block", "boot:\n  root:\n    %s: %s\n"},
		{"boot.root.overlay", "block", "boot:\n  root:\n    overlay:\n      %s: %s\n"},
		{"boot.disks[0]", "block", "boot:\n  disks:\n    - name: data\n      %s: %s\n"},
		{"boot.disks[0].overlay", "block", "boot:\n  disks:\n    - name: data\n      overlay:\n        %s: %s\n"},
	}
	loaders := map[string]func([]byte) error{
		"bytes": func(raw []byte) error {
			_, err := LoadConfigBytes(raw)
			return err
		},
		"presence": func(raw []byte) error {
			_, _, err := LoadConfigBytesWithPresence(raw)
			return err
		},
		"yaml": func(raw []byte) error {
			var cfg SandboxConfig
			return yaml.Unmarshal(raw, &cfg)
		},
	}
	for _, shape := range shapes {
		for _, key := range []string{"diff_size", "size"} {
			for _, value := range []string{"512MiB", `""`, "null", "0", "{}"} {
				for name, load := range loaders {
					t.Run(shape.path+"/"+shape.style+"/"+key+"/"+value+"/"+name, func(t *testing.T) {
						err := load([]byte(fmt.Sprintf(shape.yaml, key, value)))
						want := shape.path + "." + key + " is not supported"
						if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "required logical capacity") {
							t.Fatalf("error = %v, want %q and migration guidance", err, want)
						}
					})
				}
			}
		}
	}
}

func TestRemovedDiskSizeKeysThroughYAMLAliasesAndMerges(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		path string
	}{
		{"root-alias", "disk: &disk {diff_size: null}\nboot: {root: *disk}", "boot.root.diff_size"},
		{"disk-alias", "disk: &disk {size: 1GiB}\nboot: {disks: [*disk]}", "boot.disks[0].size"},
		{"overlay-alias", "upper: &upper {diff_size: 1GiB}\nboot: {root: {overlay: *upper}}", "boot.root.overlay.diff_size"},
		{"root-merge", "disk: &disk {diff_size: 1GiB}\nboot: {root: {<<: *disk, diff: file:///active}}", "boot.root.diff_size"},
		{"disk-merge", "disk: &disk {size: 1GiB}\nboot: {disks: [{<<: *disk, name: data}]}", "boot.disks[0].size"},
		{"overlay-merge", "upper: &upper {size: 1GiB}\nboot: {disks: [{name: data, overlay: {<<: *upper}}]}", "boot.disks[0].overlay.size"},
		{"boot-merge", "boot_defaults: &boot {root: {diff_size: 1GiB}}\nboot: {<<: *boot}", "boot.root.diff_size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigBytes([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.path+" is not supported") {
				t.Fatalf("error = %v, want rejection of %s", err, tc.path)
			}
		})
	}
}

func TestRemovedDiskSizeKeysCannotBeHiddenByLaterConfig(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.yaml")
	valid := filepath.Join(dir, "valid.yaml")
	if err := os.WriteFile(invalid, []byte("boot: {disks: [{name: data, overlay: {diff_size: null}}]}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valid, []byte("boot: {disks: []}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, load := range map[string]func([]string) error{
		"merged": func(paths []string) error {
			_, err := LoadMerged(paths)
			return err
		},
		"presence": func(paths []string) error {
			_, _, err := LoadMergedWithPresence(paths)
			return err
		},
	} {
		for _, paths := range [][]string{{invalid, valid}, {valid, invalid}} {
			t.Run(name+"/"+filepath.Base(paths[0]), func(t *testing.T) {
				err := load(paths)
				if err == nil || !strings.Contains(err.Error(), "boot.disks[0].overlay.diff_size is not supported") {
					t.Fatalf("error = %v, want retired key rejected before merge", err)
				}
			})
		}
	}
}

func TestBootDecodePreservesSequentialMerge(t *testing.T) {
	var cfg SandboxConfig
	for _, raw := range []string{
		"boot: {kernel: file:///kernel, runtime: file:///runtime, root: {base: file:///image, overlay: {diff_template: file:///template}}, disks: [{name: data, diff: file:///data}]}\nmetadata: {size: arbitrary, diff_size: opaque}",
		"boot: {root: {overlay: {diff: file:///active}}, unrelated: {size: ignored}}",
	} {
		if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatal(err)
		}
	}
	if cfg.Boot.Kernel != "file:///kernel" || cfg.Boot.Runtime != "file:///runtime" || cfg.Boot.Root.Base != "file:///image" {
		t.Fatalf("boot merge lost prior bindings: %+v", cfg.Boot)
	}
	upper := cfg.Boot.Root.Overlay
	if upper == nil || upper.Diff != "file:///active" || upper.DiffTemplate != "file:///template" {
		t.Fatalf("overlay merge lost sibling fields: %+v", upper)
	}
	if len(cfg.Boot.Disks) != 1 || cfg.Boot.Disks[0].Name != "data" || cfg.Boot.Disks[0].Diff != "file:///data" {
		t.Fatalf("merge lost data disks: %+v", cfg.Boot.Disks)
	}
	if cfg.Metadata["size"] != "arbitrary" || cfg.Metadata["diff_size"] != "opaque" {
		t.Fatalf("unrelated metadata was changed: %+v", cfg.Metadata)
	}
}

func TestDiskConfigMarshalDoesNotEmitRetiredSizeKeys(t *testing.T) {
	cfg := SandboxConfig{Boot: BootConfig{
		Root:  RootConfig{Overlay: &OverlayConfig{DiffTemplate: "file:///template"}},
		Disks: []DiskConfig{{Name: "data", RootConfig: RootConfig{Diff: "file:///data"}}},
	}}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "diff_size:") || strings.Contains(string(raw), "\n        size:") {
		t.Fatalf("marshal emitted a retired size key:\n%s", raw)
	}
	var decoded SandboxConfig
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("config does not round-trip: %v", err)
	}
	if decoded.Boot.Root.Overlay == nil || decoded.Boot.Root.Overlay.DiffTemplate != "file:///template" || len(decoded.Boot.Disks) != 1 || decoded.Boot.Disks[0].Name != "data" {
		t.Fatalf("round-trip lost disk bindings: %+v", decoded.Boot)
	}
}

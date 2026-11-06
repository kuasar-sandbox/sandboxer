package config

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDiffCOWDiscardYAML(t *testing.T) {
	for _, tc := range []struct {
		name, input, stored, mode string
		invalid                   bool
	}{
		{name: "omitted", input: "{}", mode: "auto"},
		{name: "empty mapping", input: "resources: {diff_cow: {}}", mode: "auto"},
		{name: "auto", input: "resources: {diff_cow: {discard: auto}}", stored: "auto", mode: "auto"},
		{name: "manual", input: "resources: {diff_cow: {discard: manual}}", stored: "manual", mode: "manual"},
		{name: "empty", input: "resources: {diff_cow: {discard: ''}}", invalid: true},
		{name: "null", input: "resources: {diff_cow: {discard: null}}", invalid: true},
		{name: "bool", input: "resources: {diff_cow: {discard: false}}", invalid: true},
		{name: "number", input: "resources: {diff_cow: {discard: 0}}", invalid: true},
		{name: "sequence", input: "resources: {diff_cow: {discard: [auto]}}", invalid: true},
		{name: "mapping", input: "resources: {diff_cow: {discard: {mode: auto}}}", invalid: true},
		{name: "unknown", input: "resources: {diff_cow: {discard: disabled}}", invalid: true},
		{name: "case", input: "resources: {diff_cow: {discard: Auto}}", invalid: true},
		{name: "whitespace", input: "resources: {diff_cow: {discard: ' manual '}}", invalid: true},
		{name: "duplicate", input: "resources: {diff_cow: {discard: auto, discard: manual}}", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, presence, err := LoadConfigBytesWithPresence([]byte(tc.input))
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "discard") {
					t.Fatalf("invalid policy error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg.ApplyDefaults()
			if got := cfg.Resources.DiffCOW.Discard; got != tc.stored || presence.Has("resources.diff_cow.discard") != (tc.stored != "") {
				t.Fatalf("discard value/presence = %q/%t", got, presence.Has("resources.diff_cow.discard"))
			}
			before := cfg.Resources.DiffCOW
			mode, err := before.DiscardMode()
			if err != nil || mode != tc.mode {
				t.Fatalf("mode = %q, %v", mode, err)
			}
			resolved, err := before.Resolve()
			if err != nil || resolved.Discard != tc.stored || cfg.Resources.DiffCOW != before {
				t.Fatalf("resolution changed discard presence: %+v, %v", resolved, err)
			}
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("discard:")) != (tc.stored != "") {
				t.Fatalf("marshal changed discard presence:\n%s", raw)
			}
		})
	}
}

func TestDiffCOWDiscardProgrammaticValidation(t *testing.T) {
	for _, value := range []string{"off", "disabled", "AUTO", " manual ", "false"} {
		t.Run(value, func(t *testing.T) {
			cfg := DiffCOWConfig{Discard: value}
			if _, err := cfg.DiscardMode(); err == nil {
				t.Fatal("DiscardMode accepted invalid policy")
			}
			if err := cfg.ValidateExplicit(); err == nil {
				t.Fatal("ValidateExplicit accepted invalid policy")
			}
			if _, err := cfg.Resolve(); err == nil {
				t.Fatal("Resolve accepted invalid policy")
			}
			if _, _, err := cfg.Bytes(); err == nil {
				t.Fatal("Bytes accepted invalid policy")
			}
		})
	}
}

func TestDiffCOWDiscardMergedPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, stored string
		layers       []string
	}{
		{name: "omitted", layers: []string{"cache_size: 16MiB"}},
		{name: "manual retained by cache override", stored: "manual", layers: []string{"discard: manual", "cache_size: 16MiB"}},
		{name: "explicit auto overrides manual", stored: "auto", layers: []string{"discard: manual", "discard: auto, cache_size: 16MiB"}},
		{name: "explicit manual overrides auto", stored: "manual", layers: []string{"discard: auto", "discard: manual, cache_size: 16MiB"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := []string{writeYAML(t, "resources: {diff_cow: {cache_size: 8MiB, max_dirty_size: 4MiB}}")}
			for _, layer := range tc.layers {
				paths = append(paths, writeYAML(t, "resources: {diff_cow: {"+layer+"}}"))
			}
			cfg, presence, err := LoadMergedWithPresence(paths)
			if err != nil {
				t.Fatal(err)
			}
			ordinary, err := LoadMerged(paths)
			if err != nil {
				t.Fatal(err)
			}
			want := DiffCOWConfig{CacheSize: "16MiB", MaxDirtySize: "4MiB", Discard: tc.stored}
			if cfg.Resources.DiffCOW != want || ordinary.Resources.DiffCOW != want || presence.Has("resources.diff_cow.discard") != (tc.stored != "") {
				t.Fatalf("merged policies = %+v / %+v; want %+v", cfg.Resources.DiffCOW, ordinary.Resources.DiffCOW, want)
			}
		})
	}
}

func TestDiffCOWDiscardRunFromHostOwnership(t *testing.T) {
	artifact := portableFromFixture()
	before, err := MarshalPortableSandboxConfig(artifact)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "auto", "manual"} {
		t.Run(fmt.Sprintf("mode=%q", mode), func(t *testing.T) {
			host := restoreHostConfig()
			host.Network.TAP = "tap0"
			host.Boot.Root = RootConfig{Overlay: &OverlayConfig{DiffTemplate: "file:///node/root.ext4"}}
			host.Resources.DiffCOW = DiffCOWConfig{Discard: mode}
			runtime, c0, err := ApplyFromRules(artifact, host, FieldPresence{}, ApplyFromOptions{})
			if err != nil || runtime.Resources.DiffCOW != host.Resources.DiffCOW {
				t.Fatalf("run-from host policy was lost: runtime=%+v, err=%v", runtime, err)
			}
			for _, portable := range []*PortableSandboxConfig{artifact, c0} {
				raw, err := MarshalPortableSandboxConfig(portable)
				if err != nil || !bytes.Equal(raw, before) {
					t.Fatalf("run-from changed portable configuration: %v\n%s", err, raw)
				}
			}
		})
	}
}

func TestDiffCOWDiscardRestoreIsColdOnly(t *testing.T) {
	for _, mode := range []string{"auto", "manual"} {
		for _, withPresence := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/presence=%t", mode, withPresence), func(t *testing.T) {
				host := restoreHostConfig()
				host.Resources.DiffCOW.Discard = mode
				presence := FieldPresence{}
				if withPresence {
					presence.add("resources.diff_cow.discard")
				}
				host.ApplyDefaults()
				for _, check := range []func() error{
					func() error { return host.ValidateRestoreHostConfigWithPresence(presence) },
					func() error { _, _, err := ApplyRestoreRules(validPortableConfig(), host, presence); return err },
				} {
					if err := check(); err == nil || !strings.Contains(err.Error(), "resources.diff_cow.discard is cold-start-only") {
						t.Fatalf("restore accepted explicit %s: %v", mode, err)
					}
				}
			})
		}
	}
	// Resolution must not turn an omitted cold-launch default into a restore
	// override. Only the target host's cache budgets are reapplied on restore.
	host := restoreHostConfig()
	host.Resources.Capacity = validPortableConfig().Resources.Capacity
	host.ApplyDefaults()
	resolved, err := host.Resources.DiffCOW.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	host.Resources.DiffCOW = resolved
	if err := host.ValidateRestoreHostConfigWithPresence(FieldPresence{}); err != nil {
		t.Fatal(err)
	}
	runtime, _, err := ApplyRestoreRules(validPortableConfig(), host, FieldPresence{})
	if err != nil || runtime.Resources.DiffCOW != resolved || runtime.Resources.DiffCOW.Discard != "" {
		t.Fatalf("restore changed omitted discard policy: runtime=%+v, err=%v", runtime, err)
	}
}

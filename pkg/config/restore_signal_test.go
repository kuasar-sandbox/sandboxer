package config

import (
	"strings"
	"testing"
)

func TestParseRestoreSignal(t *testing.T) {
	for input, want := range map[string]int{"": 0, "SIGUSR1": 10, "usr1": 10, "10": 10, "SIGUSR2": 12, "64": 64} {
		if got, err := ParseRestoreSignal(input); err != nil || got != want {
			t.Fatalf("%q: %d %v", input, got, err)
		}
	}
	for _, input := range []string{"0", "-1", "65", "999", "NO_SUCH_SIGNAL", " 10", "1.5"} {
		if _, err := ParseRestoreSignal(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestRestoreSignalColdInheritanceAndPortableRoundTrip(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want, plugin string }{
		{"inherit", "", "SIGUSR1", "SIGUSR2"},
		{"clear", "launch: {restore_signal: \"\"}\n", "", "SIGUSR2"},
		{"override", "launch: {restore_signal: '12'}\n", "12", "SIGUSR2"},
		{"replace plugin", "launch: {plugin: [{exec: /new}]}\n", "SIGUSR1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := portableFromFixture()
			source.Launch.RestoreSignal = "SIGUSR1"
			source.Launch.Plugin = []PortablePluginConfig{{Exec: "/plugin", RestoreSignal: "SIGUSR2"}}
			host, presence, err := LoadConfigBytesWithPresence([]byte("network: {tap: tap0}\nboot:\n  kernel: file:///node/vmlinux\n  runtime: file:///node/runtime\n  root:\n    overlay: {diff_template: file:///upper.ext4}\n" + tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			runtime, portable, err := ApplyFromRules(source, host, presence, ApplyFromOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if runtime.Launch.RestoreSignal != tc.want || runtime.Launch.Plugin[0].RestoreSignal != tc.plugin {
				t.Fatalf("runtime: %+v", runtime.Launch)
			}
			raw, err := MarshalPortableSandboxConfig(portable)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := ParsePortableSandboxConfig(raw)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Launch.RestoreSignal != tc.want || saved.Launch.Plugin[0].RestoreSignal != tc.plugin {
				t.Fatalf("saved: %+v", saved.Launch)
			}
			projected := portableLaunch(runtime.Launch)
			if projected.RestoreSignal != tc.want || projected.Plugin[0].RestoreSignal != tc.plugin {
				t.Fatalf("export: %+v", projected)
			}
			if source.Launch.RestoreSignal != "SIGUSR1" || source.Launch.Plugin[0].RestoreSignal != "SIGUSR2" {
				t.Fatal("source mutated")
			}
		})
	}
}

func TestRestoreSignalValidationAndMemoryRestoreBoundary(t *testing.T) {
	for _, plugin := range []bool{false, true} {
		cfg, err := Load(writeYAML(t, minimalCold))
		if err != nil {
			t.Fatal(err)
		}
		saved := validPortableConfig()
		field := "launch.restore_signal"
		if plugin {
			cfg.Launch.Plugin = []PluginConfig{{Exec: "/plugin", RestoreSignal: "65"}}
			saved.Launch.Plugin = []PortablePluginConfig{{Exec: "/plugin", RestoreSignal: "65"}}
			field = "plugin[0].restore_signal"
		} else {
			cfg.Launch.RestoreSignal = "65"
			saved.Launch.RestoreSignal = "65"
		}
		for _, err := range []error{cfg.ValidateCold(), saved.Validate()} {
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("want %s validation, got %v", field, err)
			}
		}
	}
	for _, data := range []string{"launch: {restore_signal: SIGUSR1}", "launch: {restore_signal: \"\"}", "launch: {plugin: [{exec: /plugin, restore_signal: SIGUSR2}]}"} {
		host, presence, err := LoadConfigBytesWithPresence([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if err := rejectRestoreColdOnly(host, presence); err == nil {
			t.Fatalf("memory restore accepted %s", data)
		}
	}
	if err := rejectRestoreColdOnly(&SandboxConfig{Launch: LaunchConfig{RestoreSignal: "SIGUSR1"}}, FieldPresence{}); err == nil {
		t.Fatal("API bypassed restore boundary")
	}
}

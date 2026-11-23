package config

import (
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

func TestSystemdPathProvenanceCannotBeSetByLaunchDocuments(t *testing.T) {
	for _, raw := range []string{
		"launch: {systemd_path_default: true}",
		`{"launch":{"systemd_path_default":true}}`,
	} {
		if _, err := LoadConfigBytes([]byte(raw)); err == nil {
			t.Fatalf("accepted internal input: %s", raw)
		}
	}
	launch := LaunchConfig{Env: map[string]string{"PATH": "/defaults"}}
	launch.SetSystemdPathDefault(true)
	if err := yaml.Unmarshal([]byte("env: {OTHER: value}"), &launch); err != nil {
		t.Fatal(err)
	}
	if !launch.SystemdPathDefault() {
		t.Fatal("unrelated overlay erased default origin")
	}
	if err := yaml.Unmarshal([]byte("env: {PATH: /defaults}"), &launch); err != nil {
		t.Fatal(err)
	}
	if launch.SystemdPathDefault() {
		t.Fatal("even an equal explicit PATH must override origin")
	}
}

func TestSystemdPathPortableSourceAndOverrides(t *testing.T) {
	for _, defaults := range []bool{false, true} {
		for _, explicit := range []bool{false, true} {
			src := portableFromFixture()
			src.Launch.Systemd = "auto"
			src.Launch.Env = map[string]string{"PATH": "/source/default", "SOURCE": "kept"}
			src.Launch.SystemdPathDefault = defaults
			raw := systemdHostBase + "\nlaunch:\n  env:\n    EXTRA: value\n"
			if explicit {
				raw += "    PATH: /caller\n"
			}
			host, presence, err := LoadConfigBytesWithPresence([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			runtime, c0, err := ApplyFromRules(src, host, presence, ApplyFromOptions{})
			if err != nil {
				t.Fatal(err)
			}
			wantPath, wantDefault := "", true
			if defaults {
				wantPath = "/source/default"
			}
			if explicit {
				wantPath, wantDefault = "/caller", false
			}
			if c0.Launch.Env["PATH"] != wantPath || c0.Launch.SystemdPathDefault != wantDefault || runtime.Launch.SystemdPathDefault() != wantDefault {
				t.Fatalf("defaults=%v explicit=%v: portable=%+v runtime=%+v", defaults, explicit, c0.Launch, runtime.Launch)
			}
			data, err := MarshalPortableSandboxConfig(c0)
			if err != nil {
				t.Fatal(err)
			}
			round, err := ParsePortableSandboxConfig(data)
			if err != nil {
				t.Fatal(err)
			}
			if round.Launch.SystemdPathDefault != wantDefault || round.Launch.Env["PATH"] != wantPath {
				t.Fatal("portable provenance roundtrip lost")
			}
			if src.Launch.Env["PATH"] != "/source/default" || src.Launch.SystemdPathDefault != defaults {
				t.Fatal("source changed")
			}
		}
	}
	// With no env override, a pre-feature E's PATH stays authoritative.
	src := portableFromFixture()
	src.Launch.Env = map[string]string{"PATH": "/legacy"}
	host, p, err := LoadConfigBytesWithPresence([]byte(systemdHostBase))
	if err != nil {
		t.Fatal(err)
	}
	runtime, _, err := ApplyFromRules(src, host, p, ApplyFromOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Launch.SystemdPathDefault() {
		t.Fatal("guessed provenance for an old E")
	}
	data, err := yaml.Marshal(runtime.Launch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "systemd_path_default") {
		t.Fatal("internal marker leaked into ordinary Launch serializer")
	}
}

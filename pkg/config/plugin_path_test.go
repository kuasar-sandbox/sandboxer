package config

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPluginPathProvenanceRejectsOrdinaryDocuments(t *testing.T) {
	for _, value := range []string{"true", "false", "null"} {
		raw := "launch: {plugin: [{exec: /envd, systemd_path_default: " + value + "}]}"
		if _, err := LoadConfigBytes([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
		var plugin PluginConfig
		if err := json.Unmarshal([]byte(`{"exec":"/envd","systemd_path_default":`+value+`}`), &plugin); err == nil {
			t.Fatalf("accepted JSON marker %s", value)
		}
	}
	for _, key := range []string{"SystemdPathDefault", "SYSTEMD_PATH_DEFAULT"} {
		var plugin PluginConfig
		if err := json.Unmarshal([]byte(`{"`+key+`":true}`), &plugin); err == nil {
			t.Fatal("accepted alternate marker", key)
		}
	}
	if _, err := LoadConfigBytes([]byte("shared: &p {systemd_path_default: true}\nlaunch: {plugin: [{<<: *p, exec: /envd}]}")); err == nil {
		t.Fatal("YAML merge bypassed private provenance")
	}
}

func TestPluginPathExplicitOverlayAndSDKMutationClearProvenance(t *testing.T) {
	for _, useJSON := range []bool{false, true} {
		plugin := PluginConfig{Exec: "/envd", Env: map[string]string{"PATH": "/image/custom:/usr/bin"}}
		plugin.SetSystemdPathDefault(true)
		decode := func(raw string) error {
			if useJSON {
				return json.Unmarshal([]byte(raw), &plugin)
			}
			return yaml.Unmarshal([]byte(raw), &plugin)
		}
		if err := decode(`{"env":{"OTHER":"value"}}`); err != nil {
			t.Fatal(err)
		}
		if !plugin.PersistentSystemdPathDefault() {
			t.Fatal("unrelated env changed provenance")
		}
		if err := decode(`{"env":{"PATH":"/image/custom:/usr/bin"}}`); err != nil {
			t.Fatal(err)
		}
		if plugin.PersistentSystemdPathDefault() {
			t.Fatal("equal explicit PATH retained default provenance")
		}
		plugin.SetSystemdPathDefault(true)
		plugin.Env["PATH"] = ""
		if plugin.SystemdPathDefault() {
			t.Fatal("SDK explicit empty PATH retained default provenance")
		}
		delete(plugin.Env, "PATH")
		if !plugin.SystemdPathDefault() {
			t.Fatal("absent PATH did not use the implicit default")
		}
	}
}

func TestPluginPathPortableCloneAndSourceOverrides(t *testing.T) {
	for _, defaults := range []bool{false, true} {
		source := portableFromFixture()
		source.Launch.Plugin = []PortablePluginConfig{{Exec: "/envd", Args: []string{"--port", "42"}, Env: map[string]string{"PATH": "/image/custom:/usr/bin"}, SystemdPathDefault: defaults}}
		host, presence, err := LoadConfigBytesWithPresence([]byte(systemdHostBase))
		if err != nil {
			t.Fatal(err)
		}
		runtime, c0, err := ApplyFromRules(source, host, presence, ApplyFromOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if runtime.Launch.Plugin[0].PersistentSystemdPathDefault() != defaults || c0.Launch.Plugin[0].SystemdPathDefault != defaults {
			t.Fatal("source plugin provenance changed")
		}
		clone, err := c0.Clone()
		if err != nil {
			t.Fatal(err)
		}
		if clone.Launch.Plugin[0].SystemdPathDefault != defaults {
			t.Fatal("E roundtrip lost provenance")
		}
		clone.Launch.Plugin[0].Env["PATH"] = "changed"
		clone.Launch.Plugin[0].Args[0] = "changed"
		if source.Launch.Plugin[0].Env["PATH"] != "/image/custom:/usr/bin" || source.Launch.Plugin[0].Args[0] != "--port" {
			t.Fatal("source aliased a clone")
		}
		for _, value := range []string{"/image/custom:/usr/bin", ""} {
			raw := systemdHostBase + "\nlaunch:\n  plugin:\n  - exec: /envd\n    env: {PATH: " + strconv.Quote(value) + "}\n"
			overlay, p, err := LoadConfigBytesWithPresence([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			_, result, err := ApplyFromRules(source, overlay, p, ApplyFromOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Launch.Plugin[0].SystemdPathDefault || result.Launch.Plugin[0].Env["PATH"] != value {
				t.Fatal("explicit plugin override changed")
			}
		}
		for _, encode := range []func(any) ([]byte, error){yaml.Marshal, json.Marshal} {
			data, err := encode(runtime.Launch.Plugin[0])
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "systemd_path_default") || strings.Contains(string(data), "SystemdPathDefault") {
				t.Fatal("ordinary serializer exposed provenance")
			}
		}
	}
}

package config

import (
	"encoding/json"
	"strings"
	"testing"
)

const provenanceConfig = `launch:
  systemd: auto
  env: {PATH: /image/custom:/usr/bin}
  plugin:
  - exec: /envd
    env: {PATH: /image/custom:/usr/bin}
  - exec: /explicit
    env: {PATH: /image/custom:/usr/bin}
`

func trustedProvenance(t *testing.T) ([]byte, LaunchConfig) {
	t.Helper()
	cfg, err := LoadConfigBytes([]byte(provenanceConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Launch.SetSystemdPathDefault(true)
	cfg.Launch.Plugin[0].SetSystemdPathDefault(true)
	encoded, err := MarshalConfigProvenance([]byte(provenanceConfig), cfg.Launch)
	if err != nil {
		t.Fatal(err)
	}
	return encoded, cfg.Launch
}

func TestConfigProvenanceBindsExactBytesAndRetainsExplicitEquality(t *testing.T) {
	envelope, _ := trustedProvenance(t)
	cfg, err := LoadConfigBytes([]byte(provenanceConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Launch.SystemdPathDefault() || cfg.Launch.Plugin[0].SystemdPathDefault() {
		t.Fatal("ordinary decode guessed defaults")
	}
	if err := ApplyConfigProvenance([]byte(provenanceConfig), cfg, envelope); err != nil {
		t.Fatal(err)
	}
	if !cfg.Launch.SystemdPathDefault() || !cfg.Launch.Plugin[0].SystemdPathDefault() || cfg.Launch.Plugin[1].SystemdPathDefault() {
		t.Fatal("default provenance confused equal explicit strings")
	}
	if err := ApplyConfigProvenance([]byte(provenanceConfig+"\n"), cfg, envelope); err == nil {
		t.Fatal("digest ignored exact document bytes")
	}
}

func TestConfigProvenanceSurvivesFromAndC0Projection(t *testing.T) {
	raw := []byte(systemdHostBase + "\n" + provenanceConfig)
	host, presence, err := LoadConfigBytesWithPresence(raw)
	if err != nil {
		t.Fatal(err)
	}
	host.Launch.SetSystemdPathDefault(true)
	host.Launch.Plugin[0].SetSystemdPathDefault(true)
	envelope, err := MarshalConfigProvenance(raw, host.Launch)
	if err != nil {
		t.Fatal(err)
	}
	host, presence, err = LoadConfigBytesWithPresence(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyConfigProvenance(raw, host, envelope); err != nil {
		t.Fatal(err)
	}
	runtime, c0, err := ApplyFromRules(portableFromFixture(), host, presence, ApplyFromOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.Launch.SystemdPathDefault() || !runtime.Launch.Plugin[0].SystemdPathDefault() || runtime.Launch.Plugin[1].SystemdPathDefault() {
		t.Fatal("from merge dropped authenticated origin")
	}
	clone, err := c0.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if !clone.Launch.SystemdPathDefault || !clone.Launch.Plugin[0].SystemdPathDefault || clone.Launch.Plugin[1].SystemdPathDefault {
		t.Fatal("C0 omitted authenticated origin")
	}
}

func TestConfigProvenanceRejectsMismatchesBeforeMutation(t *testing.T) {
	envelope, _ := trustedProvenance(t)
	var proof configProvenance
	if err := json.Unmarshal(envelope, &proof); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*configProvenance){
		func(p *configProvenance) { p.Version = 2 },
		func(p *configProvenance) { p.ConfigSHA256 = strings.Repeat("0", 64) },
		func(p *configProvenance) { p.PluginPaths[0].Index = -1 },
		func(p *configProvenance) { p.PluginPaths[0].Index = 2 },
		func(p *configProvenance) { p.PluginPaths[0].Path = "/wrong" },
		func(p *configProvenance) { p.PluginPaths = append(p.PluginPaths, p.PluginPaths[0]) },
	} {
		candidate := proof
		candidate.PluginPaths = append([]configPathProvenance(nil), proof.PluginPaths...)
		mutate(&candidate)
		bad, _ := json.Marshal(candidate)
		cfg, err := LoadConfigBytes([]byte(provenanceConfig))
		if err != nil {
			t.Fatal(err)
		}
		if err := ApplyConfigProvenance([]byte(provenanceConfig), cfg, bad); err == nil {
			t.Fatalf("accepted mismatch: %s", bad)
		}
		if cfg.Launch.SystemdPathDefault() || cfg.Launch.Plugin[0].SystemdPathDefault() {
			t.Fatal("rejection partially changed config")
		}
	}
	for _, bad := range []string{"[]", `{"version":1,"version":1}`, `{"version":null}`, `{"other":true}`, string(envelope) + "{}", strings.Repeat(" ", MaxConfigProvenanceBytes+1), strings.Replace(string(envelope), `"index":0`, `"index":0,"index":0`, 1), strings.Replace(string(envelope), `"path":"/image/custom:/usr/bin"`, `"path":null`, 1)} {
		cfg, _ := LoadConfigBytes([]byte(provenanceConfig))
		if err := ApplyConfigProvenance([]byte(provenanceConfig), cfg, []byte(bad)); err == nil {
			t.Fatalf("accepted malformed envelope: %.150s", bad)
		}
	}
}

func TestConfigProvenanceEncoderRejectsStaleTypedProjection(t *testing.T) {
	_, launch := trustedProvenance(t)
	launch.Env["PATH"] = "/different"
	launch.SetSystemdPathDefault(true)
	if _, err := MarshalConfigProvenance([]byte(provenanceConfig), launch); err == nil {
		t.Fatal("accepted stale top PATH")
	}
	_, launch = trustedProvenance(t)
	launch.Plugin[0].Env["PATH"] = "/different"
	launch.Plugin[0].SetSystemdPathDefault(true)
	if _, err := MarshalConfigProvenance([]byte(provenanceConfig), launch); err == nil {
		t.Fatal("accepted stale plugin PATH")
	}
}

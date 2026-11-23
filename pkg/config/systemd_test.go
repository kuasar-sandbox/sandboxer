package config

import (
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
)

const systemdHostBase = "network: {tap: tap0}\nboot:\n kernel: file:///node/vmlinux\n runtime: file:///node/runtime\n root:\n  overlay: {diff_template: file:///node/upper.ext4}\n"

func TestSystemdHostParser(t *testing.T) {
	for _, m := range []string{"off", "auto", "runtime", "provided"} {
		t.Run(m, func(t *testing.T) {
			raw := []byte("launch: {systemd: " + m + "}\n")
			for _, withPresence := range []bool{false, true} {
				var c *SandboxConfig
				var err error
				if withPresence {
					var p FieldPresence
					c, p, err = LoadConfigBytesWithPresence(raw)
					if err == nil && !p.Has("launch.systemd") {
						t.Fatal("presence lost")
					}
				} else {
					c, err = LoadConfigBytes(raw)
				}
				if err != nil {
					t.Fatal(err)
				}
				if c.Launch.Systemd != m {
					t.Fatalf("mode=%q", c.Launch.Systemd)
				}
			}
		})
	}
	for _, m := range []string{"null", "false", "true", "{}", "[]", "''", "disabled", "image"} {
		t.Run("invalid_"+m, func(t *testing.T) {
			if _, e := LoadConfigBytes([]byte("launch: {systemd: " + m + "}")); e == nil {
				t.Fatal("accepted invalid")
			}
		})
	}
	c, e := LoadConfigBytes([]byte("{}"))
	if e != nil {
		t.Fatal(e)
	}
	if c.Launch.Systemd != "" || c.Launch.EffectiveSystemd() != "off" {
		t.Fatal("default erased presence")
	}
}

func TestSystemdApplyFromMatrix(t *testing.T) {
	for _, src := range []string{"", "off", "auto", "runtime", "provided"} {
		for _, request := range []string{"", "off", "auto", "runtime", "provided"} {
			t.Run(src+"/"+request, func(t *testing.T) {
				artifact := portableFromFixture()
				artifact.Launch.Systemd = src
				artifact.Launch.CgroupControl = false
				raw := systemdHostBase
				if request != "" {
					raw += "\nlaunch: {systemd: " + request + "}\n"
				}
				host, p, e := LoadConfigBytesWithPresence([]byte(raw))
				if e != nil {
					t.Fatal(e)
				}
				runtime, c0, e := ApplyFromRules(artifact, host, p, ApplyFromOptions{})
				if e != nil {
					t.Fatal(e)
				}
				want := src
				if request != "" {
					want = request
				}
				if want == "" {
					want = "off"
				}
				if c0.Launch.Systemd != want || runtime.Launch.Systemd != want {
					t.Fatalf("runtime=%q c0=%q want=%q", runtime.Launch.Systemd, c0.Launch.Systemd, want)
				}
				if c0.Launch.CgroupControl || runtime.Launch.CgroupControl {
					t.Fatal("rewrote raw cgroup_control")
				}
				data, e := MarshalPortableSandboxConfig(c0)
				if e != nil {
					t.Fatal(e)
				}
				round, e := ParsePortableSandboxConfig(data)
				if e != nil {
					t.Fatal(e)
				}
				if round.Launch.Systemd != want || round.Launch.CgroupControl {
					t.Fatal("portable roundtrip drift")
				}
				if artifact.Launch.Systemd != src {
					t.Fatal("mutated E source")
				}
			})
		}
	}
}

func TestSystemdApplyRestoreRejectsOffSDKAndYAML(t *testing.T) {
	for _, m := range []string{"off", "auto", "runtime", "provided"} {
		t.Run(m, func(t *testing.T) {
			host, p, e := LoadConfigBytesWithPresence([]byte(systemdHostBase + "\nlaunch: {systemd: " + m + "}\n"))
			if e != nil {
				t.Fatal(e)
			}
			if e = rejectRestoreColdOnly(host, p); e == nil {
				t.Fatal("YAML accepted cold override")
			}
			if e = rejectRestoreColdOnly(host, FieldPresence{}); e == nil {
				t.Fatal("SDK accepted cold override")
			}
			if _, _, e = ApplyRestoreRules(portableFromFixture(), host, FieldPresence{}); e == nil {
				t.Fatal("SDK ApplyRestoreRules accepted override")
			}
		})
	}
}

func TestSystemdPortableRejectsInvalidOrUnknown(t *testing.T) {
	src := portableFromFixture()
	src.Launch.Systemd = "auto"
	raw, e := MarshalPortableSandboxConfig(src)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range []string{"null", "false", "{}", "[]", "''", "disabled", "image"} {
		t.Run(v, func(t *testing.T) {
			mod := strings.Replace(string(raw), "systemd: auto", "systemd: "+v, 1)
			if _, e := ParsePortableSandboxConfig([]byte(mod)); e == nil {
				t.Fatalf("portable accepted %s", v)
			}
		})
	}
	unknown := strings.Replace(string(raw), "systemd: auto", "systemd: auto\n    nonexistent: 1", 1)
	if _, e := ParsePortableSandboxConfig([]byte(unknown)); e == nil {
		t.Fatal("KnownFields was weakened")
	}
}

func TestSystemdAliasMergeAndProgrammaticE(t *testing.T) {
	artifact := portableFromFixture()
	artifact.Launch.Systemd = "provided"
	for _, launch := range []string{"launch: {systemd: off}", "launch:\n  <<: {systemd: off}", "mode: &mode off\nlaunch: {systemd: *mode}"} {
		raw := systemdHostBase + "\n" + launch + "\n"
		host, p, e := LoadConfigBytesWithPresence([]byte(raw))
		if e != nil {
			t.Fatal(e)
		}
		_, c0, e := ApplyFromRules(artifact, host, p, ApplyFromOptions{})
		if e != nil {
			t.Fatal(e)
		}
		if c0.Launch.Systemd != "off" {
			t.Fatal("lost explicit off through alias/merge")
		}
		_, c0, e = ApplyFromRules(artifact, host, FieldPresence{}, ApplyFromOptions{})
		if e != nil {
			t.Fatal(e)
		}
		if c0.Launch.Systemd != "off" {
			t.Fatal("SDK could not express explicit off")
		}
	}
}

func TestSystemdMultiFileOverlayKeepsMode(t *testing.T) {
	var c SandboxConfig
	if e := yaml.Unmarshal([]byte("launch: {systemd: provided, exec: /bin/old}"), &c); e != nil {
		t.Fatal(e)
	}
	if e := yaml.Unmarshal([]byte("launch: {exec: /bin/new}"), &c); e != nil {
		t.Fatal(e)
	}
	if c.Launch.Systemd != "provided" || c.Launch.Exec != "/bin/new" {
		t.Fatalf("overlay=%+v", c.Launch)
	}
	for _, pidns := range []string{"", "private", "shared", "bogus"} {
		src := portableFromFixture()
		src.Launch.Systemd = "runtime"
		src.Launch.PIDNamespace = pidns
		e := src.Validate()
		if pidns == "bogus" && e == nil {
			t.Fatal("systemd weakened pid_namespace validation")
		}
		if pidns != "bogus" && e != nil {
			t.Fatalf("pidns=%q %v", pidns, e)
		}
	}
}

func TestStopGraceAccessorWholeSeconds(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{{"", 10}, {"0", 10}, {"0s", 10}, {"500ms", 0}, {"1500ms", 1}, {"30s", 30}} {
		c := SandboxConfig{Launch: LaunchConfig{StopGracePeriod: tc.raw}}
		if got := c.StopGraceSeconds(); got != tc.want {
			t.Errorf("%q: got %d, want %d", tc.raw, got, tc.want)
		}
	}
}

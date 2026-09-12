package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestUsageDefaultsStrictAndOverrides(t *testing.T) {
	c, err := LoadConfigBytes([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	s, f, err := c.Usage.Intervals()
	if err != nil || c.Usage.Enabled || s != time.Second || f != 5*time.Minute {
		t.Fatalf("%+v %v", c.Usage, err)
	}
	for _, body := range []string{"usage: {enabeld: true}", "usage: {enabled: 'true'}", "usage: {sample_interval: ''}", "usage: {enabled: true, enabled: false}", "usage: null", "usage: ~", "usage: []", "usage: {enabled: null}", "defaults: &defaults {usage: null}\n<<: *defaults", "defaults: &defaults {enabled: true}\nusage: {<<: *defaults}"} {
		if _, err := LoadConfigBytes([]byte(body)); err == nil {
			t.Fatal(body)
		}
	}
	for _, u := range []UsageConfig{{SampleInterval: "0"}, {SampleInterval: "-1s"}, {SampleInterval: "1e999s"}, {SampleInterval: "2s", FlushInterval: "1s"}, {FlushInterval: "NaN"}} {
		if _, _, err := u.Intervals(); err == nil {
			t.Fatal(u)
		}
	}
	paths := []string{filepath.Join(t.TempDir(), "a.yaml"), filepath.Join(t.TempDir(), "b.yaml")}
	for i, body := range []string{"usage: {enabled: true, sample_interval: 2s, flush_interval: 8m}", "usage: {enabled: false, flush_interval: 9m}"} {
		if err := os.WriteFile(paths[i], []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, err = LoadMerged(paths)
	if err != nil || c.Usage.Enabled || c.Usage.SampleInterval != "2s" || c.Usage.FlushInterval != "9m" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestUsageHostOnlyFromAndRestore(t *testing.T) {
	host, presence, err := LoadConfigBytesWithPresence([]byte("boot: {kernel: 'file:///node/vmlinux', runtime: 'file:///node/runtime'}\nnetwork: {tap: tap0}\nusage: {enabled: true, sample_interval: 3s, flush_interval: 8m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, restore := range []bool{false, true} {
		artifact := portableFromFixture()
		artifact.Resources.Allocatable.CPU = float64(artifact.Resources.Capacity.CPU)
		var runtime *SandboxConfig
		var portable *PortableSandboxConfig
		if restore {
			runtime, portable, err = ApplyRestoreRules(artifact, host, presence)
		} else {
			runtime, portable, err = ApplyFromRules(artifact, host, presence, ApplyFromOptions{})
		}
		if err != nil {
			t.Fatal(err)
		}
		if runtime.Usage != host.Usage {
			t.Fatal("lost runtime policy")
		}
		encoded, err := yaml.Marshal(portable)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "usage:") {
			t.Fatal("usage leaked into portable artifact")
		}
	}
}

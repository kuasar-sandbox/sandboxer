package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

const qosQoSPolicy = `resources:
  diff_cow:
    discard: manual
    cache_size: 32MiB
    cache_idle_after: 10s
    max_dirty_size: 32MiB
    read_bandwidth: 50M
    write_bandwidth: 20M
    read_iops: 1000
    write_iops: 500
    max_dirty_size_per_disk: 64MiB
    read_bandwidth_per_disk: 50M
    write_bandwidth_per_disk: 20M
    read_iops_per_disk: 1000
    write_iops_per_disk: 500
`

func TestDiffCOWQoSDefaultsAndTypedPolicy(t *testing.T) {
	c, err := (DiffCOWConfig{}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxDirtySize != "0" {
		t.Fatalf("dirty default=%q", c.MaxDirtySize)
	}
	cache, dirty, err := c.Bytes()
	if err != nil || cache != 32<<20 || dirty != 0 {
		t.Fatalf("cache/dirty=%d/%d, %v", cache, dirty, err)
	}
	limits, err := c.Limits()
	if err != nil || limits != (DiffCOWLimits{}) {
		t.Fatalf("omitted policy=%+v, %v", limits, err)
	}
	cfg, _, err := LoadConfigBytesWithPresence([]byte(qosQoSPolicy))
	if err != nil {
		t.Fatal(err)
	}
	limits, err = cfg.Resources.DiffCOW.Limits()
	if err != nil {
		t.Fatal(err)
	}
	if limits.Aggregate.ReadBandwidth != 52_428_800 || limits.Aggregate.WriteBandwidth != 20_971_520 || limits.PerDisk.MaxDirtySize != 64<<20 {
		t.Fatalf("policy=%+v", limits)
	}
	// Per-disk limits may be wider than aggregate/cache: the intersection wins.
	if limits.Aggregate.MaxDirtySize != 32<<20 {
		t.Fatalf("dirty=%+v", limits)
	}
	for _, raw := range []string{"max_dirty_size: 0", "max_dirty_size: '0'", "read_bandwidth: 1B", "read_bandwidth: 1.0B", "max_dirty_size_per_disk: 64MiB"} {
		if _, _, err := LoadConfigBytesWithPresence([]byte("resources: {diff_cow: {" + raw + "}}")); err != nil {
			t.Fatalf("valid %s: %v", raw, err)
		}
	}
}

func TestDiffCOWQoSOverlayAndSerialization(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base.yaml")
	over := filepath.Join(t.TempDir(), "over.yaml")
	if err := os.WriteFile(base, []byte(qosQoSPolicy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, []byte("resources: {diff_cow: {max_dirty_size: 0, read_bandwidth: 0, read_iops: 0, read_iops_per_disk: 0}}"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, presence, err := LoadMergedWithPresence([]string{base, over})
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Resources.DiffCOW
	if c.ReadIOPS == nil || *c.ReadIOPS != 0 || c.ReadIOPSPerDisk == nil || *c.ReadIOPSPerDisk != 0 || c.ReadBandwidth != "0" || c.MaxDirtySize != "0" {
		t.Fatalf("zero lost: %+v", c)
	}
	if c.WriteBandwidth != "20M" || c.WriteIOPS == nil || *c.WriteIOPS != 500 {
		t.Fatalf("omitted did not inherit: %+v", c)
	}
	for _, field := range []string{"max_dirty_size", "read_bandwidth", "read_iops", "read_iops_per_disk"} {
		if !presence.Has("resources.diff_cow." + field) {
			t.Fatalf("presence missing %s", field)
		}
	}
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			var data []byte
			var decoded DiffCOWConfig
			var err error
			if format == "yaml" {
				data, err = yaml.Marshal(c)
				if err == nil {
					err = yaml.Unmarshal(data, &decoded)
				}
			} else {
				data, err = json.Marshal(c)
				if err == nil {
					err = json.Unmarshal(data, &decoded)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, c) {
				t.Fatalf("zero/omission roundtrip changed: %s\n%+v", data, decoded)
			}
			*decoded.ReadIOPS = 17
			if *c.ReadIOPS != 0 {
				t.Fatal("serialization clone aliased source")
			}
		})
	}
}

func TestDiffCOWQoSStrictLeaves(t *testing.T) {
	for _, field := range []string{"read_iops", "write_iops", "read_iops_per_disk", "write_iops_per_disk"} {
		for _, value := range []string{"-1", "1.0", "1.5", "'1'", "null", "true", "[]", "{}", "18446744073709551616"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				_, _, err := LoadConfigBytesWithPresence([]byte(fmt.Sprintf("resources: {diff_cow: {%s: %s}}", field, value)))
				if err == nil {
					t.Fatal("non-integer or invalid value accepted")
				}
			})
		}
	}
	for _, leaf := range []string{"read_bandwidth: -1", "read_bandwidth: null", "read_bandwidth: '50M/s'", "read_bandwidth: '0.1B'", "read_bandwidth: 18446744073709551616", "max_dirty_size_per_disk: 1B", "read_iops: 0, read_iops: 1", "read_bandwith: 50M"} {
		if _, _, err := LoadConfigBytesWithPresence([]byte("resources: {diff_cow: {" + leaf + "}}")); err == nil {
			t.Fatalf("invalid accepted: %s", leaf)
		}
	}
	if _, _, err := (DiffCOWConfig{CacheSize: "32MiB", MaxDirtySize: "64MiB"}).Bytes(); err == nil {
		t.Fatal("existing positive aggregate/cache relationship lost")
	}
}

func TestDiffCOWQoSHostPolicyCloneAndPortableBoundary(t *testing.T) {
	cfg, _, err := LoadConfigBytesWithPresence([]byte(qosQoSPolicy))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"restore", "from"} {
		t.Run(mode, func(t *testing.T) {
			host := restoreHostConfig()
			host.Resources.DiffCOW = cfg.Resources.DiffCOW.Clone()
			host.Resources.DiffCOW.Discard = ""
			var runtime *SandboxConfig
			var c0 *PortableSandboxConfig
			var err error
			if mode == "restore" {
				runtime, c0, err = ApplyRestoreRules(validPortableConfig(), host, FieldPresence{})
			} else {
				host.Network.TAP = "tap0"
				host.Boot.Root = RootConfig{Overlay: &OverlayConfig{DiffTemplate: "file:///node/root.ext4"}}
				runtime, c0, err = ApplyFromRules(portableFromFixture(), host, FieldPresence{}, ApplyFromOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(runtime.Resources.DiffCOW, host.Resources.DiffCOW) {
				t.Fatal("target host policy lost")
			}
			*runtime.Resources.DiffCOW.ReadIOPS = 0
			if *host.Resources.DiffCOW.ReadIOPS != 1000 {
				t.Fatal("host pointer alias")
			}
			data, err := MarshalPortableSandboxConfig(c0)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("diff_cow")) {
				t.Fatalf("host policy leaked into portable artifact: %s", data)
			}
		})
	}
}

package config

import (
	"fmt"

	"github.com/kuasar-sandbox/sandboxer/pkg/util"
	"gopkg.in/yaml.v3"
)

// DiffCOWConfig is host-only sandbox-ctl memory policy, shared across all active
// writable disks. MaxDirtySize is a subset of CacheSize, not an extra pool.
type DiffCOWConfig struct {
	CacheSize    string `yaml:"cache_size,omitempty"`
	MaxDirtySize string `yaml:"max_dirty_size,omitempty"`
}

func (c *DiffCOWConfig) defaults() {
	if c.CacheSize == "" {
		c.CacheSize = "32MiB"
	}
	if c.MaxDirtySize == "" {
		c.MaxDirtySize = "16MiB"
	}
}
func diffCOWSize(field, value string) (uint64, error) {
	n, err := util.ParseSize(value)
	if err != nil || n == 0 || n%4096 != 0 || n > uint64(int(^uint(0)>>1)) {
		return 0, fmt.Errorf("resources.diff_cow.%s must be a positive 4 KiB multiple fitting host address space (got %q)", field, value)
	}
	return n, nil
}
func (c DiffCOWConfig) Bytes() (uint64, uint64, error) {
	c.defaults()
	total, err := diffCOWSize("cache_size", c.CacheSize)
	if err != nil {
		return 0, 0, err
	}
	dirty, err := diffCOWSize("max_dirty_size", c.MaxDirtySize)
	if err != nil {
		return 0, 0, err
	}
	if dirty > total {
		return 0, 0, fmt.Errorf("resources.diff_cow.max_dirty_size must be <= cache_size")
	}
	return total, dirty, nil
}

// Validate explicit empty/zero values and unknown fields even when nested YAML
// custom decoding would otherwise bypass KnownFields. Preserve merge semantics.
func (c *DiffCOWConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("resources.diff_cow must be a mapping")
	}
	seen := make(map[string]bool)
	for i := 0; i < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		if key != "cache_size" && key != "max_dirty_size" {
			return fmt.Errorf("field %s not found in type config.DiffCOWConfig", key)
		}
		if seen[key] {
			return fmt.Errorf("resources.diff_cow.%s duplicated", key)
		}
		seen[key] = true
		if val.Kind != yaml.ScalarNode || val.Tag == "!!null" {
			return fmt.Errorf("resources.diff_cow.%s must be a size", key)
		}
		if _, err := diffCOWSize(key, val.Value); err != nil {
			return err
		}
		if key == "cache_size" {
			c.CacheSize = val.Value
		} else {
			c.MaxDirtySize = val.Value
		}
	}
	return nil
}

package config

import (
	"fmt"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/util"
	"gopkg.in/yaml.v3"
)

// DiffCOWConfig is host-selected policy for the sandbox's writable disks.
// MaxDirtySize is a subset of the shared CacheSize, not an extra pool, and an
// empty MaxDirtySize is derived from the final effective CacheSize. Discard
// selects the cold-launch ext4 mount policy; it cannot reconfigure restored
// mounts. None of these fields is included in portable Sandbox configuration.
type DiffCOWConfig struct {
	CacheSize    string `yaml:"cache_size,omitempty"`
	MaxDirtySize string `yaml:"max_dirty_size,omitempty"`
	Discard      string `yaml:"discard,omitempty"`
}

func (c *DiffCOWConfig) defaults() {
	if c.CacheSize == "" {
		c.CacheSize = "32MiB"
	}
	// Keep an omitted dirty limit unset so later cache overrides can change it.
	// Preserve omitted Discard too: restore must distinguish no mount-policy
	// request from an explicit cold-only setting. DiscardMode resolves it.
}

// DiscardMode returns the cold-launch mount policy without changing c. An
// omitted Go value means auto; explicit empty YAML is rejected during decoding.
func (c DiffCOWConfig) DiscardMode() (string, error) {
	switch c.Discard {
	case "", proto.DiscardAuto:
		return proto.DiscardAuto, nil
	case proto.DiscardManual:
		return proto.DiscardManual, nil
	default:
		return "", fmt.Errorf("resources.diff_cow.discard must be auto or manual (got %q)", c.Discard)
	}
}

func diffCOWSize(field, value string) (uint64, error) {
	n, err := util.ParseSize(value)
	if err != nil || n == 0 || n%4096 != 0 || n > uint64(int(^uint(0)>>1)) {
		return 0, fmt.Errorf("resources.diff_cow.%s must be a positive 4 KiB multiple fitting host address space (got %q)", field, value)
	}
	return n, nil
}

// ValidateExplicit validates supplied nonempty fields without applying defaults.
// The dirty <= cache relationship is checked only when both fields are supplied.
// Use Resolve or Bytes to validate a complete configuration before runtime use.
func (c DiffCOWConfig) ValidateExplicit() error {
	_, _, err := c.sizes(false)
	return err
}

// Resolve validates the complete policy and resolves its cache limits without
// changing c. Explicit field strings are preserved; an omitted dirty limit
// becomes a decimal byte count equal to half the final cache size, rounded down
// to a 4 KiB multiple with a 4 KiB minimum. Discard keeps its explicit/omitted
// representation; use DiscardMode when preparing a cold launch.
func (c DiffCOWConfig) Resolve() (DiffCOWConfig, error) {
	_, dirty, err := c.sizes(true)
	if err != nil {
		return DiffCOWConfig{}, err
	}
	c.defaults()
	if c.MaxDirtySize == "" {
		c.MaxDirtySize = fmt.Sprintf("%d", dirty)
	}
	return c, nil
}

// Bytes returns the fully resolved cache and dirty limits without changing c.
func (c DiffCOWConfig) Bytes() (uint64, uint64, error) {
	return c.sizes(true)
}

// sizes shares validation and default calculation between partial validation
// and final resolution. YAML decoding deliberately validates only each leaf.
func (c DiffCOWConfig) sizes(resolve bool) (uint64, uint64, error) {
	if _, err := c.DiscardMode(); err != nil {
		return 0, 0, err
	}
	if resolve {
		c.defaults()
	}
	var total, dirty uint64
	var err error
	if c.CacheSize != "" {
		total, err = diffCOWSize("cache_size", c.CacheSize)
		if err != nil {
			return 0, 0, err
		}
	}
	if c.MaxDirtySize != "" {
		dirty, err = diffCOWSize("max_dirty_size", c.MaxDirtySize)
		if err != nil {
			return 0, 0, err
		}
	} else if resolve {
		dirty = max(uint64(4096), total/2/4096*4096)
	}
	if total != 0 && dirty > total {
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
		if key != "cache_size" && key != "max_dirty_size" && key != "discard" {
			return fmt.Errorf("field %s not found in type config.DiffCOWConfig", key)
		}
		if seen[key] {
			return fmt.Errorf("resources.diff_cow.%s duplicated", key)
		}
		seen[key] = true
		if key == "discard" {
			if val.Kind != yaml.ScalarNode || val.Tag != "!!str" || val.Value == "" {
				return fmt.Errorf("resources.diff_cow.discard must be auto or manual")
			}
			if _, err := (DiffCOWConfig{Discard: val.Value}).DiscardMode(); err != nil {
				return err
			}
			c.Discard = val.Value
			continue
		}
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

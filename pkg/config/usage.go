package config

import (
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// UsageConfig is host runtime policy and never enters PortableSandboxConfig.
type UsageConfig struct {
	Enabled        bool   `yaml:"enabled"`
	SampleInterval string `yaml:"sample_interval"`
	FlushInterval  string `yaml:"flush_interval"`
}

func (u *UsageConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("usage must be a mapping")
	}
	seen := map[string]bool{}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode || seen[key.Value] {
			return errors.New("usage: duplicate/non-scalar key")
		}
		seen[key.Value] = true
		switch key.Value {
		case "enabled":
			if value.Tag != "!!bool" {
				return errors.New("usage.enabled must be a boolean")
			}
			if err := value.Decode(&u.Enabled); err != nil {
				return err
			}
		case "sample_interval", "flush_interval":
			if value.Tag != "!!str" || value.Value == "" {
				return fmt.Errorf("usage.%s must be a positive duration", key.Value)
			}
			if key.Value == "sample_interval" {
				u.SampleInterval = value.Value
			} else {
				u.FlushInterval = value.Value
			}
		default:
			return fmt.Errorf("usage: unknown field %q", key.Value)
		}
	}
	return nil
}

func (u UsageConfig) Intervals() (time.Duration, time.Duration, error) {
	sample, flush := u.SampleInterval, u.FlushInterval
	if sample == "" {
		sample = "1s"
	}
	if flush == "" {
		flush = "5m"
	}
	s, err := time.ParseDuration(sample)
	if err != nil || s <= 0 || s > time.Duration(1<<62-1) {
		return 0, 0, errors.New("usage.sample_interval must be positive and allow a 2*interval continuity bound")
	}
	f, err := time.ParseDuration(flush)
	if err != nil || f < s {
		return 0, 0, errors.New("usage.flush_interval must be finite and >= sample_interval")
	}
	return s, f, nil
}

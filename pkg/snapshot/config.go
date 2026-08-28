package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"gopkg.in/yaml.v3"
)

const (
	SnapshotConfigVersion  = 1
	MaxSnapshotConfigBytes = 1 << 20
	MaxMemoryFromRefs      = 64
)

// Config is the complete V1 snapshot.cfg schema. Disk and launch provenance
// live exclusively in the referenced Sandbox E.
type Config struct {
	Version    int      `yaml:"version"`
	SandboxRef string   `yaml:"sandbox_ref"`
	FromRefs   []string `yaml:"from_refs,omitempty"`
}

func (c *Config) Validate() error {
	if c == nil {
		return errors.New("snapshot config is nil")
	}
	if c.Version != SnapshotConfigVersion {
		return fmt.Errorf("unsupported snapshot format/version %d", c.Version)
	}
	if err := validateSnapshotRef("sandbox_ref", c.SandboxRef); err != nil {
		return err
	}
	if len(c.FromRefs) > MaxMemoryFromRefs {
		return fmt.Errorf("snapshot from_refs exceeds %d entries", MaxMemoryFromRefs)
	}
	seen := make(map[string]int, len(c.FromRefs))
	for i, raw := range c.FromRefs {
		if err := validateSnapshotRef(fmt.Sprintf("from_refs[%d]", i), raw); err != nil {
			return err
		}
		if previous, duplicate := seen[raw]; duplicate {
			return fmt.Errorf("snapshot from_refs[%d] duplicates from_refs[%d]", i, previous)
		}
		seen[raw] = i
	}
	return nil
}

func validateSnapshotRef(field, raw string) error {
	if raw == "" {
		return fmt.Errorf("snapshot %s is required", field)
	}
	if raw == "self" {
		return fmt.Errorf("snapshot %s cannot be self", field)
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", field, err)
	}
	if ref.Scheme == manifest.RefSchemeFile {
		if filepath.IsAbs(ref.Path) || filepath.Base(ref.Path) != ref.Path || strings.ContainsAny(ref.Path, `/\\`) {
			return fmt.Errorf("snapshot %s local ref must use a basename", field)
		}
		if ref.DigestScheme == "" || ref.Digest == "" {
			return fmt.Errorf("snapshot %s local ref requires content identity", field)
		}
	}
	return nil
}

func MarshalConfig(cfg *Config) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot config: %w", err)
	}
	if len(raw) > MaxSnapshotConfigBytes {
		return nil, fmt.Errorf("snapshot config exceeds %d bytes", MaxSnapshotConfigBytes)
	}
	if _, err := ParseConfig(raw); err != nil {
		return nil, fmt.Errorf("validate canonical snapshot config: %w", err)
	}
	return raw, nil
}

func ParseConfig(raw []byte) (*Config, error) {
	if len(raw) == 0 {
		return nil, errors.New("snapshot config is empty")
	}
	if len(raw) > MaxSnapshotConfigBytes {
		return nil, fmt.Errorf("snapshot config exceeds %d bytes", MaxSnapshotConfigBytes)
	}
	var document yaml.Node
	nodes := yaml.NewDecoder(bytes.NewReader(raw))
	if err := nodes.Decode(&document); err != nil {
		return nil, fmt.Errorf("snapshot config YAML: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("snapshot config must be one mapping document")
	}
	if err := validateSnapshotYAMLNode(document.Content[0], ""); err != nil {
		return nil, fmt.Errorf("snapshot config YAML: %w", err)
	}
	var extra yaml.Node
	if err := nodes.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("snapshot config contains multiple documents")
		}
		return nil, fmt.Errorf("snapshot config trailing YAML: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("unsupported snapshot format: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func validateSnapshotYAMLNode(node *yaml.Node, path string) error {
	if node == nil || node.Kind == yaml.AliasNode || node.Alias != nil {
		return fmt.Errorf("%s: YAML aliases are not allowed", snapshotYAMLPath(path))
	}
	switch node.Kind {
	case yaml.MappingNode:
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" {
				return fmt.Errorf("%s: invalid mapping key", snapshotYAMLPath(path))
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return fmt.Errorf("%s: duplicate field %q", snapshotYAMLPath(path), key.Value)
			}
			seen[key.Value] = struct{}{}
			child := key.Value
			if path != "" {
				child = path + "." + key.Value
			}
			if err := validateSnapshotYAMLNode(value, child); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			if err := validateSnapshotYAMLNode(child, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
	default:
		return fmt.Errorf("%s: unsupported YAML node", snapshotYAMLPath(path))
	}
	return nil
}

func snapshotYAMLPath(path string) string {
	if path == "" {
		return "document"
	}
	return path
}

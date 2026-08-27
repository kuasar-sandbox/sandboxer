package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// FieldPresence records which YAML paths were explicitly supplied. It lets
// artifact merge rules distinguish an absent value from an intentional zero,
// false, empty map, or empty list.
type FieldPresence struct {
	paths map[string]struct{}
}

func (p FieldPresence) Has(path string) bool {
	_, ok := p.paths[path]
	return ok
}

func (p FieldPresence) Any(prefix string) bool {
	if p.Has(prefix) {
		return true
	}
	prefix += "."
	for path := range p.paths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func (p *FieldPresence) add(path string) {
	if path == "" {
		return
	}
	if p.paths == nil {
		p.paths = make(map[string]struct{})
	}
	p.paths[path] = struct{}{}
}

func (p *FieldPresence) merge(other FieldPresence) {
	for path := range other.paths {
		p.add(path)
	}
}

// LoadMergedWithPresence is the presence-aware counterpart of LoadMerged.
// Host YAML retains the established merge/default behavior; the additional
// map is consumed only by ApplyFromRules ownership checks.
func LoadMergedWithPresence(paths []string) (*SandboxConfig, FieldPresence, error) {
	if len(paths) == 0 {
		return nil, FieldPresence{}, errors.New("sandbox: no config files given")
	}
	var cfg SandboxConfig
	var combined FieldPresence
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, FieldPresence{}, fmt.Errorf("sandbox: read %s: %w", path, err)
		}
		presence, err := inspectPresence(raw)
		if err != nil {
			return nil, FieldPresence{}, fmt.Errorf("sandbox: parse %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, FieldPresence{}, fmt.Errorf("sandbox: parse %s: %w", path, err)
		}
		combined.merge(presence)
	}
	cfg.ApplyDefaults()
	return &cfg, combined, nil
}

func LoadConfigBytesWithPresence(raw []byte) (*SandboxConfig, FieldPresence, error) {
	presence, err := inspectPresence(raw)
	if err != nil {
		return nil, FieldPresence{}, fmt.Errorf("sandbox: parse config bytes: %w", err)
	}
	var cfg SandboxConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, FieldPresence{}, fmt.Errorf("sandbox: parse config bytes: %w", err)
	}
	cfg.ApplyDefaults()
	return &cfg, presence, nil
}

func inspectPresence(raw []byte) (FieldPresence, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return FieldPresence{}, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return FieldPresence{}, errors.New("sandbox config must be one mapping document")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return FieldPresence{}, err
	} else if err == nil {
		return FieldPresence{}, errors.New("sandbox config contains multiple YAML documents")
	}
	var presence FieldPresence
	collectPresence(document.Content[0], "", &presence)
	return presence, nil
}

func collectPresence(node *yaml.Node, prefix string, presence *FieldPresence) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		path := key.Value
		if prefix != "" {
			path = prefix + "." + key.Value
		}
		presence.add(path)
		if value.Kind == yaml.MappingNode {
			collectPresence(value, path, presence)
		}
	}
}

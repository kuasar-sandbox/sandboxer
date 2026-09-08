package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// UnmarshalYAML rejects the retired disk-size settings at the input boundary.
// They never enforced a capacity or quota. Checking key presence rather than
// decoded values also rejects empty/null settings and inherited YAML merge keys.
// Keep unrelated fields permissive and decode into the existing value so the
// sequential-decode contract of LoadMerged is unchanged.
func (b *BootConfig) UnmarshalYAML(node *yaml.Node) error {
	var input struct {
		Root  map[string]yaml.Node   `yaml:"root"`
		Disks []map[string]yaml.Node `yaml:"disks"`
	}
	if err := node.Decode(&input); err != nil {
		return err
	}
	if err := rejectDiskSizeFields(input.Root, "boot.root"); err != nil {
		return err
	}
	for i, disk := range input.Disks {
		if err := rejectDiskSizeFields(disk, fmt.Sprintf("boot.disks[%d]", i)); err != nil {
			return err
		}
	}
	type plain BootConfig
	return node.Decode((*plain)(b))
}

func rejectDiskSizeFields(fields map[string]yaml.Node, path string) error {
	if err := rejectDiskSizeKeys(fields, path); err != nil {
		return err
	}
	if node, ok := fields["overlay"]; ok {
		var overlay map[string]yaml.Node
		if err := node.Decode(&overlay); err != nil {
			return fmt.Errorf("%s.overlay: %w", path, err)
		}
		return rejectDiskSizeKeys(overlay, path+".overlay")
	}
	return nil
}

func rejectDiskSizeKeys(fields map[string]yaml.Node, path string) error {
	for _, key := range []string{"diff_size", "size"} {
		if _, ok := fields[key]; ok {
			return fmt.Errorf("%s.%s is not supported; remove it and use an existing diff, diff_template, or base with the required logical capacity; disk-size settings do not enforce a quota", path, key)
		}
	}
	return nil
}

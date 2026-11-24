package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// The empty value means omitted input. Defaults must not erase it before E
// inheritance; an effective cold definition can materialize it as off.
func validSystemdMode(mode string) bool {
	switch mode {
	case "", "off", "auto", "runtime", "provided":
		return true
	}
	return false
}

// EffectiveSystemd resolves an already inherited policy without erasing input presence.
func (c LaunchConfig) EffectiveSystemd() string {
	if c.Systemd == "" {
		return "off"
	}
	return c.Systemd
}

// EffectiveSystemd returns the cold-start policy of a complete portable definition.
func (c PortableLaunchConfig) EffectiveSystemd() string {
	return LaunchConfig{Systemd: c.Systemd}.EffectiveSystemd()
}

func validateSystemdNode(n *yaml.Node) error {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag != "!!str" || n.Value == "" || !validSystemdMode(n.Value) {
		return fmt.Errorf("launch.systemd must be off|auto|runtime|provided")
	}
	return nil
}

// UnmarshalYAML validates presence at the parent: yaml.v3 does not invoke a
// scalar field's UnmarshalYAML for null. Decode over the existing value so an
// omitted field in a later overlay retains its earlier value.
func (l *LaunchConfig) UnmarshalYAML(n *yaml.Node) error {
	var fields map[string]yaml.Node
	if err := n.Decode(&fields); err != nil {
		return err
	}
	if _, ok := fields["systemd_path_default"]; ok {
		return fmt.Errorf("launch.systemd_path_default is internal artifact metadata, not a launch option")
	}
	if v, ok := fields["systemd"]; ok {
		if err := validateSystemdNode(&v); err != nil {
			return err
		}
	}
	type plain LaunchConfig
	out := plain(*l)
	if err := n.Decode(&out); err != nil {
		return err
	}
	*l = LaunchConfig(out)

	return nil
}

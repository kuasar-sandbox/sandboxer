package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// PersistentSystemdPathDefault distinguishes a materialized product default
// from an explicitly configured PATH, including an explicit empty string.
// Absence uses the Guest default; old artifacts with a PATH and no marker keep
// their exact value. A later SDK map mutation cannot retain stale provenance.
func (p PluginConfig) PersistentSystemdPathDefault() bool {
	path, present := p.Env["PATH"]
	return !present || p.systemdPathDefault && path == p.systemdDefaultPath
}

func (p PluginConfig) SystemdPathDefault() bool { return p.PersistentSystemdPathDefault() }

// SetSystemdPathDefault is a trusted artifact/product projection operation.
// The marker is private and ordinary YAML/JSON inputs cannot supply it.
func (p *PluginConfig) SetSystemdPathDefault(value bool) {
	path, present := p.Env["PATH"]
	p.systemdPathDefault = value && present
	p.systemdDefaultPath = ""
	if p.systemdPathDefault {
		p.systemdDefaultPath = path
	}
}

func (p *PluginConfig) UnmarshalYAML(node *yaml.Node) error {
	var fields map[string]yaml.Node
	if err := node.Decode(&fields); err != nil {
		return err
	}
	if _, present := fields["systemd_path_default"]; present {
		return fmt.Errorf("launch.plugin.systemd_path_default is internal artifact metadata, not a plugin option")
	}
	type plain PluginConfig
	out := plain(*p)
	if err := node.Decode(&out); err != nil {
		return err
	}
	*p = PluginConfig(out)
	if env, present := fields["env"]; present {
		var entries map[string]yaml.Node
		if err := env.Decode(&entries); err != nil {
			return err
		}
		if _, explicit := entries["PATH"]; explicit {
			p.SetSystemdPathDefault(false)
		}
	}
	return nil
}

func (p *PluginConfig) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key := range fields {
		if strings.EqualFold(key, "systemd_path_default") || strings.EqualFold(key, "SystemdPathDefault") {
			return fmt.Errorf("launch.plugin.systemd_path_default is internal artifact metadata, not a plugin option")
		}
	}
	type plain PluginConfig
	out := plain(*p)
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	*p = PluginConfig(out)
	for key, value := range fields {
		if !strings.EqualFold(key, "env") {
			continue
		}
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(value, &entries); err != nil {
			return err
		}
		if _, explicit := entries["PATH"]; explicit {
			p.SetSystemdPathDefault(false)
		}
	}
	return nil
}

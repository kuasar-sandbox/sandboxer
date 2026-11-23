package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// ConfigProvenanceFDEnv is an internal trusted-task/CLI handoff. It is not
	// a user configuration namespace or an authentication boundary.
	ConfigProvenanceFDEnv    = "_SANDBOX_CONFIG_PROVENANCE_FD"
	MaxConfigProvenanceBytes = 64 << 10
)

type configPathProvenance struct {
	Index int    `json:"index"`
	Path  string `json:"path"`
}

type configProvenance struct {
	Version            int                    `json:"version"`
	ConfigSHA256       string                 `json:"config_sha256"`
	SystemdPathDefault bool                   `json:"systemd_path_default,omitempty"`
	PluginPaths        []configPathProvenance `json:"plugin_paths,omitempty"`
}

func configBytesDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// MarshalConfigProvenance carries only trusted default origins across the
// ordinary YAML file boundary. The exact bytes and materialized values must
// agree with the typed projection; strings never establish origin by themselves.
func MarshalConfigProvenance(raw []byte, launch LaunchConfig) ([]byte, error) {
	cfg, err := LoadConfigBytes(raw)
	if err != nil {
		return nil, err
	}
	proof := configProvenance{Version: 1, ConfigSHA256: configBytesDigest(raw)}
	if path, present := launch.Env["PATH"]; present && launch.PersistentSystemdPathDefault() {
		actual, exists := cfg.Launch.Env["PATH"]
		if !exists || path != actual {
			return nil, errors.New("config provenance: projected launch PATH does not match config")
		}
		proof.SystemdPathDefault = true
	}
	for index, plugin := range launch.Plugin {
		if path, present := plugin.Env["PATH"]; present && plugin.PersistentSystemdPathDefault() {
			proof.PluginPaths = append(proof.PluginPaths, configPathProvenance{Index: index, Path: path})
		}
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return nil, err
	}
	if err := ApplyConfigProvenance(raw, cfg, encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

// ApplyConfigProvenance runs after ordinary decoding and before E inheritance.
// It validates the whole envelope before changing any origin. Callers retain
// the bytes they decoded, rather than re-reading a possibly replaced path.
func ApplyConfigProvenance(raw []byte, cfg *SandboxConfig, envelope []byte) error {
	if cfg == nil {
		return errors.New("config provenance: nil config")
	}
	if len(envelope) == 0 || len(envelope) > MaxConfigProvenanceBytes {
		return errors.New("config provenance: invalid envelope size")
	}
	fields, err := provenanceObject(envelope, "version", "config_sha256", "systemd_path_default", "plugin_paths")
	if err != nil {
		return err
	}
	var proof configProvenance
	if err := json.Unmarshal(fields["version"], &proof.Version); err != nil || proof.Version != 1 {
		return errors.New("config provenance: unsupported envelope version")
	}
	if err := json.Unmarshal(fields["config_sha256"], &proof.ConfigSHA256); err != nil || proof.ConfigSHA256 != configBytesDigest(raw) {
		return errors.New("config provenance: config digest mismatch")
	}
	if value, present := fields["systemd_path_default"]; present {
		if err := json.Unmarshal(value, &proof.SystemdPathDefault); err != nil {
			return fmt.Errorf("config provenance: default PATH: %w", err)
		}
	}
	if value, present := fields["plugin_paths"]; present {
		var plugins []json.RawMessage
		if err := json.Unmarshal(value, &plugins); err != nil {
			return fmt.Errorf("config provenance: plugin paths: %w", err)
		}
		if len(plugins) > MaxPortablePlugins {
			return errors.New("config provenance: too many plugin paths")
		}
		for _, item := range plugins {
			entry, err := provenanceObject(item, "index", "path")
			if err != nil {
				return err
			}
			var plugin configPathProvenance
			if err := json.Unmarshal(entry["index"], &plugin.Index); err != nil {
				return fmt.Errorf("config provenance: plugin index: %w", err)
			}
			if err := json.Unmarshal(entry["path"], &plugin.Path); err != nil {
				return fmt.Errorf("config provenance: plugin PATH: %w", err)
			}
			proof.PluginPaths = append(proof.PluginPaths, plugin)
		}
	}
	if proof.SystemdPathDefault {
		if _, exists := cfg.Launch.Env["PATH"]; !exists {
			return errors.New("config provenance: default launch PATH is absent")
		}
	}
	seen := make(map[int]struct{}, len(proof.PluginPaths))
	for _, plugin := range proof.PluginPaths {
		if plugin.Index < 0 || plugin.Index >= len(cfg.Launch.Plugin) {
			return errors.New("config provenance: plugin index is out of range")
		}
		if _, exists := seen[plugin.Index]; exists {
			return errors.New("config provenance: duplicate plugin index")
		}
		seen[plugin.Index] = struct{}{}
		path, exists := cfg.Launch.Plugin[plugin.Index].Env["PATH"]
		if !exists || path != plugin.Path {
			return errors.New("config provenance: plugin PATH does not match config")
		}
	}
	cfg.Launch.SetSystemdPathDefault(proof.SystemdPathDefault)
	for index := range cfg.Launch.Plugin {
		_, isDefault := seen[index]
		cfg.Launch.Plugin[index].SetSystemdPathDefault(isDefault)
	}
	return nil
}

// Strict internal objects reject unknown fields, duplicate keys, nulls and
// trailing JSON, so a malformed handoff cannot silently choose one authority.
func provenanceObject(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("config provenance: expected an object")
	}
	valid := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		valid[key] = true
	}
	fields := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		if !valid[key] {
			return nil, fmt.Errorf("config provenance: unknown or duplicate field %q", key)
		}
		delete(valid, key)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("config provenance: %s may not be null", key)
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("config provenance: trailing JSON")
	}
	return fields, nil
}

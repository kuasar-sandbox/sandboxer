package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// consumeConfigProvenance owns only the explicitly inherited descriptor. A
// sealed memfd is a regular file, with finite size and no open-stream EOF wait.
// ReadAt keeps the exact payload independent of a shared descriptor offset.
func consumeConfigProvenance() ([]byte, error) {
	raw, supplied := os.LookupEnv(config.ConfigProvenanceFDEnv)
	if !supplied {
		return nil, nil
	}
	_ = os.Unsetenv(config.ConfigProvenanceFDEnv)
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 || fd > 1<<31-1 {
		return nil, errors.New("config provenance: invalid inherited descriptor")
	}
	file := os.NewFile(uintptr(fd), "sandbox-config-provenance")
	if file == nil {
		return nil, errors.New("config provenance: invalid inherited descriptor")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("config provenance: descriptor: %w", err)
	}
	if !stat.Mode().IsRegular() || stat.Size() <= 0 || stat.Size() > config.MaxConfigProvenanceBytes {
		return nil, errors.New("config provenance: descriptor must contain a regular file of 1..65536 bytes")
	}
	data, err := io.ReadAll(io.NewSectionReader(file, 0, stat.Size()))
	if err != nil {
		return nil, fmt.Errorf("config provenance: read: %w", err)
	}
	if int64(len(data)) != stat.Size() {
		return nil, errors.New("config provenance: descriptor content changed during read")
	}
	return data, nil
}

func loadRunConfig(paths string, withPresence bool, envelope []byte) (*config.SandboxConfig, config.FieldPresence, error) {
	if len(envelope) != 0 {
		if paths == "" || len(strings.Split(paths, ":")) != 1 {
			return nil, config.FieldPresence{}, errors.New("config provenance requires one exact --config document")
		}
		raw, err := os.ReadFile(paths)
		if err != nil {
			return nil, config.FieldPresence{}, err
		}
		cfg, presence, err := config.LoadConfigBytesWithPresence(raw)
		if err != nil {
			return nil, config.FieldPresence{}, err
		}
		if err := config.ApplyConfigProvenance(raw, cfg, envelope); err != nil {
			return nil, config.FieldPresence{}, err
		}
		return cfg, presence, nil
	}
	if paths == "" {
		cfg := &config.SandboxConfig{}
		cfg.ApplyDefaults()
		return cfg, config.FieldPresence{}, nil
	}
	if withPresence {
		return config.LoadMergedWithPresence(strings.Split(paths, ":"))
	}
	cfg, err := config.LoadMerged(strings.Split(paths, ":"))
	return cfg, config.FieldPresence{}, err
}

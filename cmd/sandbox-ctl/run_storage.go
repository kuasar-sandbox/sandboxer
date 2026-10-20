package main

import (
	"fmt"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// newRunProcessStorage captures the environment key before removing it from
// the runtime environment, the environment inherited by future children, and
// the original environment memory exposed by /proc.
func newRunProcessStorage(cfg *config.ManifestConfig) (*artifact.ProcessStorage, error) {
	return newRunProcessStorageWithScrub(cfg, scrubInitialEnvironment)
}

func newRunProcessStorageWithScrub(cfg *config.ManifestConfig, scrub func(string) error) (*artifact.ProcessStorage, error) {
	_, keyInEnvironment := os.LookupEnv(manifest.CustomerKeyEnv)
	storage, err := artifact.NewProcessStorage(cfg)
	if err != nil {
		return nil, err
	}
	// With crypto.local=off, ProcessStorage resolves the key lazily. Force
	// resolution while the environment override is still available, so later
	// remote reads and snapshot uploads retain the same tenant key.
	if keyFn := storage.CustomerKeyFunc(); keyFn != nil && os.Getenv(manifest.CustomerKeyEnv) != "" {
		if _, err := keyFn(); err != nil {
			_ = storage.Close()
			return nil, err
		}
	}
	// YAML-only and local-only invocations must not require readable /proc.
	// An explicitly supplied environment entry (even an empty one) must still
	// be removed from both environments; failures in that case are fatal.
	if !keyInEnvironment {
		return storage, nil
	}
	if err := os.Unsetenv(manifest.CustomerKeyEnv); err != nil {
		_ = storage.Close()
		return nil, fmt.Errorf("unset manifest key environment: %w", err)
	}
	if err := scrub(manifest.CustomerKeyEnv); err != nil {
		_ = storage.Close()
		return nil, fmt.Errorf("scrub initial manifest key environment: %w", err)
	}
	return storage, nil
}

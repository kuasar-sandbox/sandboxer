package main

import (
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// Keep the CLI's MANIFEST_KEY convention even for a file-only invocation.
// Passing nil cfg to storage still leaves Manifest clients disabled.
func newCLIStorage(cfg *config.ManifestConfig) (*artifact.ProcessStorage, error) {
	keyConfig := cfg
	if keyConfig == nil {
		keyConfig = &config.ManifestConfig{}
	}
	return artifact.NewProcessStorageWithCustomerKey(cfg, keyConfig.CustomerKey)
}

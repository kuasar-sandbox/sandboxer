package main

import (
	"fmt"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// Explicit nodiscard also overrides a filesystem image whose superblock enables
// discard by default. The policy is resolved before any workload disk is mounted.
func ext4MountOptions(mode string) (string, error) {
	switch mode {
	case "", proto.DiscardAuto:
		return "discard", nil
	case proto.DiscardManual:
		return "nodiscard", nil
	default:
		return "", fmt.Errorf("launch ext4_discard %q invalid (want auto|manual)", mode)
	}
}

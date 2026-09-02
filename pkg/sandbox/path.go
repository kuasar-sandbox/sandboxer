package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ResolvePathID returns the directory leaf used below a run/base root. An
// omitted PathID preserves the historical layout by using SandboxID.
func ResolvePathID(sandboxID, pathID string) (string, error) {
	if pathID == "" {
		pathID = sandboxID
	}
	if err := ValidatePathID(pathID); err != nil {
		return "", err
	}
	return pathID, nil
}

// ValidatePathID accepts exactly one safe host-path component. PathID is only
// a directory leaf; it does not change or validate the logical SandboxID.
func ValidatePathID(pathID string) error {
	if pathID == "" || pathID == "." || pathID == ".." ||
		filepath.Base(pathID) != pathID || strings.ContainsAny(pathID, "/\\\x00") {
		return fmt.Errorf("path id %q must be one non-empty path component", pathID)
	}
	return nil
}

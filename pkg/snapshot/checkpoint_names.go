package snapshot

import (
	"os"
	"strconv"
	"strings"
)

// CheckpointArtifactName recognizes exactly the immutable namespace emitted by
// FileSink and BundleSink. It grants no ownership of the containing directory.
func CheckpointArtifactName(name string) bool {
	stem, ext, ok := strings.Cut(name, ".")
	if !ok || !lowerHex(stem, 64) {
		return false
	}
	switch ext {
	case "snapshot", "sandbox", "overlay", "image", "bundle":
		return true
	}
	return false
}

func lowerHex(s string, width int) bool {
	if len(s) != width {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// CheckpointCandidate matches producer names AND types, including interrupted
// writes whose contents need not be valid. Aliases are unlinked, never followed.
// target must be the readlink value for symlinks; unexpected targets are kept.
func CheckpointCandidate(name, sandboxID string, mode os.FileMode, target string) bool {
	if validateArtifactAliasID(sandboxID) != nil {
		return false
	}
	if mode.IsRegular() {
		if CheckpointArtifactName(name) {
			return true
		}
		for _, kind := range []string{"snapshot", "sandbox", "overlay", "image", "bundle"} {
			suffix, ok := strings.CutPrefix(name, artifactPartialPrefix(sandboxID, kind))
			if !ok {
				continue
			}
			suffix, ok = strings.CutSuffix(suffix, ".partial")
			if !ok {
				continue
			}
			n, err := strconv.ParseUint(suffix, 10, 32)
			// os.CreateTemp uses strconv.FormatUint(uint64(uint32(runtime_rand())), 10).
			if err == nil && strconv.FormatUint(n, 10) == suffix {
				return true
			}
		}
		return false
	}
	if mode&os.ModeSymlink == 0 || !CheckpointArtifactName(target) {
		return false
	}
	for _, role := range []string{"snapshot", "sandbox"} {
		if !strings.HasSuffix(target, "."+role) && !strings.HasSuffix(target, ".bundle") {
			continue
		}
		if name == sandboxID+"."+role {
			return true
		}
		suffix, ok := strings.CutPrefix(name, artifactAliasTempPrefix(sandboxID, role))
		if !ok {
			continue
		}
		suffix, ok = strings.CutSuffix(suffix, ".tmp")
		if ok && lowerHex(suffix, 2*artifactAliasRandomBytes) {
			return true
		}
	}
	return false
}

const artifactAliasRandomBytes = 16

func artifactPartialPrefix(sandboxID, kind string) string { return sandboxID + "." + kind + "." }
func artifactPartialPattern(sandboxID, kind string) string {
	return artifactPartialPrefix(sandboxID, kind) + "*.partial"
}
func artifactAliasTempPrefix(sandboxID, role string) string {
	return "." + sandboxID + "." + role + "."
}

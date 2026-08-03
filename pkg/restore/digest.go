package restore

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/internal/runtimebundle"
	"github.com/kuasar-sandbox/sandboxer/internal/tartransition"
)

func openTarArtifact(path string) (fetch.Stream, string, error) {
	stream, err := fetch.OpenTarStream(path)
	if err != nil {
		return nil, "", err
	}
	digest, ok, digestErr := tartransition.Digest(stream)
	if digestErr != nil {
		stream.Close()
		return nil, "", fmt.Errorf("file artifact %s has invalid declared digest: %w", path, digestErr)
	}
	if !ok {
		stream.Close()
		return nil, "", fmt.Errorf("file artifact %s has no declared digest", path)
	}
	return stream, digest, nil
}

func readRuntimeBundleDigest(path string) (string, error) {
	info, err := runtimebundle.Inspect(path)
	if err != nil {
		return "", err
	}
	return info.Digest, nil
}

func readTarArtifactDigest(path string) (string, error) {
	stream, digest, err := openTarArtifact(path)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	return digest, nil
}

func digestHex(digest string) (string, error) {
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if hexDigest == digest || len(hexDigest) != 64 || strings.ToLower(hexDigest) != hexDigest {
		return "", fmt.Errorf("invalid SHA256 digest %q", digest)
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", fmt.Errorf("invalid SHA256 digest %q", digest)
	}
	return hexDigest, nil
}

func matchDigest(got, wantHex string) error {
	hexDigest, err := digestHex(got)
	if err != nil {
		return err
	}
	if hexDigest != wantHex {
		return fmt.Errorf("sha256 marker mismatch (got %s, want %s)", hexDigest, wantHex)
	}
	return nil
}

func validateContentAddressedName(path, digest string) error {
	hexDigest, err := digestHex(digest)
	if err != nil {
		return err
	}
	real := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		real = resolved
	}
	base := filepath.Base(real)
	stem, _, ok := strings.Cut(base, ".")
	if !ok || stem != hexDigest {
		return fmt.Errorf("artifact basename %q does not match marker %s", base, hexDigest)
	}
	return nil
}

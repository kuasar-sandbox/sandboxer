package config

import (
	"fmt"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
)

// RuntimeContentMatches compares canonical portable runtime content identities.
// The basename is a host lookup hint, not part of content equality. Both refs
// must remain unlocated file basenames with an explicit digest type and value.
// This function performs no file I/O and does not verify the runtime payload.
func RuntimeContentMatches(required, actual string) (bool, error) {
	parse := func(label, raw string) (manifest.Ref, error) {
		if err := validatePortableHostArtifactRef("boot.runtime", raw); err != nil {
			return manifest.Ref{}, fmt.Errorf("runtime %s identity: %w", label, err)
		}
		return manifest.ParseRef(raw)
	}
	want, err := parse("required", required)
	if err != nil {
		return false, err
	}
	have, err := parse("actual", actual)
	if err != nil {
		return false, err
	}
	return want.DigestScheme == have.DigestScheme && want.Digest == have.Digest, nil
}

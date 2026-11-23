package artifact

import "github.com/kuasar-sandbox/sandboxer/internal/runtimebundle"

// RuntimeBundleInfo contains the runtime's declared content identity and size.
// It is distinct from a whole-file checksum and from a Manifest Bundle.
type RuntimeBundleInfo = runtimebundle.Info

// InspectRuntimeBundle uses the same envelope reader as Node restore. It reads
// the local ZIP footer and digest marker without scanning the EROFS payload.
func InspectRuntimeBundle(path string) (RuntimeBundleInfo, error) {
	return runtimebundle.Inspect(path)
}

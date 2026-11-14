package artifact

import (
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// DiskV1 is the portable logical disk returned by ExportDisk.
type DiskV1 = config.DiskV1

// ResolveDiskTemplate confines named seed paths to their configured directory
// using the same alias checks as immutable carriers.
func ResolveDiskTemplate(ref manifest.Ref, locations config.RefLocations) (string, error) {
	path, err := locations.ResolveFile(ref, "")
	if err != nil {
		return "", err
	}
	return resolveFileTarget(path, ref, locations)
}

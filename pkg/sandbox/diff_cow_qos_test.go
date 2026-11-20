package sandbox

import (
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"testing"
)

func TestDiffCOWQoSSDKCloneRetainsZero(t *testing.T) {
	var zero uint64
	source := &config.SandboxConfig{Resources: config.ResourcesConfig{DiffCOW: config.DiffCOWConfig{MaxDirtySize: "0", ReadIOPS: &zero}}}
	cloned, err := cloneSDKConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	if cloned.Resources.DiffCOW.ReadIOPS == nil || *cloned.Resources.DiffCOW.ReadIOPS != 0 || cloned.Resources.DiffCOW.WriteIOPS != nil {
		t.Fatal("SDK clone lost zero/omitted presence")
	}
	*cloned.Resources.DiffCOW.ReadIOPS = 8
	if *source.Resources.DiffCOW.ReadIOPS != 0 {
		t.Fatal("SDK clone aliased resources")
	}
}

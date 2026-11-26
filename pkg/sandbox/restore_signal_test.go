package sandbox

import (
	"encoding/json"
	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"testing"
)

func TestRestoreSignalLaunchAndMaterialization(t *testing.T) {
	image := &ImageConfig{Cmd: []string{"/app"}, StopSignal: "SIGTERM"}
	for _, input := range []string{"", "SIGUSR1", "12"} {
		cfg := &config.SandboxConfig{Launch: config.LaunchConfig{RestoreSignal: input, Plugin: []config.PluginConfig{{Exec: "/plugin", RestoreSignal: "SIGUSR2"}}}}
		want, _ := config.ParseRestoreSignal(input)
		if err := MaterializeImageDefaults(cfg, image); err != nil {
			t.Fatal(err)
		}
		spec, err := MergeLaunch(image, cfg.Launch)
		if err != nil {
			t.Fatal(err)
		}
		spec.Plugins, err = toProtoPlugins(cfg.Launch.Plugin)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		var wire proto.LaunchSpec
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if wire.RestoreSignal != want || wire.Plugins[0].RestoreSignal != 12 || wire.StopSignal != 15 {
			t.Fatalf("wire %+v", wire)
		}
	}
	if _, err := MergeLaunch(image, config.LaunchConfig{RestoreSignal: "65"}); err == nil {
		t.Fatal("invalid main signal accepted")
	}
	if _, err := toProtoPlugins([]config.PluginConfig{{Exec: "/plugin", RestoreSignal: "65"}}); err == nil {
		t.Fatal("invalid plugin signal accepted")
	}
}

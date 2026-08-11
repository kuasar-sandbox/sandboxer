package sandbox

import (
	"reflect"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestMergeLaunch_OverrideTakesPrecedence(t *testing.T) {
	image := &ImageConfig{
		Cmd:        []string{"image-arg"},
		Entrypoint: []string{"/image/exec"},
		Env:        []string{"FROM_IMAGE=1", "BOTH=image"},
		WorkingDir: "/image-dir",
	}
	override := config.LaunchConfig{
		Exec:          "/override/exec",
		Args:          []string{"override-arg"},
		Env:           map[string]string{"BOTH": "override", "FROM_OVERRIDE": "1"},
		Workdir:       "/override-dir",
		Restart:       "always",
		CgroupControl: true,
	}
	got, err := MergeLaunch(image, override)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "/override/exec" {
		t.Errorf("Exec = %q", got.Exec)
	}
	if !reflect.DeepEqual(got.Args, []string{"override-arg"}) {
		t.Errorf("Args = %v", got.Args)
	}
	if got.Workdir != "/override-dir" {
		t.Errorf("Workdir = %q", got.Workdir)
	}
	if got.Restart != "always" {
		t.Errorf("Restart = %q", got.Restart)
	}
	if !got.CgroupControl {
		t.Error("CgroupControl = false, want true")
	}
	if got.Env["BOTH"] != "override" {
		t.Errorf("Env BOTH = %q (override should win)", got.Env["BOTH"])
	}
	if got.Env["FROM_IMAGE"] != "1" {
		t.Errorf("Env FROM_IMAGE = %q (image-only key should survive)", got.Env["FROM_IMAGE"])
	}
	if got.Env["FROM_OVERRIDE"] != "1" {
		t.Errorf("Env FROM_OVERRIDE = %q", got.Env["FROM_OVERRIDE"])
	}
}

func TestMergeLaunch_FallsBackToImageEntrypoint(t *testing.T) {
	image := &ImageConfig{
		Entrypoint: []string{"/image/exec", "ep-arg"},
		Cmd:        []string{"cmd-arg"},
	}
	override := config.LaunchConfig{} // empty override
	got, err := MergeLaunch(image, override)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "/image/exec" {
		t.Errorf("Exec = %q", got.Exec)
	}
	wantArgs := []string{"ep-arg", "cmd-arg"}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", got.Args, wantArgs)
	}
}

func TestMergeLaunch_ErrorWhenNoExecAnywhere(t *testing.T) {
	_, err := MergeLaunch(&ImageConfig{}, config.LaunchConfig{})
	if err == nil {
		t.Fatal("expected error when neither image nor override provides exec")
	}
}

func TestMergeLaunch_Placeholder(t *testing.T) {
	// Placeholder ignores image Entrypoint/Cmd, needs no exec, and forces
	// restart=always even when the config asks for something else. Env/workdir
	// still apply to the anchor process.
	image := &ImageConfig{
		Entrypoint: []string{"/image/exec"},
		Cmd:        []string{"ignored"},
		Env:        []string{"FROM_IMAGE=1"},
	}
	override := config.LaunchConfig{
		Placeholder: true,
		Restart:     "never", // must be overridden to "always"
		Workdir:     "/work",
		Env:         map[string]string{"K": "v"},
	}
	got, err := MergeLaunch(image, override)
	if err != nil {
		t.Fatalf("placeholder MergeLaunch: %v", err)
	}
	if !got.Placeholder {
		t.Error("Placeholder = false, want true")
	}
	if got.Exec != "" || len(got.Args) != 0 {
		t.Errorf("Exec/Args = %q/%v, want empty (image Entrypoint/Cmd ignored)", got.Exec, got.Args)
	}
	if got.Restart != "always" {
		t.Errorf("Restart = %q, want always (forced)", got.Restart)
	}
	if got.Workdir != "/work" {
		t.Errorf("Workdir = %q, want /work", got.Workdir)
	}
	if got.Env["K"] != "v" || got.Env["FROM_IMAGE"] != "1" {
		t.Errorf("Env = %v, want override+image merged", got.Env)
	}
}

func TestMergeLaunch_FallsBackToImageCmdWhenEntrypointEmpty(t *testing.T) {
	// python:3.12-slim shape: Entrypoint=[], Cmd=["python3"]
	image := &ImageConfig{
		Cmd: []string{"python3"},
		Env: []string{"PATH=/usr/local/bin:/usr/bin"},
	}
	got, err := MergeLaunch(image, config.LaunchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "python3" {
		t.Errorf("Exec = %q, want python3", got.Exec)
	}
	if len(got.Args) != 0 {
		t.Errorf("Args = %v, want empty", got.Args)
	}
}

func TestMergeLaunch_OverrideArgsReplaceImageCmd(t *testing.T) {
	// Docker semantics: `docker run img foo bar` keeps Entrypoint, replaces Cmd.
	image := &ImageConfig{
		Entrypoint: []string{"/usr/bin/wrap"},
		Cmd:        []string{"image-cmd-arg"},
	}
	got, err := MergeLaunch(image, config.LaunchConfig{Args: []string{"new-arg"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "/usr/bin/wrap" {
		t.Errorf("Exec = %q", got.Exec)
	}
	if !reflect.DeepEqual(got.Args, []string{"new-arg"}) {
		t.Errorf("Args = %v, want [new-arg] (override replaces image.Cmd)", got.Args)
	}
}

func TestMergeLaunch_CmdOnlyImageWithOverrideArgs(t *testing.T) {
	// python:3.12-slim + override args = "python3 -c 'print(...)'"
	image := &ImageConfig{Cmd: []string{"python3"}}
	got, err := MergeLaunch(image, config.LaunchConfig{
		Args: []string{"-c", "print('hi')"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != "python3" {
		t.Errorf("Exec = %q", got.Exec)
	}
	if !reflect.DeepEqual(got.Args, []string{"-c", "print('hi')"}) {
		t.Errorf("Args = %v", got.Args)
	}
}

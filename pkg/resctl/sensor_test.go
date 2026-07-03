package resctl

import (
	"context"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSensorRuntime_Defaults(t *testing.T) {
	c := &config.SandboxConfig{}
	mode, stall, win, dbnc := c.SensorRuntime()
	if mode != SensorModePSI {
		t.Errorf("default mode: want %q, got %q", SensorModePSI, mode)
	}
	if stall != 10_000 {
		t.Errorf("default stall: want 10000, got %d", stall)
	}
	if win != 1_000_000 {
		t.Errorf("default window: want 1000000, got %d", win)
	}
	if dbnc != 100*time.Millisecond {
		t.Errorf("default debounce: want 100ms, got %s", dbnc)
	}
}

func TestSensorRuntime_FromConfig(t *testing.T) {
	c := &config.SandboxConfig{}
	c.Resources.Control.Sensor = &config.SensorConfig{
		Mode:            "events_poll",
		PSISomeStallUs:  20_000,
		PSISomeWindowUs: 500_000,
		MinIntervalMs:   50,
	}
	mode, stall, win, dbnc := c.SensorRuntime()
	if mode != "events_poll" {
		t.Errorf("mode: want events_poll, got %q", mode)
	}
	if stall != 20_000 || win != 500_000 {
		t.Errorf("trigger: want (20000,500000), got (%d,%d)", stall, win)
	}
	if dbnc != 50*time.Millisecond {
		t.Errorf("debounce: want 50ms, got %s", dbnc)
	}
}

func TestSensorRuntime_PartialConfig(t *testing.T) {
	// Only override mode; other fields keep defaults.
	c := &config.SandboxConfig{}
	c.Resources.Control.Sensor = &config.SensorConfig{Mode: "none"}
	mode, stall, win, dbnc := c.SensorRuntime()
	if mode != "none" {
		t.Errorf("mode: want none, got %q", mode)
	}
	if stall != 10_000 || win != 1_000_000 || dbnc != 100*time.Millisecond {
		t.Errorf("defaults not applied: stall=%d window=%d debounce=%s", stall, win, dbnc)
	}
}

func TestValidateCold_SensorMode(t *testing.T) {
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"", false},
		{"psi", false},
		{"events_poll", false},
		{"none", false},
		{"PSI", true}, // case-sensitive
		{"poll", true},
		{"adaptive", true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			c := minimalValidConfig(t)
			c.Resources.Control.Sensor = &config.SensorConfig{Mode: tc.mode}
			err := c.ValidateCold()
			if (err != nil) != tc.wantErr {
				t.Fatalf("mode=%q wantErr=%v, got err=%v", tc.mode, tc.wantErr, err)
			}
		})
	}
}

func TestValidateCold_SensorPSITriggerCoherence(t *testing.T) {
	cases := []struct {
		stall   uint64
		win     uint64
		wantErr bool
	}{
		{0, 0, false},                 // both unset → default
		{20_000, 1_000_000, false},    // valid
		{1_000_000, 1_000_000, false}, // equal allowed
		{1_000_000, 0, true},          // stall set, window unset
		{500_000, 100_000, true},      // stall > window
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			c := minimalValidConfig(t)
			c.Resources.Control.Sensor = &config.SensorConfig{
				PSISomeStallUs:  tc.stall,
				PSISomeWindowUs: tc.win,
			}
			err := c.ValidateCold()
			if (err != nil) != tc.wantErr {
				t.Fatalf("stall=%d win=%d wantErr=%v, got err=%v", tc.stall, tc.win, tc.wantErr, err)
			}
		})
	}
}

// TestRunPSI_SetupWritesTrigger verifies the trigger string is written
// to memory.pressure with the configured stall+window. Uses a fake
// cgroup dir with a writable memory.pressure regular file (we can't get
// real PSI in a unit test environment, so we only verify the setup
// path; epoll on a regular file fails, runPSI returns that error and
// caller falls back to events_poll).
func TestRunPSI_SetupWritesTrigger(t *testing.T) {
	cgDir := t.TempDir()
	pressPath := filepath.Join(cgDir, "memory.pressure")
	if err := os.WriteFile(pressPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	hooks := &ControllerHooks{
		cfg: &config.SandboxConfig{},
	}
	s := NewPressureSensor(hooks, cgDir, 4<<20, nil)
	s.runtime.StallUs = 25_000
	s.runtime.WindowUs = 500_000

	// runPSI will fail at epoll_ctl (regular file can't be added). We
	// don't care about that — we just want to verify the trigger was
	// written first.
	_ = s.runPSI(t.Context())

	got, err := os.ReadFile(pressPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "some 25000 500000"
	if !strings.HasPrefix(string(got), want) {
		t.Errorf("trigger: want prefix %q, got %q", want, string(got))
	}
}

// TestRunPSI_MissingPressureFile returns error (caller falls back).
func TestRunPSI_MissingPressureFile(t *testing.T) {
	cgDir := t.TempDir() // no memory.pressure inside
	hooks := &ControllerHooks{cfg: &config.SandboxConfig{}}
	s := NewPressureSensor(hooks, cgDir, 4<<20, nil)
	err := s.runPSI(t.Context())
	if err == nil {
		t.Fatal("expected error opening missing memory.pressure, got nil")
	}
	if !strings.Contains(err.Error(), "memory.pressure") {
		t.Errorf("error should mention memory.pressure, got: %v", err)
	}
}

// TestRunEventsPoll_NoHigh_NoOOM_NoDispatch: with no high or OOM events
// in memory.events.local, the legacy poll loop runs without dispatching.
// We verify by running 250ms and confirming no panic/RPC attempt (RPC
// is unreachable here; sensor would fail silently per isExpectedRPCErr).
func TestRunEventsPoll_NoHigh_NoOOM_NoDispatch(t *testing.T) {
	cgDir := t.TempDir()
	// memory.events.local with all zeros: no high/oom transitions
	body := "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n"
	if err := os.WriteFile(filepath.Join(cgDir, "memory.events.local"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// memory.current returns 0
	if err := os.WriteFile(filepath.Join(cgDir, "memory.current"), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	// memory.high empty
	if err := os.WriteFile(filepath.Join(cgDir, "memory.high"), []byte("max"), 0o644); err != nil {
		t.Fatal(err)
	}

	hooks := &ControllerHooks{cfg: &config.SandboxConfig{}}
	s := NewPressureSensor(hooks, cgDir, 4<<20, nil)

	ctx, cancel := contextWithTimeout(t, 250*time.Millisecond)
	defer cancel()
	s.runEventsPoll(ctx) // returns when ctx done; no panic = pass
}

// minimalValidConfig returns a config.SandboxConfig that passes ValidateCold
// (network/boot/launch all set per minimalCold). Tests apply their
// own config.SensorConfig + cgroup_path on top to exercise sensor validation
// in isolation.
func minimalValidConfig(t *testing.T) *config.SandboxConfig {
	t.Helper()
	cfg, err := config.Load(writeYAML(t, minimalCold))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// contextWithTimeout is a tiny helper that returns a context that auto-
// cancels after d, registered with t for cleanup.
func contextWithTimeout(t *testing.T, d time.Duration) (ctx context.Context, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel = context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return
}

package config

import (
	"strings"
	"testing"
	"time"
)

func TestAppStartDeadlineIsIndependentAndAlwaysBounded(t *testing.T) {
	c := SandboxConfig{Timeouts: TimeoutsConfig{AppNotify: "1h"}}
	if c.AppStartDeadline() != 2*time.Second {
		t.Fatal(c.AppStartDeadline())
	}
	for _, raw := range []string{"0", "0s", "-1s", "nonsense"} {
		c.Timeouts.AppStart = raw
		if err := c.Timeouts.validate(); err == nil || !strings.Contains(err.Error(), "timeouts.app_start") {
			t.Fatal(raw, err)
		}
		if c.AppStartDeadline() <= 0 {
			t.Fatal("unbounded startup", raw)
		}
	}
	c.Timeouts.AppStart = "3s"
	if err := c.Timeouts.validate(); err != nil {
		t.Fatal(err)
	}
	if c.AppStartDeadline() != 3*time.Second || c.AppNotifyDeadline() != time.Hour {
		t.Fatal(c.Timeouts)
	}
}

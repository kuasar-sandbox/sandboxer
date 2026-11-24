package sandbox

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestEnvironmentExitResultSeparatesGuestFaultFromCHStatus(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		fact                     *proto.EnvironmentExit
		physical, normalShutdown bool
		want                     string
	}{
		{name: "normal-self-poweroff", fact: &proto.EnvironmentExit{State: "stopped", Reason: "manager-stopping"}},
		{name: "normal-namespace-poweroff", fact: &proto.EnvironmentExit{State: "stopped", Code: -1, TermSignal: 2, Reason: "manager-stopping"}},
		{name: "observer-failure-with-CH-zero", fact: &proto.EnvironmentExit{State: "failed", Reason: "observer-expired"}, want: "observer-expired"},
		{name: "missing-natural-report", want: "without a terminal environment report"},
		{name: "missing-host-shutdown-report", normalShutdown: true, want: "without a terminal environment report"},
		{name: "physical-retirement", physical: true},
		{name: "physical-does-not-erase-known-failure", physical: true, fact: &proto.EnvironmentExit{State: "failed", Reason: "executor-EOF"}, want: "executor-EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := shutdownRuntimeFixture(func(context.Context) error { return nil })
			r.environmentActive = true
			if tc.fact != nil {
				if err := r.recordEnvironmentExit(*tc.fact); err != nil {
					t.Fatal(err)
				}
			}
			if tc.normalShutdown {
				if err := r.beginShutdown(time.Time{}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.physical {
				r.markPhysicalRetirement()
			}
			r.finishExit(ExitResult{Code: 0})
			result, err := r.Wait(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if result.Err != nil || result.Code != 0 {
					t.Fatalf("result=%+v", result)
				}
			} else if result.Err == nil || !strings.Contains(result.Err.Error(), tc.want) || result.Code != -1 {
				t.Fatalf("result=%+v want=%q", result, tc.want)
			}
		})
	}
}

func TestEnvironmentExitDuplicateAndContradiction(t *testing.T) {
	r := shutdownRuntimeFixture(func(context.Context) error { return nil })
	r.environmentActive = true
	fact := proto.EnvironmentExit{State: "failed", Reason: "executor-EOF"}
	if err := r.recordEnvironmentExit(fact); err != nil {
		t.Fatal(err)
	}
	if err := r.recordEnvironmentExit(fact); err != nil {
		t.Fatal("retry rejected", err)
	}
	if err := r.recordEnvironmentExit(proto.EnvironmentExit{State: "stopped", Reason: "manager-stopping"}); err == nil {
		t.Fatal("contradictory success accepted")
	}
	r.finishExit(ExitResult{})
	result, _ := r.Wait(context.Background())
	if result.Err == nil || !strings.Contains(result.Err.Error(), "executor-EOF") || !strings.Contains(result.Err.Error(), "conflicting") {
		t.Fatalf("failure overwritten: %+v", result)
	}
}

func TestEnvironmentExitLatePhysicalCloseCannotWaiveMissingFact(t *testing.T) {
	r := shutdownRuntimeFixture(func(context.Context) error { return nil })
	r.environmentActive = true
	reaped := make(chan struct{})
	close(reaped)
	r.reaped = reaped
	r.markPhysicalRetirement()
	r.finishExit(ExitResult{})
	if r.exit.Err == nil {
		t.Fatal("late Close erased an unreported natural exit")
	}
}

func TestFrozenRetirementDoesNotRequireEnvironmentExecution(t *testing.T) {
	r := shutdownRuntimeFixture(func(context.Context) error { t.Error("frozen retirement executed Guest shutdown"); return nil })
	r.environmentActive = true
	reaped := make(chan struct{})
	r.reaped = reaped
	signals := make(chan os.Signal, 1)
	released := make(chan struct{})
	retire := frozenRetirement(r, &channelSignaler{sent: signals}, nil, func() { close(released) }, reaped, func(string, ...any) {})
	done := make(chan error, 1)
	go func() { done <- retire() }()
	select {
	case sig := <-signals:
		if sig != syscall.SIGKILL {
			t.Fatalf("frozen source received %v", sig)
		}
	case <-time.After(time.Second):
		t.Fatal("frozen source was not retired")
	}
	select {
	case <-done:
		t.Fatal("retirement completed without real Wait")
	case <-released:
		t.Fatal("retirement released backing before Wait")
	default:
	}
	close(reaped)
	r.finishExit(ExitResult{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("retirement did not complete")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("retirement did not release backing")
	}
	if err := retire(); err != nil {
		t.Fatal(err)
	}
	select {
	case sig := <-signals:
		t.Fatalf("retirement repeated %v", sig)
	default:
	}
}

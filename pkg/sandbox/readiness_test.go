package sandbox

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
)

func TestReadinessEmitterOrdersAndDeduplicatesEvents(t *testing.T) {
	var got []ReadinessEvent
	e := newReadinessEmitter(func(event ReadinessEvent) { got = append(got, event) })
	e.notifyReady() // fail closed at the emitter boundary; do not reorder.
	e.notifyControlReady()
	e.notifyControlReady()
	e.notifyReady()
	e.notifyReady() // models a later in-place app restart.
	want := []ReadinessEvent{ReadinessControlReady, ReadinessReady}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestReadinessEmitterNilNotifierHasNoEffect(t *testing.T) {
	e := newReadinessEmitter(nil)
	e.notifyControlReady()
	e.notifyReady()
}

func TestReadinessEmitterConcurrentNotifications(t *testing.T) {
	var mu sync.Mutex
	var got []ReadinessEvent
	e := newReadinessEmitter(func(event ReadinessEvent) {
		mu.Lock()
		got = append(got, event)
		mu.Unlock()
	})
	e.notifyControlReady()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); e.notifyControlReady() }()
		go func() { defer wg.Done(); e.notifyReady() }()
	}
	wg.Wait()
	want := []ReadinessEvent{ReadinessControlReady, ReadinessReady}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestServeAndWaitEmitsControlReadyOnlyAfterCtlListen(t *testing.T) {
	runDir := t.TempDir()
	var got []ReadinessEvent
	_, err := ServeAndWait(VMParams{
		Ctx:        context.Background(),
		SandboxID:  "readiness-test",
		RunDir:     runDir,
		Logf:       func(string, ...any) {},
		CapBytes:   4096,
		UffdSource: uffd.ZeroSource{},
		LaunchSpec: &proto.LaunchSpec{},
		SnapCfg:    &config.SandboxConfig{},
		NotifyReadiness: func(event ReadinessEvent) {
			got = append(got, event)
			if event != ReadinessControlReady {
				t.Fatalf("unexpected event %q", event)
			}
			conn, dialErr := net.Dial("unix", filepath.Join(runDir, "ctl.sock"))
			if dialErr != nil {
				t.Fatalf("control_ready before ctl.sock became connectable: %v", dialErr)
			}
			_ = conn.Close()
		},
		BuildCmd: func(CmdEnv) (*exec.Cmd, func(), error) {
			return nil, func() {}, errors.New("injected build failure")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected build failure") {
		t.Fatalf("ServeAndWait error = %v, want injected build failure", err)
	}
	if !reflect.DeepEqual(got, []ReadinessEvent{ReadinessControlReady}) {
		t.Fatalf("events = %v, want [control_ready]", got)
	}
}

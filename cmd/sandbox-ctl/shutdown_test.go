package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
)

func TestShutdownCommandTargetsExactRun(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cmd-stop-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Mkdir(filepath.Join(dir, "path"), 0700); err != nil {
		t.Fatal(err)
	}
	target := ctl.ShutdownTarget{SandboxID: "sandbox", RunID: "run"}
	s := &ctl.Server{Path: filepath.Join(dir, "path", "ctl.sock"), ShutdownHandler: func(req ctl.ShutdownRequest) (ctl.ShutdownOperation, error) {
		if req.Target != target {
			return ctl.ShutdownOperation{}, ctl.ErrShutdownTargetMismatch
		}
		return ctl.ShutdownOperation{Begin: func() (int64, error) { return ctl.MonotonicDeadline(time.Now().Add(time.Second)) }, Wait: func() error { return nil }}, nil
	}}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()
	go s.Serve(ctx)
	args := []string{"--sandbox-id", "sandbox", "--run-id", "run", "--path-id", "path", "--run-root", dir}
	if err := runShutdown(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	args[3] = "stale"
	if err := runShutdown(context.Background(), args); !errors.Is(err, ctl.ErrShutdownTargetMismatch) {
		t.Fatalf("stale generation err=%v", err)
	}
}

func TestShutdownCommandRejectsMissingIdentityAndInvalidTimeout(t *testing.T) {
	for _, args := range [][]string{{}, {"--sandbox-id", "sandbox"}, {"--sandbox-id", "sandbox", "--run-id", "../bad"}, {"--sandbox-id", "sandbox", "--run-id", "run", "--timeout", "-1s"}} {
		if err := runShutdown(context.Background(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

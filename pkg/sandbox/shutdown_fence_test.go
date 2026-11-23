package sandbox

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestShutdownFenceBoundsBlockedVMMAPIAndWaitsForReaper(t *testing.T) {
	for _, secondSignal := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "second-signal"}[secondSignal], func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "stop-fence-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			sock := filepath.Join(dir, "ch.sock")
			if err := os.WriteFile(filepath.Join(dir, "memory.high"), []byte("max"), 0600); err != nil {
				t.Fatal(err)
			}
			l, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			request := make(chan struct{})
			release := make(chan struct{})
			server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(request); <-release })}
			go server.Serve(l)
			defer func() { close(release); server.Close() }()
			done := make(chan error, 1)
			signals := make(chan os.Signal, 2)
			sent := make(chan os.Signal, 8)
			proc := &channelSignaler{sent: sent}
			waited := make(chan error, 1)
			deadline := time.Now().Add(150 * time.Millisecond)
			if secondSignal {
				deadline = time.Now().Add(5 * time.Second)
			}
			go func() {
				waited <- waitForCHWithSignalEscalation(done, signals, proc, 1, sock, dir, time.Minute, 5*time.Second, discardLogf, func() time.Time { return deadline })
			}()
			signals <- syscall.SIGTERM
			select {
			case <-request:
			case <-time.After(time.Second):
				t.Fatal("VMM API request missing")
			}
			start := time.Now()
			if secondSignal {
				signals <- syscall.SIGTERM
			}
			killed := false
			for !killed {
				select {
				case sig := <-sent:
					killed = sig == syscall.SIGKILL
				case <-time.After(time.Second):
					t.Fatal("blocked API prevented SIGKILL")
				}
			}
			if secondSignal && time.Since(start) > time.Second/2 {
				t.Fatal("second signal waited for API timeout")
			}
			select {
			case err := <-waited:
				t.Fatalf("SIGKILL substituted for actual Wait: %v", err)
			default:
			}
			done <- nil
			select {
			case err := <-waited:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("actual reaper completion ignored")
			}
		})
	}
}

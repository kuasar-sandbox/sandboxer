package guestlink

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
)

// MUXLink holds the live stdio MUX session and its bridge-cleanup func —
// the host-side counterpart of the guest's consoleBridge. Exactly one
// stdio MUX exists at a time, but which connection backs it changes across
// cold-start launch → restore → attach. MUXLink guards the pair so a
// snapshot handler (running on the ctl.sock server's goroutine) can swap
// in a fresh session via Reattach without racing the lifecycle's teardown.
//
// The zero value is an empty link (no session).
type MUXLink struct {
	mu      sync.Mutex
	sess    *mux.Session
	cleanup func()
}

// Set records the current session and its bridge cleanup. Called once
// after the initial bridge (cold start: LaunchServer.OnMUXReady; restore:
// after OpenMUXViaRestore) and again after each successful Reattach. Any
// previously held pair is replaced without cleanup — callers Teardown
// first (Reattach does).
func (l *MUXLink) Set(sess *mux.Session, cleanup func()) {
	l.mu.Lock()
	l.sess, l.cleanup = sess, cleanup
	l.mu.Unlock()
}

// Teardown closes the current MUX connection (unblocking the bridge pumps)
// and runs its cleanup (restores the terminal in tty mode, drains the
// guest→host pumps), then clears the link. Idempotent; a no-op when empty.
func (l *MUXLink) Teardown() {
	l.mu.Lock()
	s, c := l.sess, l.cleanup
	l.sess, l.cleanup = nil, nil
	l.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
	if c != nil {
		c()
	}
}

// Reattach tears down the current session, then re-establishes a fresh
// stdio MUX via an `attach` op against the guest's reverse-channel
// listener and bridges its streams onto mode's host stdio under ctx,
// recording the new pair on success. On error the link is left empty (the
// old session is already gone).
//
// Used after `snapshot --resume` (the guest closed the MUX during quiesce)
// and as sandbox-ctl's own reliability fallback when a MUX breaks mid-run.
// Note: in tty mode the new bridge re-acquires the terminal; a keystroke
// arriving in the brief window before the old stdin-copy goroutine has
// noticed the close may be dropped (it self-heals after one byte).
func (l *MUXLink) Reattach(ctx context.Context, client *HostClient, mode stdio.Mode) error {
	l.Teardown()
	var attemptErrors []error
	for attempt := 1; attempt <= 2; attempt++ {
		conn, established, err := OpenMUXViaAttachContext(ctx, client, uint32(attempt), proto.DeadlineAttach)
		if err == nil {
			ss := stdio.StreamSetFor(established)
			sess := mux.NewSession(conn, ss, mux.Options{})
			cleanup, bridgeErr := mode.Bridge(ctx, sess, ss)
			if bridgeErr == nil {
				l.Set(sess, cleanup)
				return nil
			}
			_ = sess.Close()
			err = bridgeErr
		}
		attemptErrors = append(attemptErrors, err)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(append(attemptErrors, ctxErr)...)
		}
		if attempt == 1 && client != nil && client.Logf != nil {
			client.Logf("attach handshake failed after request; retrying once to complete idempotent post-quiesce recovery: %v", err)
		}
	}
	return fmt.Errorf("attach recovery failed after two attempts: %w", errors.Join(attemptErrors...))
}

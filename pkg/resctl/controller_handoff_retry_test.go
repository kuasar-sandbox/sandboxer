package resctl

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestControllerRetainedRetry(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		name := "lost-ack"
		if rejected {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var attempts []resource.Message
			revision, lookups := uint64(1), 0
			var server *reservationServer
			server = startReservationServer(t, func(req resource.Message) (*resource.Message, bool) {
				mu.Lock()
				defer mu.Unlock()
				switch req.Type {
				case resource.TypeGetRetainedAccount:
					lookups++
					return &resource.Message{Type: resource.TypeAck,
						RetainedAccount: &resource.RetainedAccount{ID: "account", Revision: revision},
						ClientFeatures:  []string{resource.FeatureRetainedMemoryV2}}, false
				case resource.TypeAdoptRetained:
					attempts = append(attempts, req)
					if len(attempts) == 1 || rejected {
						// A definitive rejection precedes acceptance. An ACK loss
						// instead happens after the real lease has been published.
						if rejected && len(attempts) == 1 {
							revision++
							return &resource.Message{Type: resource.TypeAdmitResponse,
								Status: resource.StatusRejected, Reason: resource.ReasonRetentionAccountChanged,
								ClientFeatures: []string{resource.FeatureRetainedMemoryV2}}, false
						}
						_, pid, identity, err := resource.InspectLeaseCandidate(server.sock, req.SandboxID, req.LeaseCandidate)
						if err == nil {
							err = resource.PublishLeaseCandidate(server.sock, req.SandboxID, req.LeaseCandidate, identity, pid)
						}
						if err != nil {
							t.Errorf("publish candidate: %v", err)
							return &resource.Message{Type: resource.TypeError, Msg: err.Error()}, false
						}
					}
					if !rejected && len(attempts) == 1 {
						// Settlement changes the live inventory before reconnect.
						revision++
						return nil, true
					}
					return &resource.Message{Type: resource.TypeAdmitResponse,
						Status: resource.StatusAdmitted, Token: "adopted", GrantedInitialAlloc: 320 << 20,
						ClientFeatures: []string{resource.FeatureRetainedMemoryV2}}, false
				default:
					return &resource.Message{Type: resource.TypeAck}, false
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cfg := testControllerConfig(t, server.sock)
			hooks, err := NewControllerHooks(ControllerHookOptions{
				SocketPath: server.sock, SandboxID: "sandbox-1", Context: ctx,
				CheckpointAccount: true,
			}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer hooks.Release("test")
			if granted, err := hooks.Admit("sandbox-1", 320<<20); err != nil || granted != 320<<20 {
				t.Fatalf("admit after retry: budget=%d err=%v", granted, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(attempts) != 2 || attempts[0].RetainedAccount == nil || attempts[1].RetainedAccount == nil {
				t.Fatalf("expected two retained handoff attempts, got %d", len(attempts))
			}
			wantLookups := 1
			if rejected {
				wantLookups = 2
				if attempts[1].RetainedAccount.Revision != revision {
					t.Fatal("definitive rejection did not refresh the account")
				}
				// Only the rejected reference changes; the candidate remains ours.
				attempts[1].RetainedAccount = attempts[0].RetainedAccount
			}
			if !reflect.DeepEqual(attempts[0], attempts[1]) {
				t.Fatal("uncertain retry changed the accepted handoff contract")
			}
			if lookups != wantLookups {
				t.Fatalf("account lookups=%d, want %d", lookups, wantLookups)
			}
		})
	}
}

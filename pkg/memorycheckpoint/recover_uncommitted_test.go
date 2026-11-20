package memorycheckpoint

import (
	"context"
	"encoding/hex"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRecoverAbandonedPreparation(t *testing.T) {
	for _, gate := range []string{"dead", "writer", "live-producer", "reader", "nonce"} {
		t.Run(gate, func(t *testing.T) {
			p := startRecoveryProcess(t, "uncommitted")
			// A live writer always excludes recovery, regardless of its phase.
			got, err := RecoverMemoryRelease(context.Background(), p.path, p.record)
			if err != nil || got.Record.Aborted || got.ProducerExited {
				t.Fatalf("live writer changed preparation: %+v %v", got, err)
			}
			if gate == "live-producer" {
				// Even free writer/RAM locks cannot replace exact PID evidence.
				p.request(t, "closed")
				p.request(t, "unlock")
				got, err = RecoverMemoryRelease(context.Background(), p.path, p.record)
				if err != nil || got.Record.Aborted || got.ProducerExited {
					t.Fatalf("live process changed preparation: %+v %v", got, err)
				}
			}
			var pin *os.File
			if gate == "reader" {
				pin, err = openRAMPin(p.path, p.record.Holder.RAMLock, unix.LOCK_SH)
				if err != nil {
					t.Fatal(err)
				}
				defer pin.Close()
			}
			p.kill(t)
			switch gate {
			case "writer":
				writer, err := LockPreparation(p.dir, p.record.CheckpointID)
				if err != nil {
					t.Fatal(err)
				}
				got, err = RecoverMemoryRelease(context.Background(), p.path, p.record)
				if err != nil || got.Record.Aborted {
					t.Fatalf("writer fence bypassed: %+v %v", got, err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			case "reader":
				got, err = RecoverMemoryRelease(context.Background(), p.path, p.record)
				if err != nil || !got.ProducerExited || got.Record.Aborted {
					t.Fatalf("borrowed RAM was aborted: %+v %v", got, err)
				}
				if err := pin.Close(); err != nil {
					t.Fatal(err)
				}
			case "nonce":
				if err := os.WriteFile(p.path+".ram.lock", make([]byte, 32), 0600); err != nil {
					t.Fatal(err)
				}
				if got, err := RecoverMemoryRelease(context.Background(), p.path, p.record); err == nil || got.Record.Aborted {
					t.Fatalf("replacement nonce authorized abort: %+v %v", got, err)
				}
				original, err := hex.DecodeString(p.record.Holder.RAMLock.Nonce)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.path+".ram.lock", original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err = RecoverMemoryRelease(context.Background(), p.path, p.record)
			if err != nil || !got.ProducerExited || !got.Record.Aborted {
				t.Fatalf("dead preparation did not abort: %+v %v", got, err)
			}
			if got.Record.Committed || got.Record.RuntimeRetired || got.Record.RAMReleased || got.Record.FileReady || got.Record.Discarded {
				t.Fatalf("abort invented retired runtime or payload facts: %+v", got.Record)
			}
			durable, err := ReadCaptureRecord(p.path)
			if err != nil || !durable.Aborted {
				t.Fatalf("abort was not durable: %+v %v", durable, err)
			}
		})
	}
}

func TestRecoveryKeepsCommittedUnretiredCaptureUnconfirmed(t *testing.T) {
	p := startRecoveryProcess(t, "committed")
	p.kill(t)
	got, err := RecoverMemoryRelease(context.Background(), p.path, p.record)
	if err != nil || !got.ProducerExited || got.Record != p.record {
		t.Fatalf("unconfirmed runtime retirement was changed: %+v %v", got, err)
	}
}

package snapshot

import (
	"os"
	"strings"
	"testing"
)

func TestCheckpointCandidateExactProducerNamesAndTypes(t *testing.T) {
	digest := strings.Repeat("a", 64)
	sid := "sandbox.one"
	for _, kind := range []string{"snapshot", "sandbox", "overlay", "image", "bundle"} {
		for _, name := range []string{digest + "." + kind, sid + "." + kind + ".0.partial", sid + "." + kind + ".4294967295.partial"} {
			if !CheckpointCandidate(name, sid, 0, "") {
				t.Errorf("producer regular %q rejected", name)
			}
			for _, mode := range []os.FileMode{os.ModeDir, os.ModeSymlink, os.ModeNamedPipe, os.ModeSocket} {
				if CheckpointCandidate(name, sid, mode, digest+".snapshot") {
					t.Errorf("unexpected type %v accepted for %q", mode, name)
				}
			}
		}
		for _, name := range []string{strings.Repeat("A", 64) + "." + kind, strings.Repeat("a", 63) + "." + kind, digest + "x." + kind,
			sid + "." + kind + ".01.partial", sid + "." + kind + ".4294967296.partial", sid + "." + kind + ".+1.partial", sid + "." + kind + ".x.partial", sid + "." + kind + "..partial",
			sid + "2." + kind + ".1.partial", "other." + kind + ".1.partial", sid + "." + kind + ".1.partial.more"} {
			if CheckpointCandidate(name, sid, 0, "") {
				t.Errorf("lookalike %q accepted", name)
			}
		}
	}
	for _, role := range []string{"snapshot", "sandbox"} {
		for _, name := range []string{sid + "." + role, "." + sid + "." + role + "." + strings.Repeat("b", 32) + ".tmp"} {
			for _, target := range []string{digest + "." + role, digest + ".bundle"} {
				if !CheckpointCandidate(name, sid, os.ModeSymlink, target) {
					t.Errorf("producer alias %q -> %q rejected", name, target)
				}
				if CheckpointCandidate(name, sid, 0, target) {
					t.Errorf("regular alias %q accepted", name)
				}
			}
			for _, target := range []string{"../" + digest + "." + role, "/tmp/" + digest + "." + role, "user-file", digest + ".overlay"} {
				if CheckpointCandidate(name, sid, os.ModeSymlink, target) {
					t.Errorf("unexpected alias %q -> %q accepted", name, target)
				}
			}
		}
	}
	for _, name := range []string{".sandbox.one.snapshot.123.tmp", ".sandbox.one.snapshot." + strings.Repeat("B", 32) + ".tmp", ".sandbox.one2.snapshot." + strings.Repeat("a", 32) + ".tmp", "notes.tmp", "notes.snapshot"} {
		if CheckpointCandidate(name, sid, os.ModeSymlink, digest+".snapshot") {
			t.Errorf("unknown %q accepted", name)
		}
	}
}

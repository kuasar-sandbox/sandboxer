package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
)

type componentSink struct {
	bytes.Buffer
	closes int
}

func (s *componentSink) Close() error { s.closes++; return nil }

func TestComponentLogDefaultAndInvalidPreserveOutput(t *testing.T) {
	previous := log.Writer()
	for _, target := range []string{"", "default"} {
		writer, close, err := componentLog(target)
		if err != nil || writer != os.Stderr || log.Writer() != previous {
			t.Fatalf("default changed output: %v", err)
		}
		close()
	}
	for _, target := range []string{"file=/tmp/component.log", "journald=", "journald=ctl,MESSAGE=x"} {
		if _, _, err := componentLog(target); err == nil {
			t.Fatalf("accepted %q", target)
		}
		if log.Writer() != previous {
			t.Fatal("invalid target changed logger")
		}
	}
}

func TestComponentLoggerPreservesTextFlagsAndRestores(t *testing.T) {
	previous, flags, prefix := log.Writer(), log.Flags(), log.Prefix()
	sink := &componentSink{}
	close := installComponentLogger(sink)
	defer close()
	fmt.Fprintln(log.Writer(), "direct diagnostic")
	log.Print("standard diagnostic")
	if !strings.HasPrefix(sink.String(), "direct diagnostic\n") || !strings.Contains(sink.String(), "standard diagnostic\n") {
		t.Fatalf("missing diagnostic: %q", sink.String())
	}
	if log.Flags() != flags || log.Prefix() != prefix {
		t.Fatal("installation changed log format")
	}
	close()
	close()
	if log.Writer() != previous || sink.closes != 1 {
		t.Fatal("logger cleanup did not restore exactly once")
	}
}

func TestComponentLogExplicitNeedsNeitherJournalStderrNorIdentity(t *testing.T) {
	previous := log.Writer()
	writer, close, err := componentLog("journald=ctl,WORKLOAD_ID=w-1")
	if err != nil {
		t.Fatal(err)
	}
	if writer == os.Stderr || log.Writer() != writer {
		t.Fatal("explicit target not installed")
	}
	// No bytes are sent in this constructor test; native availability is not
	// required to select an explicit target.
	close()
	if log.Writer() != previous {
		t.Fatal("not restored")
	}
}

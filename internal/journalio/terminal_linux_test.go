package journalio

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTerminalFallbackPTYModeTransitions(t *testing.T) {
	master, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(master)
	if err := unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(master, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	state, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	w, err := newWriter(Target{Tag: "console"}, terminalFallback(slave), "[console] ", func(string, map[string]string) error {
		return errors.New("journal unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, raw := range []bool{false, true, false} {
		mode := *state
		mode.Oflag |= unix.OPOST | unix.ONLCR
		if raw {
			mode.Oflag &^= unix.OPOST
		}
		if err := unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, &mode); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("line\n")); err != nil {
			t.Fatal(err)
		}
		want := []byte("[console] line\r\n")
		var got []byte
		deadline := time.Now().Add(2 * time.Second)
		for len(got) < len(want) && time.Now().Before(deadline) {
			buf := make([]byte, 128)
			n, err := unix.Read(master, buf)
			if err == unix.EAGAIN || err == unix.EINTR {
				time.Sleep(time.Millisecond)
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, buf[:n]...)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("raw=%t got=%q want=%q", raw, got, want)
		}
	}
}

func TestTerminalFallbackFileIsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fallback")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	input := []byte("line\nexisting\r\n")
	if _, err := terminalFallback(file).Write(input); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, input) {
		t.Fatalf("regular-file fallback changed: %q, %v", got, err)
	}
}

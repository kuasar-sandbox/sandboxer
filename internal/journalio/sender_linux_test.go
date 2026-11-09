package journalio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The receiver deliberately does not use the real system journal. Keeping its
// queue full exercises the kernel's Unix datagram readiness behavior, which a
// mock returning EAGAIN cannot reproduce.
type nativeJournalReceiver struct {
	path    string
	fd      int
	fillers []int
	payload []byte
	control []byte
}

func listenNativeJournal(t *testing.T, path string) *nativeJournalReceiver {
	t.Helper()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	r := &nativeJournalReceiver{
		path: path, fd: fd,
		payload: make([]byte, 256<<10),
		control: make([]byte, unix.CmsgSpace(4)),
	}
	t.Cleanup(r.close)
	return r
}

func newNativeJournalReceiver(t *testing.T) *nativeJournalReceiver {
	t.Helper()
	// Short paths also work when the caller has a long subtest name.
	dir, err := os.MkdirTemp("", "journal-send-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return listenNativeJournal(t, filepath.Join(dir, "socket"))
}

func (r *nativeJournalReceiver) close() {
	if r.fd >= 0 {
		_ = unix.Close(r.fd)
		r.fd = -1
	}
	for _, fd := range r.fillers {
		_ = unix.Close(fd)
	}
	r.fillers = nil
}

func (r *nativeJournalReceiver) fill(t *testing.T) {
	t.Helper()
	// A sender's own buffer can fill before the receiver's queue. Only an
	// EAGAIN from a fresh sender, which has queued no bytes, proves the latter.
	for count := 0; count < 20000; {
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		r.fillers = append(r.fillers, fd)
		sent := 0
		for count < 20000 {
			err := unix.Sendto(fd, []byte{'x'}, unix.MSG_DONTWAIT, &unix.SockaddrUnix{Name: r.path})
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EAGAIN) {
				if sent == 0 {
					return
				}
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			sent++
			count++
		}
	}
	t.Fatal("receiver queue did not fill within the isolated test's message bound")
}

func (r *nativeJournalReceiver) receive(t *testing.T) ([]byte, bool) {
	t.Helper()
	payload, control := r.payload, r.control
	deadline := time.Now().Add(3 * time.Second)
	for {
		n, controlN, flags, _, err := unix.Recvmsg(r.fd, payload, control, unix.MSG_DONTWAIT|unix.MSG_CMSG_CLOEXEC)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) {
			if time.Now().After(deadline) {
				t.Fatal("timed out receiving journal record")
			}
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
			t.Fatalf("truncated journal record: flags=%#x", flags)
		}
		if controlN == 0 {
			return payload[:n], false
		}
		messages, err := unix.ParseSocketControlMessage(control[:controlN])
		if err != nil {
			t.Fatal(err)
		}
		var fds []int
		for _, message := range messages {
			rights, err := unix.ParseUnixRights(&message)
			if err != nil {
				for _, fd := range fds {
					_ = unix.Close(fd)
				}
				t.Fatal(err)
			}
			fds = append(fds, rights...)
		}
		if len(fds) != 1 || n != 0 {
			for _, fd := range fds {
				_ = unix.Close(fd)
			}
			t.Fatalf("invalid large-entry envelope: data=%d, descriptors=%d", n, len(fds))
		}
		seals, err := unix.FcntlInt(uintptr(fds[0]), unix.F_GET_SEALS, 0)
		const requiredSeals = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
		if err != nil || seals&requiredSeals != requiredSeals {
			_ = unix.Close(fds[0])
			t.Fatalf("large entry was not sealed before handoff: seals=%#x, err=%v", seals, err)
		}
		file := os.NewFile(uintptr(fds[0]), "journal-entry")
		_, seekErr := file.Seek(0, io.SeekStart)
		data, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if err := errors.Join(seekErr, readErr, closeErr); err != nil {
			t.Fatal(err)
		}
		return data, true
	}
}

func nativeJournalFields(t *testing.T, data []byte) map[string]string {
	t.Helper()
	fields := make(map[string]string)
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			t.Fatal("journal field header has no terminating newline")
		}
		header := data[:end]
		data = data[end+1:]
		key, value, text := bytes.Cut(header, []byte{'='})
		if !text {
			if len(data) < 8 {
				t.Fatal("journal binary field has no length")
			}
			size := binary.LittleEndian.Uint64(data[:8])
			data = data[8:]
			if size >= uint64(len(data)) || data[size] != '\n' {
				t.Fatal("invalid journal binary field length or terminator")
			}
			value, data = data[:size], data[size+1:]
		}
		if _, exists := fields[string(key)]; exists {
			t.Fatalf("duplicate journal field %q", key)
		}
		fields[string(key)] = string(value)
	}
	return fields
}

type nativeJournalFallback struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (f *nativeJournalFallback) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data.Write(p)
}

func (f *nativeJournalFallback) text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data.String()
}

func TestJournalBackpressureDoesNotBlockDiagnostics(t *testing.T) {
	r := newNativeJournalReceiver(t)
	fallback := &nativeJournalFallback{}
	stream, err := newJournalWriter(Target{Tag: "guest"}, fallback, "", r.path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Release any blocked send before taking the writer's Close lock,
		// including when an assertion failed while the queue was still full.
		r.close()
		_ = stream.Close()
	})
	if _, err := stream.Write([]byte("warmup\n")); err != nil {
		t.Fatal(err)
	}
	r.receive(t)
	r.fill(t)

	streamDone := make(chan error, 1)
	go func() {
		n, err := stream.Write([]byte("preserve this output\n"))
		if err == nil && n != len("preserve this output\n") {
			err = io.ErrShortWrite
		}
		streamDone <- err
	}()
	select {
	case err := <-streamDone:
		t.Fatalf("full queue did not retain stream backpressure: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if got := fallback.text(); got != "" {
		t.Fatalf("congestion prematurely sent business output to fallback: %q", got)
	}

	diagnosticFallback := &nativeJournalFallback{}
	diagnostic, err := newJournalWriter(Target{Tag: "ctl"}, diagnosticFallback, "", r.path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.close(); _ = diagnostic.Close() })
	diagnosticDone := make(chan error, 1)
	go func() {
		if _, err := diagnostic.Write([]byte("diagnostic\npartial tail")); err != nil {
			diagnosticDone <- err
			return
		}
		diagnosticDone <- diagnostic.Close()
	}()
	select {
	case err := <-diagnosticDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("diagnostic write/close waited for the congested receiver or another output")
	}
	if got := diagnosticFallback.text(); got != "" {
		t.Fatalf("diagnostic congestion entered an unbounded fallback: %q", got)
	}

	for {
		data, _ := r.receive(t)
		if bytes.Equal(data, []byte{'x'}) {
			continue
		}
		fields := nativeJournalFields(t, data)
		if fields["MESSAGE"] != "preserve this output" || fields["SYSLOG_IDENTIFIER"] != "guest" {
			t.Fatalf("wrong output after receiver resumed: %#v", fields)
		}
		break
	}
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not resume after receiver drained")
	}
	if got := fallback.text(); got != "" {
		t.Fatalf("delivered output was duplicated to fallback: %q", got)
	}
}

func TestNativeJournalReceiverReplacement(t *testing.T) {
	for _, diagnostic := range []bool{false, true} {
		t.Run(fmt.Sprintf("diagnostic=%t", diagnostic), func(t *testing.T) {
			r := newNativeJournalReceiver(t)
			sender := newNativeSender(r.path, diagnostic)
			t.Cleanup(func() { r.close(); _ = sender.close() })
			if err := sender.send("before", map[string]string{"SYSLOG_IDENTIFIER": "test"}); err != nil {
				t.Fatal(err)
			}
			r.receive(t)
			r.close()
			if err := os.Remove(r.path); err != nil {
				t.Fatal(err)
			}
			if err := sender.send("while unavailable", nil); err == nil {
				t.Fatal("send to absent journal unexpectedly succeeded")
			}
			replacement := listenNativeJournal(t, r.path)
			if err := sender.send("after", map[string]string{"SYSLOG_IDENTIFIER": "test"}); err != nil {
				t.Fatal("sender did not recover after journal returned:", err)
			}
			data, _ := replacement.receive(t)
			if fields := nativeJournalFields(t, data); fields["MESSAGE"] != "after" {
				t.Fatalf("wrong recovered message: %#v", fields)
			}
		})
	}
}

func TestJournalFirstMessageAfterReceiverReplacement(t *testing.T) {
	for _, diagnostic := range []bool{false, true} {
		t.Run(fmt.Sprintf("diagnostic=%t", diagnostic), func(t *testing.T) {
			r := newNativeJournalReceiver(t)
			fallback := &nativeJournalFallback{}
			w, err := newJournalWriter(Target{Tag: "test"}, fallback, "", r.path, diagnostic)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.close(); _ = w.Close() })
			if _, err := w.Write([]byte("before\n")); err != nil {
				t.Fatal(err)
			}
			data, _ := r.receive(t)
			if got := nativeJournalFields(t, data)["MESSAGE"]; got != "before" {
				t.Fatalf("warmup message=%q", got)
			}

			r.close()
			if err := os.Remove(r.path); err != nil {
				t.Fatal(err)
			}
			replacement := listenNativeJournal(t, r.path)
			// No send during the outage: the stream still holds its old peer.
			// A preliminary failure must not be needed to reconnect it, or the
			// first user record after each journal restart goes to fallback.
			const first = "first after restart\n"
			if n, err := w.Write([]byte(first)); err != nil || n != len(first) {
				t.Fatalf("first Write=%d, %v", n, err)
			}
			if got := fallback.text(); got != "" {
				t.Fatalf("available replacement journal lost the first record to fallback: %q", got)
			}
			data, _ = replacement.receive(t)
			fields := nativeJournalFields(t, data)
			if fields["MESSAGE"] != "first after restart" || fields["SYSLOG_IDENTIFIER"] != "test" {
				t.Fatalf("wrong first record after restart: %#v", fields)
			}
			var extra [1]byte
			if _, _, err := unix.Recvfrom(replacement.fd, extra[:], unix.MSG_DONTWAIT); !errors.Is(err, unix.EAGAIN) {
				t.Fatalf("reconnect duplicated the record or failed to drain the receiver: %v", err)
			}
		})
	}
}

func TestNativeJournalBinaryFieldsAndLargeEntries(t *testing.T) {
	for _, diagnostic := range []bool{false, true} {
		t.Run(fmt.Sprintf("diagnostic=%t", diagnostic), func(t *testing.T) {
			r := newNativeJournalReceiver(t)
			sender := newNativeSender(r.path, diagnostic)
			t.Cleanup(func() { r.close(); _ = sender.close() })
			fields := map[string]string{"SYSLOG_IDENTIFIER": "test", "MULTILINE": "first\nsecond\x00third\n"}
			message := "first line\nsecond line"
			if err := sender.send(message, fields); err != nil {
				t.Fatal(err)
			}
			data, byFD := r.receive(t)
			got := nativeJournalFields(t, data)
			if byFD || got["MESSAGE"] != message || got["MULTILINE"] != fields["MULTILINE"] || got["PRIORITY"] != "6" || got["SYSLOG_IDENTIFIER"] != "test" {
				t.Fatalf("native fields changed: byFD=%t, fields=%#v", byFD, got)
			}

			// Force the same oversized-datagram error as a small configured
			// host send buffer, without changing any system-wide limits.
			raw, err := sender.conn.SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			var setErr error
			if err := raw.Control(func(fd uintptr) { setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF, 4096) }); err != nil {
				t.Fatal(err)
			}
			if setErr != nil {
				t.Fatal(setErr)
			}
			message = strings.Repeat("large entry\x00", 4096)
			if err := sender.send(message, fields); err != nil {
				t.Fatal("large-entry descriptor send:", err)
			}
			data, byFD = r.receive(t)
			got = nativeJournalFields(t, data)
			if !byFD || got["MESSAGE"] != message || got["MULTILINE"] != fields["MULTILINE"] {
				t.Fatalf("large entry changed: byFD=%t, message bytes=%d", byFD, len(got["MESSAGE"]))
			}
		})
	}
}

func TestDiagnosticConcurrentWriteCloseWhileCongested(t *testing.T) {
	r := newNativeJournalReceiver(t)
	r.fill(t)
	fallback := &nativeJournalFallback{}
	w, err := newJournalWriter(Target{Tag: "ctl"}, fallback, "", r.path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.close(); _ = w.Close() })
	if _, err := w.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	problems := make(chan error, 9)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 32; j++ {
				n, err := w.Write([]byte("diagnostic\n"))
				if errors.Is(err, io.ErrClosedPipe) {
					return
				}
				if err != nil || n != len("diagnostic\n") {
					problems <- fmt.Errorf("Write=%d, %v", n, err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if err := w.Close(); err != nil {
			problems <- err
		}
	}()
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent diagnostic Write/Close waited for receiver")
	}
	close(problems)
	for err := range problems {
		t.Error(err)
	}
	if got := fallback.text(); got != "" {
		t.Fatalf("diagnostics entered fallback while congested: %q", got)
	}
}

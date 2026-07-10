// Package stdio resolves `sandbox-ctl run`'s stdio model and bridges it
// to the guest. Two concerns (docs/sandbox.md §2.2, docs/sandbox-runtime
// .md §3.5 / §4.5):
//
//  1. The application's stdin/stdout/stderr — or a single pty in tty
//     mode — travel over the vsock stdio MUX (pkg/mux). The host
//     side: in tty mode put the controlling terminal in raw mode and
//     bridge it byte-for-byte with the PTY stream (+ SIGWINCH →
//     SET_WINSIZE); in pipe mode copy stdin → STDIN stream and STDOUT /
//     STDERR streams → the host's stdout/stderr (or files), per
//     --stdin / --stdout / --stderr and their -from/-to variants.
//
//  2. The guest *kernel* dmesg goes a separate way: CH (`--console tty`)
//     writes it to CH's own stdout, and sandbox-ctl points CH's stdout
//     at /dev/null (--console off), its own stderr (--console default),
//     or a file (--console file=PATH). CH's stdin is /dev/null so its
//     `--console tty` never raw-izes a terminal.
//
// Flags:
//
//	--tty                          (bool; default = auto: stdin&stdout both ttys)
//	--stdin / --stdout / --stderr  (bool; pipe mode; default false / true / true)
//	--stdin-from / --stdout-to / --stderr-to FILE
//	--console off | default | file=PATH    (default: default)
package stdio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/coreos/go-systemd/v22/journal"
	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

// --- resolved configuration -----------------------------------------

// StreamKind selects how one app data stream is wired host-side.
type StreamKind int

const (
	StreamNone     StreamKind = iota // no channel (guest wires the app fd to /dev/null for stdin)
	StreamInherit                    // os.Stdin / os.Stdout / os.Stderr
	StreamFile                       // open Path
	StreamJournald                   // line-write to journald, SYSLOG_IDENTIFIER=Tag
)

// Stream is one resolved app data-stream endpoint (pipe mode only).
type Stream struct {
	Kind StreamKind
	Path string // when Kind == StreamFile
	Tag  string // when Kind == StreamJournald (SYSLOG_IDENTIFIER)
}

func (s Stream) active() bool { return s.Kind != StreamNone }

// ConsoleKind selects where the guest kernel dmesg goes.
type ConsoleKind int

const (
	ConsoleStderr   ConsoleKind = iota // sandbox-ctl's stderr (default)
	ConsoleOff                         // discarded (CH --console off)
	ConsoleFile                        // a file
	ConsoleJournald                    // line-write to journald, SYSLOG_IDENTIFIER=Tag
)

// Console is the resolved kernel-dmesg sink.
type Console struct {
	Kind ConsoleKind
	Path string // when Kind == ConsoleFile
	Tag  string // when Kind == ConsoleJournald (SYSLOG_IDENTIFIER)
}

// Mode is the fully resolved stdio configuration for one run.
type Mode struct {
	TTY     bool // pty mode: app gets a real pty; sandbox-ctl's terminal goes raw
	Stdin   Stream
	Stdout  Stream
	Stderr  Stream
	Console Console
}

// Defaults: pipe mode, stdin off, stdout/stderr inherited, console→stderr.
var Defaults = Mode{
	Stdin:   Stream{Kind: StreamNone},
	Stdout:  Stream{Kind: StreamInherit},
	Stderr:  Stream{Kind: StreamInherit},
	Console: Console{Kind: ConsoleStderr},
}

// FromFlags resolves CLI flags into a validated Mode.
//
// ttyFlag is tri-state: nil = auto-detect (TTY iff stdin and stdout are
// both terminals); &true = force tty mode (requires both to be terminals,
// and conflicts with explicit pipe-mode flags); &false = force pipe mode.
// The other *bool flags are likewise tri-state (nil = unset).
func FromFlags(stdin, stdout, stderr *bool, stdinFrom, stdoutTo, stderrTo string, ttyFlag *bool, console string) (Mode, error) {
	c, err := parseConsole(console)
	if err != nil {
		return Mode{}, err
	}

	pipeFlagsGiven := stdin != nil || stdout != nil || stderr != nil ||
		stdinFrom != "" || stdoutTo != "" || stderrTo != ""

	tty := false
	switch {
	case ttyFlag != nil && *ttyFlag:
		if pipeFlagsGiven {
			return Mode{}, errors.New("--tty conflicts with explicit pipe-mode stdio flags")
		}
		if !IsTerminal(int(os.Stdin.Fd())) || !IsTerminal(int(os.Stdout.Fd())) {
			return Mode{}, errors.New("--tty requires stdin and stdout to be a terminal")
		}
		tty = true
	case ttyFlag != nil && !*ttyFlag:
		tty = false
	default: // auto
		tty = !pipeFlagsGiven && IsTerminal(int(os.Stdin.Fd())) && IsTerminal(int(os.Stdout.Fd()))
	}

	if tty {
		return Mode{TTY: true, Console: c}, nil
	}

	m := Defaults
	m.Console = c

	// stdin: default off; --stdin → inherit; --stdin-from → file.
	if stdin != nil && !*stdin && stdinFrom != "" {
		return Mode{}, errors.New("--stdin=false conflicts with --stdin-from")
	}
	switch {
	case stdinFrom != "":
		m.Stdin = Stream{Kind: StreamFile, Path: stdinFrom}
	case stdin != nil && *stdin:
		m.Stdin = Stream{Kind: StreamInherit}
	default: // nil or explicit false → off
		m.Stdin = Stream{Kind: StreamNone}
	}

	// stdout: default inherit; --stdout=false → off; --stdout-to → file|journald.
	if stdout != nil && !*stdout && stdoutTo != "" {
		return Mode{}, errors.New("--stdout=false conflicts with --stdout-to")
	}
	switch {
	case stdoutTo != "":
		if m.Stdout, err = parseStreamTarget(stdoutTo); err != nil {
			return Mode{}, err
		}
	case stdout != nil && !*stdout:
		m.Stdout = Stream{Kind: StreamNone}
	default:
		m.Stdout = Stream{Kind: StreamInherit}
	}

	// stderr: same shape as stdout.
	if stderr != nil && !*stderr && stderrTo != "" {
		return Mode{}, errors.New("--stderr=false conflicts with --stderr-to")
	}
	switch {
	case stderrTo != "":
		if m.Stderr, err = parseStreamTarget(stderrTo); err != nil {
			return Mode{}, err
		}
	case stderr != nil && !*stderr:
		m.Stderr = Stream{Kind: StreamNone}
	default:
		m.Stderr = Stream{Kind: StreamInherit}
	}
	return m, nil
}

// parseStreamTarget resolves a --stdout-to / --stderr-to value: "journald=<tag>"
// → a journald sink (SYSLOG_IDENTIFIER=tag); anything else → a file path.
func parseStreamTarget(to string) (Stream, error) {
	if tag, ok := strings.CutPrefix(to, "journald="); ok {
		if err := validTag(tag); err != nil {
			return Stream{}, err
		}
		return Stream{Kind: StreamJournald, Tag: tag}, nil
	}
	return Stream{Kind: StreamFile, Path: to}, nil
}

// validTag checks a journald SYSLOG_IDENTIFIER tag is a non-empty token.
func validTag(tag string) error {
	if tag == "" {
		return errors.New("journald= requires a tag")
	}
	for _, r := range tag {
		if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("journald tag %q: only [A-Za-z0-9_-] allowed", tag)
		}
	}
	return nil
}

func parseConsole(s string) (Console, error) {
	switch {
	case s == "" || s == "default":
		return Console{Kind: ConsoleStderr}, nil
	case s == "off":
		return Console{Kind: ConsoleOff}, nil
	case strings.HasPrefix(s, "file="):
		p := strings.TrimPrefix(s, "file=")
		if p == "" {
			return Console{}, errors.New("--console: file= requires a path")
		}
		return Console{Kind: ConsoleFile, Path: p}, nil
	case strings.HasPrefix(s, "journald="):
		tag := strings.TrimPrefix(s, "journald=")
		if err := validTag(tag); err != nil {
			return Console{}, err
		}
		return Console{Kind: ConsoleJournald, Tag: tag}, nil
	default:
		return Console{}, fmt.Errorf("--console must be off, default, file=<path>, or journald=<tag> (got %q)", s)
	}
}

// --- journald sink --------------------------------------------------

// journaldMaxLine bounds an unterminated line before it is force-flushed,
// keeping the buffer from growing without a newline (and well under
// journald's field-size limit).
const journaldMaxLine = 60 << 10

// journaldWriter line-buffers a byte stream and writes each line to journald
// as one entry tagged SYSLOG_IDENTIFIER=<tag>, PRIORITY=info (stdout, stderr
// and kernel console all log at info; build failure is conveyed out-of-band by
// the build status, not the log level). When journald is unavailable (sandbox-ctl
// run outside systemd, e.g. a direct e2e run), it falls back to the process
// stderr with a "[tag] " line prefix so output is never silently lost.
type journaldWriter struct {
	tag      string
	fields   map[string]string
	fallback io.Writer // non-nil ⇒ journald unavailable
	buf      []byte
}

func newJournaldWriter(tag string) *journaldWriter {
	fields := map[string]string{"SYSLOG_IDENTIFIER": tag}
	for _, key := range []string{"KUASAR_RUN_ID", "KUASAR_SANDBOX_ID", "KUASAR_BUILD_ID"} {
		if v := os.Getenv(key); v != "" {
			fields[key] = v
		}
	}
	w := &journaldWriter{tag: tag, fields: fields}
	if !journal.Enabled() {
		w.fallback = os.Stderr
	}
	return w
}

func (w *journaldWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) >= journaldMaxLine { // very long line, no newline yet: force-flush
		w.emit(w.buf)
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

func (w *journaldWriter) emit(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(line) == 0 {
		return
	}
	if w.fallback != nil {
		fmt.Fprintf(w.fallback, "[%s] %s\n", w.tag, line)
		return
	}
	_ = journal.Send(string(line), journal.PriInfo, w.fields)
}

// Close flushes any buffered partial line. Callers invoke it only after the
// producer (the exec copy goroutine / CH) has finished, so there is no
// concurrent Write.
func (w *journaldWriter) Close() error {
	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = w.buf[:0]
	}
	return nil
}

// --- mapping to the launch protocol / MUX ---------------------------

// ProtoSpec is the proto.StdioSpec the host sends in the launch /
// restore / attach handshake telling sandbox-init what to set up. In tty
// mode the caller should fill Winsize from the controlling terminal
// (see InitialWinsize); FromFlags can't read it itself.
func (m Mode) ProtoSpec() proto.StdioSpec {
	if m.TTY {
		return proto.StdioSpec{TTY: true}
	}
	return proto.StdioSpec{
		Stdin:  m.Stdin.active(),
		Stdout: m.Stdout.active(),
		Stderr: m.Stderr.active(),
	}
}

// StreamSet is the MUX stream set this Mode implies.
func (m Mode) StreamSet() mux.StreamSet {
	if m.TTY {
		return mux.PTYStreams()
	}
	return mux.PipeStreams(m.Stdin.active(), m.Stdout.active(), m.Stderr.active())
}

// StreamSetFor builds a mux.StreamSet from a (possibly guest-narrowed)
// proto.StdioSpec — used after the *_ack to bridge exactly what the
// guest established.
func StreamSetFor(s proto.StdioSpec) mux.StreamSet {
	if s.TTY {
		return mux.PTYStreams()
	}
	return mux.PipeStreams(s.Stdin, s.Stdout, s.Stderr)
}

// InitialWinsize reads the controlling terminal's size (tty mode); ok is
// false in pipe mode or if the ioctl fails.
func (m Mode) InitialWinsize() (cols, rows uint16, ok bool) {
	if !m.TTY {
		return 0, 0, false
	}
	return getWinsize(int(os.Stdin.Fd()))
}

// --- CH process stdio setup -----------------------------------------

// SetupCHStdio wires cmd's stdio for spawning cloud-hypervisor and
// returns the value to pass to CH's `--console` flag. cmd.Stdin is left
// nil (CH reads /dev/null, so `--console tty` never raw-izes a terminal);
// cmd.Stderr = sandbox-ctl's stderr (CH's own WARN); cmd.Stdout = the
// kernel-dmesg sink per m.Console. cleanup must be invoked after
// cmd.Wait() (closes a console file if one was opened).
func (m Mode) SetupCHStdio(cmd *exec.Cmd) (consoleArg string, cleanup func(), err error) {
	cmd.Stdin = nil // → /dev/null
	cmd.Stderr = os.Stderr
	switch m.Console.Kind {
	case ConsoleOff:
		cmd.Stdout = nil // → /dev/null
		return "off", func() {}, nil
	case ConsoleFile:
		f, ferr := os.OpenFile(m.Console.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if ferr != nil {
			return "", nil, fmt.Errorf("stdio: open --console file %s: %w", m.Console.Path, ferr)
		}
		cmd.Stdout = f
		return "tty", func() { _ = f.Close() }, nil
	case ConsoleJournald:
		// A non-*os.File writer makes exec create an internal pipe + copy
		// goroutine (joined by cmd.Wait), so CH sees a non-tty stdout (skips
		// sigwinch/raw, as in the ConsoleStderr pipe case) and its kernel
		// dmesg is line-written to journald. cleanup flushes the partial line.
		jw := newJournaldWriter(m.Console.Tag)
		cmd.Stdout = jw
		return "tty", func() { _ = jw.Close() }, nil
	default: // ConsoleStderr
		if m.TTY {
			// Raw terminal: translate \n→\r\n. Passing a non-*os.File
			// io.Writer makes exec run (and cmd.Wait join) the copy.
			cmd.Stdout = CRLF(os.Stderr)
		} else {
			// Pipe mode: wrap so exec creates an internal pipe instead
			// of inheriting os.Stderr's fd directly. Two effects when
			// sandbox-ctl is run from an interactive terminal:
			//  1. CH's isatty(stdout) returns 0 → `--console tty` skips
			//     sigwinch_listener (whose child does TIOCSCTTY on the
			//     host terminal → EPERM → CH panics in VmCreate) AND
			//     skips set_raw_mode (which would silently grab the
			//     terminal into raw mode).
			//  2. Data still ends up in sandbox-ctl's stderr via the
			//     copy goroutine joined by cmd.Wait().
			cmd.Stdout = struct{ io.Writer }{os.Stderr}
		}
		return "tty", func() {}, nil
	}
}

// --- MUX bridge -----------------------------------------------------

// Bridge connects the resolved Mode's host-side endpoints to a live MUX
// session's streams. streams is the negotiated set echoed in the *_ack
// (use StreamSetFor). It returns a cleanup that, in tty mode, restores
// the terminal and stops the SIGWINCH handler; in all modes it closes
// any files it opened and waits for the guest→host pumps to drain
// (which they do once the app exits / the session ends). ctx cancel or
// session death also ends the pumps.
func (m Mode) Bridge(ctx context.Context, sess *mux.Session, streams mux.StreamSet) (cleanup func(), err error) {
	var (
		closers []io.Closer
		waits   []func()
		restore func()
	)
	cleanupAll := func() {
		if restore != nil {
			restore()
		}
		for _, w := range waits {
			w()
		}
		for _, c := range closers {
			_ = c.Close()
		}
	}
	defer func() {
		if err != nil {
			cleanupAll()
		}
	}()

	if m.TTY {
		if !streams[mux.StreamPTY] {
			return nil, errors.New("stdio: tty mode but guest did not establish a PTY stream")
		}
		fd := int(os.Stdin.Fd())
		saved, terr := makeRaw(fd)
		if terr != nil {
			return nil, fmt.Errorf("stdio: set raw mode: %w", terr)
		}
		// Raw mode disables the kernel's ONLCR — every '\n' written to
		// this terminal stairsteps. Co-pair the global log's output with
		// the termios state so sandbox-ctl's own log.Printf (and anything
		// else routed through log.Default().Writer()) emits CRLF for as
		// long as, and only as long as, the terminal is raw.
		prevLogOut := log.Writer()
		log.SetOutput(CRLF(prevLogOut))
		var restoreOnce sync.Once
		restore = func() {
			restoreOnce.Do(func() {
				log.SetOutput(prevLogOut)
				_ = restoreTermios(fd, saved)
			})
		}

		pty := sess.Stream(mux.StreamPTY)
		// initial winsize
		if cols, rows, ok := getWinsize(fd); ok {
			_ = sess.SetWinsize(cols, rows)
		}
		// SIGWINCH → SET_WINSIZE
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		winchDone := make(chan struct{})
		go func() {
			defer close(winchDone)
			for {
				select {
				case <-ctx.Done():
					return
				case <-sess.Done():
					return
				case <-winch:
					if cols, rows, ok := getWinsize(fd); ok {
						_ = sess.SetWinsize(cols, rows)
					}
				}
			}
		}()
		// keystrokes → guest pty (leaked on exit: os.Stdin can't be
		// unblocked; the process exits right after Bridge cleanup).
		go func() { _, _ = io.Copy(pty, os.Stdin) }()
		// guest pty output → terminal, verbatim
		outDone := make(chan struct{})
		go func() { defer close(outDone); _, _ = io.Copy(os.Stdout, pty) }()

		waits = append(waits, func() {
			signal.Stop(winch)
			<-winchDone
			<-outDone
		})
		return cleanupAll, nil
	}

	// pipe mode
	// stdin: host → guest (only if a channel exists)
	if streams[mux.StreamStdin] {
		var src io.ReadCloser
		switch m.Stdin.Kind {
		case StreamInherit:
			src = io.NopCloser(os.Stdin)
		case StreamFile:
			f, ferr := os.OpenFile(m.Stdin.Path, os.O_RDONLY, 0)
			if ferr != nil {
				return nil, fmt.Errorf("stdio: open --stdin-from %s: %w", m.Stdin.Path, ferr)
			}
			src = f
			closers = append(closers, f)
		default:
			src = io.NopCloser(strings.NewReader(""))
		}
		st := sess.Stream(mux.StreamStdin)
		go func() {
			_, _ = io.Copy(st, src)
			_ = st.CloseWrite() // host stdin EOF → guest closes app's stdin
		}()
	}
	// stdout / stderr: guest → host
	pipeOut := func(streamID uint8, s Stream, inherit *os.File) error {
		if !streams[streamID] {
			return nil
		}
		var dst io.Writer
		switch s.Kind {
		case StreamInherit:
			dst = inherit
		case StreamFile:
			f, ferr := os.OpenFile(s.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			if ferr != nil {
				return fmt.Errorf("stdio: open output %s: %w", s.Path, ferr)
			}
			dst = f
			closers = append(closers, f)
		case StreamJournald:
			jw := newJournaldWriter(s.Tag)
			dst = jw
			closers = append(closers, jw) // Close (flush) runs after the copy goroutine is waited
		default: // StreamNone — should not happen if streams[streamID]
			dst = io.Discard
		}
		st := sess.Stream(streamID)
		done := make(chan struct{})
		go func() { defer close(done); _, _ = io.Copy(dst, st) }()
		waits = append(waits, func() { <-done })
		return nil
	}
	if err = pipeOut(mux.StreamStdout, m.Stdout, os.Stdout); err != nil {
		return nil, err
	}
	if err = pipeOut(mux.StreamStderr, m.Stderr, os.Stderr); err != nil {
		return nil, err
	}
	return cleanupAll, nil
}

// --- terminal helpers (Linux) ---------------------------------------

// IsTerminal reports whether fd refers to a terminal.
func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

func getWinsize(fd int) (cols, rows uint16, ok bool) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, false
	}
	return ws.Col, ws.Row, true
}

// makeRaw puts fd into raw mode (cfmakeraw-equivalent) and returns the
// previous settings for restoreTermios.
func makeRaw(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	saved := *old
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	return &saved, nil
}

func restoreTermios(fd int, t *unix.Termios) error {
	if t == nil {
		return nil
	}
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}

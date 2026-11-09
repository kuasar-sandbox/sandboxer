package journalio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// nativeSender belongs to one Writer and is serialized by that Writer's mutex.
// In particular, diagnostics never share a send lock with a backpressured user
// stream. Connections are opened lazily so selecting a target does not require
// a running journal, and Close only releases this output's own connection.
type nativeSender struct {
	path       string
	diagnostic bool
	conn       *net.UnixConn
}

func newNativeSender(path string, diagnostic bool) *nativeSender {
	return &nativeSender{path: path, diagnostic: diagnostic}
}

func (s *nativeSender) send(message string, fields map[string]string) error {
	if err := s.connect(); err != nil {
		return err
	}
	data := journalEntry(message, fields)
	err := s.sendEntry(data)
	if err == nil || s.diagnostic {
		return err
	}
	_ = s.close()
	// A journal can replace its socket between two messages. These errors
	// mean the datagram was not delivered to the old peer, so retry it once
	// against the current path before falling back. Never replay a successful
	// send or introduce a retry loop for congestion or other failures.
	if errors.Is(err, unix.ECONNREFUSED) || errors.Is(err, unix.ENOTCONN) {
		if err := s.connect(); err != nil {
			return err
		}
		err = s.sendEntry(data)
		if err != nil {
			_ = s.close()
		}
	}
	return err
}

func (s *nativeSender) connect() error {
	if s.conn != nil {
		return nil
	}
	var err error
	if s.diagnostic {
		// Keeping the destination per message also allows the next diagnostic
		// to reach a journal whose socket was replaced after a restart.
		s.conn, err = net.ListenUnixgram("unixgram", &net.UnixAddr{Net: "unixgram"})
	} else {
		// A connected datagram socket lets the kernel's writable readiness
		// account for the peer's receive queue. An unconnected WriteMsgUnix
		// can spin on EAGAIN + writable events while that queue remains full.
		s.conn, err = net.DialUnix("unixgram", nil, &net.UnixAddr{Name: s.path, Net: "unixgram"})
	}
	return err
}

func (s *nativeSender) sendEntry(data []byte) error {
	err := s.sendmsg(data, nil)
	if errors.Is(err, unix.EMSGSIZE) || errors.Is(err, unix.ENOBUFS) {
		// Preserve the journal native protocol's large-entry representation.
		// The payload is kept in a sealed anonymous memory file;
		// the descriptor send follows the same stream/diagnostic wait policy.
		err = s.sendFile(data)
	}
	return err
}

func (s *nativeSender) sendmsg(data, rights []byte) error {
	raw, err := s.conn.SyscallConn()
	if err != nil {
		return err
	}
	var n int
	var sendErr error
	if s.diagnostic {
		// Control pins fd ownership but performs no netpoll wait/retry. Do not
		// use UnixConn.WriteMsgUnix here: it hides EAGAIN behind a retry loop.
		err = raw.Control(func(fd uintptr) {
			n, sendErr = unix.SendmsgN(int(fd), data, rights, &unix.SockaddrUnix{Name: s.path}, unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
		})
	} else {
		err = raw.Write(func(fd uintptr) bool {
			for {
				n, sendErr = unix.SendmsgN(int(fd), data, rights, nil, unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
				if !errors.Is(sendErr, unix.EINTR) {
					break
				}
			}
			return !errors.Is(sendErr, unix.EAGAIN)
		})
	}
	if err != nil {
		return err
	}
	if sendErr != nil {
		return sendErr
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func (s *nativeSender) sendFile(data []byte) error {
	fd, err := unix.MemfdCreate("journal-entry", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "journal-entry")
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_SEAL|unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_WRITE); err != nil {
		return err
	}
	return s.sendmsg(nil, unix.UnixRights(fd))
}

func (s *nativeSender) close() error {
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// journalEntry encodes the journal native protocol. Target validation owns the
// field names; multiline values use their byte length, not a line delimiter.
// See https://systemd.io/JOURNAL_NATIVE_PROTOCOL/.
func journalEntry(message string, fields map[string]string) []byte {
	var b bytes.Buffer
	appendJournalField(&b, "PRIORITY", "6")
	appendJournalField(&b, "MESSAGE", message)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		appendJournalField(&b, key, fields[key])
	}
	return b.Bytes()
}

func appendJournalField(b *bytes.Buffer, key, value string) {
	b.WriteString(key)
	if strings.ContainsRune(value, '\n') {
		b.WriteByte('\n')
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(value)))
		b.Write(size[:])
	} else {
		b.WriteByte('=')
	}
	b.WriteString(value)
	b.WriteByte('\n')
}

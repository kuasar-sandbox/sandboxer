package stdio

import (
	"bytes"
	"io"
	"sync"

	"github.com/kuasar-sandbox/sandboxer/internal/wireio"
)

// crlfWriter translates bare '\n' to "\r\n" on the way to w. It is used
// for everything sandbox-ctl writes to a controlling terminal that has
// been put into raw mode (its own diagnostic log, the --console=default
// kernel-dmesg passthrough) — raw mode disables the kernel's ONLCR, so
// without this such output "stairsteps". The user application's PTY
// stream is NOT wrapped: the guest-side pty already applied ONLCR.
type crlfWriter struct {
	w       io.Writer
	mu      sync.Mutex
	wroteCR bool // an error stopped between the CR and LF of an input newline
}

// CRLF wraps w so bare '\n' becomes "\r\n".
func CRLF(w io.Writer) io.Writer { return &crlfWriter{w: w} }

func (c *crlfWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(p) == 0 {
		return c.w.Write(p)
	}

	encoded := p
	c.wroteCR = c.wroteCR && p[0] == '\n'
	if bytes.IndexByte(p, '\n') >= 0 {
		encoded = bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))
		if c.wroteCR {
			encoded = encoded[1:] // resume the first newline after its CR
		}
	}
	n, err := wireio.WriteFull(c.w.Write, encoded)
	if n == len(encoded) {
		c.wroteCR = false
		return len(p), err
	}

	// On an error, report only input bytes whose entire encoding was written.
	consumed := 0
	for ; n > 0; n-- {
		if p[consumed] == '\n' && !c.wroteCR {
			c.wroteCR = true
		} else {
			consumed++
			c.wroteCR = false
		}
	}
	return consumed, err
}

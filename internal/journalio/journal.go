package journalio

import (
	"io"
)

const journalSocketPath = "/run/systemd/journal/socket"

// New opens an explicit journal output. It never infers journal selection from
// stdio, reads identity variables, or inherits other outputs' fields. prefix is
// used only for best-effort fallback, never included in native MESSAGE values.
// A file fallback is terminal-aware only when a native send actually fails.
func New(t Target, fallback io.Writer, prefix string) (*Writer, error) {
	return newJournalWriter(t, fallback, prefix, journalSocketPath, false)
}

// NewDiagnostic opens best-effort component diagnostics. A congested or absent
// journal drops the affected message without waiting or calling a fallback.
// User output must use New so that receiver congestion retains its backpressure.
func NewDiagnostic(t Target) (*Writer, error) {
	return newJournalWriter(t, nil, "", journalSocketPath, true)
}

func newJournalWriter(t Target, fallback io.Writer, prefix, path string, diagnostic bool) (*Writer, error) {
	sender := newNativeSender(path, diagnostic)
	if diagnostic {
		fallback = nil
	}
	w, err := newWriter(t, terminalFallback(fallback), prefix, sender.send)
	if err != nil {
		return nil, err
	}
	w.closeSend = sender.close
	return w, nil
}

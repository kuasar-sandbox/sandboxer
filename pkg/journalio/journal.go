// Package journalio opens explicit journald output streams for executable and
// SDK integrations.
package journalio

import (
	"io"

	internaljournal "github.com/kuasar-sandbox/sandboxer/internal/journalio"
)

// Open opens an independent output for target, using the syntax
// journald=TAG[,FIELD=VALUE...]. Empty and "default" targets are not journal
// targets; callers select their own default output. Fields are explicit and do
// not inherit process environment or another output's identity.
//
// Writes and Close are serialized. Complete lines are sent synchronously in
// bounded frames, retaining backpressure while the journal queue is full;
// Close flushes any remaining partial line. Native send failures
// use fallback on a best-effort basis. A nil fallback discards failed sends.
// The caller retains ownership of fallback: closing the result never closes it.
// Open does not install or change the process's default logger.
func Open(target string, fallback io.Writer) (io.WriteCloser, error) {
	parsed, err := internaljournal.Parse(target)
	if err != nil {
		return nil, err
	}
	writer, err := internaljournal.New(parsed, fallback, "")
	if err != nil {
		return nil, err
	}
	return writer, nil
}

// OpenDiagnostic opens best-effort component diagnostics for an explicit journal
// target. Unlike Open, it never waits for the journal to consume a queued record.
// Congestion or an unavailable journal drops the affected message, without a
// synchronous stderr fallback. Later messages can be sent when the journal
// recovers. Each output owns its send connection independently of user streams.
//
// Writes and Close are serialized; Close attempts a final partial line and
// releases the connection. Use Open for user output that requires backpressure.
// OpenDiagnostic does not install or change the process's default logger.
func OpenDiagnostic(target string) (io.WriteCloser, error) {
	parsed, err := internaljournal.Parse(target)
	if err != nil {
		return nil, err
	}
	return internaljournal.NewDiagnostic(parsed)
}

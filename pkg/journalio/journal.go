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
// bounded frames; Close flushes any remaining partial line. Native send failures
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

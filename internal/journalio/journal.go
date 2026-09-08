package journalio

import (
	"io"

	"github.com/coreos/go-systemd/v22/journal"
)

// New opens an explicit journal output. It does not inspect stderr, read
// identity variables, or inherit other outputs' fields. prefix is used only
// for best-effort fallback, never included in native MESSAGE values.
func New(t Target, fallback io.Writer, prefix string) (*Writer, error) {
	return newWriter(t, fallback, prefix, func(message string, fields map[string]string) error {
		return journal.Send(message, journal.PriInfo, fields)
	})
}

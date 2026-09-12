package sandbox

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/usage"
)

// marshalUsageHistory retains at most a wire-sized page and one decoded
// bounded record. Encoding a whole []Record with a limited Writer would still
// let encoding/json allocate the entire oversized page before its first Write.
// end is the already selected confirmed S, not a later concurrent save.
func marshalUsageHistory(history func(int64, int) ([]usage.Record, int64, error), end, cursor int64, limit int) (json.RawMessage, error) {
	if cursor < 0 || cursor > end || limit < 1 || limit > 100 {
		return nil, errors.New("usage: invalid history range")
	}
	const envelope = len(`{"usage":`) + len(`,"type":"usage_response"}`)
	body := make([]byte, 0, 1024)
	body = append(body, `{"records":[`...)
	for count := 0; count < limit; count++ {
		// Even an empty final page checks the supplied cursor's predecessor.
		records, next, err := history(cursor, 1)
		if err != nil {
			return nil, err
		}
		if cursor == end {
			break
		}
		if len(records) != 1 || next <= cursor || next > end {
			return nil, usage.ErrCorrupt
		}
		record, err := json.Marshal(records[0])
		if err != nil {
			return nil, err
		}
		suffix := `],"next_cursor":"` + strconv.FormatInt(next, 10) + `"}`
		comma := 0
		if count > 0 {
			comma = 1
		}
		if len(body)+comma+len(record)+len(suffix)+envelope > ctl.MaxUsageResponseBytes {
			return nil, errors.New("ctl: usage response too large; reduce history limit")
		}
		if comma != 0 {
			body = append(body, ',')
		}
		body = append(body, record...)
		cursor = next
		if cursor == end {
			break
		}
	}
	body = append(body, `],"next_cursor":"`...)
	body = strconv.AppendInt(body, cursor, 10)
	body = append(body, `"}`...)
	return body, nil
}

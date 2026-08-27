package ctl

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ExecRequestFrame is one fully validated exec_request ctl frame.
//
// Raw contains the exact four-byte little-endian length prefix followed by the
// original JSON payload. Callers can therefore authorize Request and forward
// Raw without re-marshaling or consuming bytes from the following MUX stream.
type ExecRequestFrame struct {
	Request Request
	Raw     []byte
}

// ReadExecRequestFrame reads and strictly validates the first ctl exec frame.
//
// The frame is bounded by MaxMessageBytes. The JSON must be one object with the
// exact Request, proto.ExecSpec, proto.StdioSpec, and proto.Winsize field sets;
// duplicate fields are rejected at every object level. The request must be an
// exec_request with a non-nil Exec and at least one argv element.
//
// This function reads exactly one frame. Bytes following its payload remain in
// r for the subsequent ctl/MUX relay.
func ReadExecRequestFrame(r io.Reader) (*ExecRequestFrame, error) {
	if r == nil {
		return nil, errors.New("ctl: read exec request frame: nil reader")
	}

	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, fmt.Errorf("ctl: read exec request frame length: %w", err)
	}
	n := binary.LittleEndian.Uint32(prefix[:])
	if n > MaxMessageBytes {
		return nil, fmt.Errorf("ctl: read exec request frame: oversized message: %d", n)
	}

	raw := make([]byte, 4+int(n))
	copy(raw, prefix[:])
	payload := raw[4:]
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("ctl: read exec request frame payload: %w", err)
	}
	request, err := decodeStrictExecRequest(payload)
	if err != nil {
		return nil, fmt.Errorf("ctl: read exec request frame: %w", err)
	}
	return &ExecRequestFrame{Request: request, Raw: raw}, nil
}

var (
	execRequestFields = map[string]struct{}{
		"type": {},
		"exec": {},
	}
	execSpecFields = map[string]struct{}{
		"argv":  {},
		"env":   {},
		"cwd":   {},
		"user":  {},
		"stdio": {},
	}
	stdioSpecFields = map[string]struct{}{
		"tty":     {},
		"winsize": {},
		"stdin":   {},
		"stdout":  {},
		"stderr":  {},
	}
	winsizeFields = map[string]struct{}{
		"cols": {},
		"rows": {},
	}
)

func decodeStrictExecRequest(payload []byte) (Request, error) {
	if err := validateSingleJSONObject(payload); err != nil {
		return Request{}, err
	}
	top, err := decodeExactJSONObject(payload, "request", execRequestFields)
	if err != nil {
		return Request{}, err
	}

	execRaw, ok := top["exec"]
	if !ok || bytes.Equal(bytes.TrimSpace(execRaw), []byte("null")) {
		return Request{}, errors.New("exec must be a non-null object")
	}
	execObject, err := decodeExactJSONObject(execRaw, "exec", execSpecFields)
	if err != nil {
		return Request{}, err
	}
	if stdioRaw, ok := execObject["stdio"]; ok &&
		!bytes.Equal(bytes.TrimSpace(stdioRaw), []byte("null")) {
		stdioObject, err := decodeExactJSONObject(stdioRaw, "exec.stdio", stdioSpecFields)
		if err != nil {
			return Request{}, err
		}
		if winsizeRaw, ok := stdioObject["winsize"]; ok &&
			!bytes.Equal(bytes.TrimSpace(winsizeRaw), []byte("null")) {
			if _, err := decodeExactJSONObject(winsizeRaw, "exec.stdio.winsize", winsizeFields); err != nil {
				return Request{}, err
			}
		}
	}

	var request Request
	if err := json.Unmarshal(payload, &request); err != nil {
		return Request{}, errors.New("invalid exec request field value")
	}
	if request.Type != TypeExecRequest {
		return Request{}, errors.New("request type must be exec_request")
	}
	if request.Exec == nil {
		return Request{}, errors.New("exec must be a non-null object")
	}
	if len(request.Exec.Argv) == 0 {
		return Request{}, errors.New("exec argv must not be empty")
	}
	return request, nil
}

func decodeExactJSONObject(raw []byte, path string, allowed map[string]struct{}) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be an object", path)
	}
	for field := range object {
		if _, ok := allowed[field]; !ok {
			return nil, fmt.Errorf("%s contains an unknown field", path)
		}
	}
	return object, nil
}

func validateSingleJSONObject(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return errors.New("invalid JSON object")
	}
	if token != json.Delim('{') {
		return errors.New("request must be an object")
	}
	if err := validateJSONObjectTokens(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request contains a second JSON value")
		}
		return errors.New("invalid trailing JSON data")
	}
	return nil
}

func validateJSONObjectTokens(decoder *json.Decoder) error {
	fields := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid JSON object")
		}
		field, ok := token.(string)
		if !ok {
			return errors.New("invalid JSON object field")
		}
		if _, duplicate := fields[field]; duplicate {
			return errors.New("duplicate object field")
		}
		fields[field] = struct{}{}
		if err := validateJSONValueTokens(decoder); err != nil {
			return err
		}
	}
	token, err := decoder.Token()
	if err != nil || token != json.Delim('}') {
		return errors.New("invalid JSON object")
	}
	return nil
}

func validateJSONValueTokens(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return errors.New("invalid JSON value")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return validateJSONObjectTokens(decoder)
	case '[':
		for decoder.More() {
			if err := validateJSONValueTokens(decoder); err != nil {
				return err
			}
		}
		token, err := decoder.Token()
		if err != nil || token != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
		return nil
	default:
		return errors.New("invalid JSON value")
	}
}

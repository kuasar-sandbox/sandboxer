package ctl

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/kuasar-sandbox/sandboxer/internal/wireio"
)

// Usage replies can include bounded vCPU arrays or pages of saved records.
// The existing short-management and MUX framing limits remain unchanged.
const MaxUsageResponseBytes = 1024 * 1024

func WriteUsageResponse(w io.Writer, response Response) error {
	if response.Type != TypeUsageResponse && response.Type != TypeError {
		return errors.New("ctl: invalid usage response type")
	}
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(body) > MaxUsageResponseBytes {
		return errors.New("ctl: usage response too large; reduce history limit")
	}
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(len(body)))
	if err := wireio.WriteAll(w, header[:]); err != nil {
		return err
	}
	return wireio.WriteAll(w, body)
}

func ReadUsageResponse(r io.Reader) (Response, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Response{}, err
	}
	n := binary.LittleEndian.Uint32(header[:])
	if n == 0 || n > MaxUsageResponseBytes {
		return Response{}, errors.New("ctl: oversized usage response")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Response{}, err
	}
	var response Response
	if err := json.Unmarshal(body, &response); err != nil {
		return Response{}, err
	}
	if response.Type != TypeUsageResponse && response.Type != TypeError {
		return Response{}, errors.New("ctl: invalid usage response")
	}
	return response, nil
}

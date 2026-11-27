package ctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ExportReferences is neutral metadata from a completed E upload. Public
// artifact reports are adapted by callers, without a ctl -> artifact dependency.
type ExportReferences struct {
	SandboxRefs []string `json:"sandbox_refs"`
	RemovedRefs []string `json:"removed_refs"`
}

const (
	typeExportReportStart = "export_report_start"
	typeExportReportChunk = "export_report_chunk"
	typeExportReportDone  = "export_report_done"
)

type exportReportFrame struct {
	Type      string `json:"type"`
	BodyBytes int    `json:"body_bytes,omitempty"`
	Data      []byte `json:"data,omitempty"`
}

// WriteExportReportResponse uses the same bounded byte-frame pattern as large
// checkpoint management requests. A selected Bundle carrier can be reused by
// many selectors, so the report need not fit one PortableConfig or ctl frame.
// Each 32 KiB chunk stays below MaxMessageBytes after JSON base64 expansion.
func WriteExportReportResponse(w io.Writer, response Response) error {
	if response.Type == TypeError {
		return WriteMessage(w, response)
	}
	if response.Type != TypeExportDone || response.ExportReferences == nil {
		return errors.New("ctl: invalid export report response")
	}
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if err := WriteMessage(w, exportReportFrame{Type: typeExportReportStart, BodyBytes: len(body)}); err != nil {
		return err
	}
	for start := 0; start < len(body); start += 32 * 1024 {
		if err := WriteMessage(w, exportReportFrame{Type: typeExportReportChunk, Data: body[start:min(start+32*1024, len(body))]}); err != nil {
			return err
		}
	}
	return WriteMessage(w, exportReportFrame{Type: typeExportReportDone, BodyBytes: len(body)})
}

// ReadExportReportResponse accepts a complete report or an ordinary error frame
// (including an older owner rejecting the new request before capture starts).
func ReadExportReportResponse(r io.Reader) (Response, error) {
	var first struct {
		exportReportFrame
		Code string `json:"code,omitempty"`
		Msg  string `json:"msg,omitempty"`
	}
	if err := ReadMessage(r, &first); err != nil {
		return Response{}, err
	}
	if first.Type == TypeError {
		return Response{Type: TypeError, Code: first.Code, Msg: first.Msg}, nil
	}
	if first.Type != typeExportReportStart || first.BodyBytes <= 0 || len(first.Data) != 0 {
		return Response{}, errors.New("ctl: invalid export report header")
	}
	// Do not allocate the peer's declared size before receiving its bytes.
	var body []byte
	for {
		var frame exportReportFrame
		if err := ReadMessage(r, &frame); err != nil {
			return Response{}, fmt.Errorf("ctl: incomplete export report: %w", err)
		}
		switch frame.Type {
		case typeExportReportChunk:
			if frame.BodyBytes != 0 || len(frame.Data) == 0 || len(frame.Data) > first.BodyBytes-len(body) {
				return Response{}, errors.New("ctl: invalid export report data frame")
			}
			body = append(body, frame.Data...)
		case typeExportReportDone:
			if len(frame.Data) != 0 || frame.BodyBytes != first.BodyBytes || len(body) != first.BodyBytes {
				return Response{}, errors.New("ctl: invalid export report completion")
			}
			var response Response
			if err := json.Unmarshal(body, &response); err != nil {
				return Response{}, err
			}
			if response.Type != TypeExportDone || response.ExportReferences == nil {
				return Response{}, errors.New("ctl: invalid completed export report")
			}
			return response, nil
		default:
			return Response{}, fmt.Errorf("ctl: unexpected export report frame %q", frame.Type)
		}
	}
}

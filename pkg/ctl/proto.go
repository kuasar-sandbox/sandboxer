// Package ctl is the host-local control protocol spoken on
// <run-dir>/<sid>/ctl.sock between the short-lived sandbox-ctl
// subcommands (snapshot / exec) and the long-running sandbox-ctl run
// process that owns the VMM.
//
// Wire format: [4 bytes LE length][JSON] — identical framing to the
// vsock management protocol (pkg/proto), kept separate because
// ctl.sock is a host-local UDS carrying host-side request types, not a
// guest channel.
//
// Two request shapes:
//
//   - snapshot_request — one request, one response, conn closes. The
//     run process handles it via Server.SnapshotHandler.
//   - exec_request — handshake (exec_request → exec_ack|error), then
//     the SAME connection switches to the stdio MUX (pkg/mux)
//     end-to-end between `sandbox-ctl exec` and the guest. The run
//     process pipes bytes through transparently after the ack.
package ctl

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/kuasar-sandbox/sandboxer/internal/wireio"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// Request is a control request on ctl.sock. Type selects which fields
// are populated.
type Request struct {
	Type string `json:"type"`

	// snapshot_request: OutDir and Upload are mutually exclusive (the
	// receiving run process enforces); ResumeAfter defaults to false
	// (zero value = sandbox is destroyed after snapshot).
	OutDir string `json:"out_dir,omitempty"`
	Upload bool   `json:"upload,omitempty"`
	// Mode is optional for wire compatibility. Empty means local tarstream;
	// bundle selects one multi-Manifest ZIP Bundle.
	Mode        string `json:"mode,omitempty"`
	ResumeAfter bool   `json:"resume_after,omitempty"`
	StagingDir  string `json:"staging_dir,omitempty"`
	// nil means that guest caches are preserved; callers can explicitly opt in
	// to cache dropping with a true value.
	DropCaches *bool `json:"drop_caches,omitempty"`
	MergeRef   *bool `json:"merge_ref,omitempty"`

	// exec_request: the command + stdio the caller wants run inside the
	// already-running sandbox.
	Exec *proto.ExecSpec `json:"exec,omitempty"`
}

// Response is the run-process reply.
type Response struct {
	Type string `json:"type"`

	// snapshot_done fields.
	MemorySize          uint64                 `json:"memory_size,omitempty"`
	MemoryResident      uint64                 `json:"memory_resident,omitempty"`
	WallclockPauseMs    int64                  `json:"wallclock_pause_ms,omitempty"`
	WallclockDumpMs     int64                  `json:"wallclock_dump_ms,omitempty"`
	SnapshotManifestKey string                 `json:"snapshot_manifest_key,omitempty"`
	OverlayManifestKey  string                 `json:"overlay_manifest_key,omitempty"`
	SnapshotPath        string                 `json:"snapshot_path,omitempty"`
	OverlayPath         string                 `json:"overlay_path,omitempty"`
	OverlayRef          string                 `json:"overlay_ref,omitempty"`
	DropCachesResult    proto.DropCachesResult `json:"drop_caches_result,omitempty"`

	// exec_ack: the stdio channel set the guest actually established
	// (the caller bridges exactly these MUX streams).
	Stdio *proto.StdioSpec `json:"stdio,omitempty"`

	// type=error: human-readable reason.
	Msg string `json:"msg,omitempty"`
}

// DropCachesEnabled reports the per-snapshot cache policy. A missing field
// means false so snapshots preserve guest caches by default.
func (r Request) DropCachesEnabled() bool {
	return r.DropCaches != nil && *r.DropCaches
}

// MergeRefEnabled reports whether a local parent memory ref is flattened into
// the new self artifact. A missing field means true for wire compatibility.
func (r Request) MergeRefEnabled() bool {
	return r.MergeRef == nil || *r.MergeRef
}

const (
	SnapshotModeLocal  = "local"
	SnapshotModeBundle = "bundle"
)

// SnapshotMode returns the backward-compatible mode or rejects unknown input.
func (r Request) SnapshotMode() (string, error) {
	switch r.Mode {
	case "", SnapshotModeLocal:
		return SnapshotModeLocal, nil
	case SnapshotModeBundle:
		return SnapshotModeBundle, nil
	default:
		return "", fmt.Errorf("snapshot mode %q invalid (want local|bundle)", r.Mode)
	}
}

const (
	TypeSnapshotRequest = "snapshot_request"
	TypeSnapshotDone    = "snapshot_done"
	TypeExecRequest     = "exec_request"
	TypeExecAck         = "exec_ack"
	TypeError           = "error"
)

// MaxMessageBytes caps any single message on ctl.sock.
const MaxMessageBytes = 64 * 1024

// WriteMessage writes v as length-prefixed JSON.
func WriteMessage(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > MaxMessageBytes {
		return fmt.Errorf("ctl: message too large: %d > %d", len(body), MaxMessageBytes)
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if err := wireio.WriteAll(w, lenBuf[:]); err != nil {
		return err
	}
	return wireio.WriteAll(w, body)
}

// ReadMessage reads length-prefixed JSON into v (a pointer).
func ReadMessage(r io.Reader, v any) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return err
	}
	n := binary.LittleEndian.Uint32(lenBuf[:])
	if n > MaxMessageBytes {
		return fmt.Errorf("ctl: oversized message: %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

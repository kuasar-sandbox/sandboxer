package ctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	TypeCheckpointSnapshotRequest     = "checkpoint_snapshot_request"
	TypeCheckpointExportRequest       = "checkpoint_export_request"
	TypeCheckpointListRequest         = "checkpoint_list_request_v2"
	TypeCheckpointListChunk           = "checkpoint_list_chunk_v2"
	TypeCheckpointListDone            = "checkpoint_list_done_v2"
	TypeCheckpointDeleteRequest       = "checkpoint_delete_request_v2"
	TypeCheckpointDeleteDone          = "checkpoint_delete_done_v2"
	TypeCheckpointDeleteStreamRequest = "checkpoint_delete_stream_request_v2"
	TypeCheckpointDeleteReady         = "checkpoint_delete_ready_v2"
	typeCheckpointDeleteChunk         = "checkpoint_delete_chunk_v2"
	typeCheckpointDeleteCommit        = "checkpoint_delete_commit_v2"
	CheckpointUnsupportedCode         = "checkpoint_unsupported"
)

var ErrCheckpointUnsupported = errors.New(CheckpointUnsupportedCode)

// CheckpointRef identifies one exact disk or memory version. Exactly one of
// DiffID/MemoryID must be supplied; ID alone never selects a retry target.
type CheckpointRef struct {
	DiffID   string `json:"diff_id,omitempty"`
	MemoryID string `json:"memory_id,omitempty"`
	ID       string `json:"id"`
}
type DiffCheckpointRef = CheckpointRef

type CheckpointSource struct {
	SnapshotRef string `json:"snapshot_ref,omitempty"`
	SandboxRef  string `json:"sandbox_ref,omitempty"`
}

// Sources is required for deletion. Retain must be explicit even when empty;
// the caller keeps its registry fence until this operation returns.
type CheckpointSources struct {
	SnapshotRef  string             `json:"snapshot_ref,omitempty"`
	SandboxRef   string             `json:"sandbox_ref,omitempty"`
	Retain       []CheckpointSource `json:"retain"`
	RefLocations map[string]string  `json:"ref_locations,omitempty"`
}

// WriteCheckpointList always terminates a complete list with an explicit count.
// A disconnected peer cannot mistake a prefix of this stream for a full list.
func WriteCheckpointList(w io.Writer, refs []DiffCheckpointRef) error {
	for start := 0; start < len(refs); start += 128 {
		end := min(start+128, len(refs))
		if err := WriteMessage(w, Response{Type: TypeCheckpointListChunk, Checkpoints: refs[start:end]}); err != nil {
			return err
		}
	}
	return WriteMessage(w, Response{Type: TypeCheckpointListDone, CheckpointCount: len(refs)})
}

func ReadCheckpointList(r io.Reader) ([]DiffCheckpointRef, error) {
	var refs []DiffCheckpointRef
	for {
		var resp Response
		if err := ReadMessage(r, &resp); err != nil {
			return nil, fmt.Errorf("checkpoint list incomplete: %w", err)
		}
		switch resp.Type {
		case TypeCheckpointListChunk:
			if len(resp.Checkpoints) == 0 {
				return nil, errors.New("checkpoint list: empty data frame")
			}
			refs = append(refs, resp.Checkpoints...)
		case TypeCheckpointListDone:
			if len(resp.Checkpoints) != 0 || resp.CheckpointCount != len(refs) {
				return nil, errors.New("checkpoint list: invalid completion count")
			}
			return refs, nil
		case TypeError:
			return nil, CaptureError(resp)
		default:
			return nil, fmt.Errorf("checkpoint list: unexpected frame %q", resp.Type)
		}
	}
}

func CaptureError(resp Response) error {
	if resp.Code == CheckpointUnsupportedCode ||
		resp.Msg == "unknown type: "+TypeCheckpointSnapshotRequest ||
		resp.Msg == "unknown type: "+TypeCheckpointExportRequest {
		return fmt.Errorf("%w: %s", ErrCheckpointUnsupported, resp.Msg)
	}
	return errors.New(resp.Msg)
}

func ListCheckpoints(ctx context.Context, socketPath string) ([]DiffCheckpointRef, error) {
	c, close, err := dialContext(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	defer close()
	if err = WriteMessage(c, Request{Type: TypeCheckpointListRequest}); err != nil {
		return nil, err
	}
	return ReadCheckpointList(c)
}

func DeleteCheckpoints(ctx context.Context, socketPath, ownerID string, refs []CheckpointRef, sources CheckpointSources) error {
	if len(refs) == 0 {
		return nil
	}
	if sources.Retain == nil {
		return errors.New("checkpoint deletion requires an explicit complete retention set")
	}
	// Freeze the caller's exact set before making any progress. Each batch is
	// idempotent, so a failed connection can safely be retried with the same set.
	refs = append([]DiffCheckpointRef(nil), refs...)
	sources.Retain = append([]CheckpointSource{}, sources.Retain...)
	sources.RefLocations = maps.Clone(sources.RefLocations)
	for start := 0; start < len(refs); start += 128 {
		err := func() error {
			c, close, err := dialContext(ctx, socketPath)
			if err != nil {
				return err
			}
			defer close()
			req := Request{Type: TypeCheckpointDeleteRequest, CheckpointOwner: ownerID, CheckpointRefs: refs[start:min(start+128, len(refs))], CheckpointSources: &sources}
			if err := writeCheckpointDeleteRequest(c, req); err != nil {
				return err
			}
			var resp Response
			if err := ReadMessage(c, &resp); err != nil {
				return err
			}
			if resp.Type == TypeError {
				return CaptureError(resp)
			}
			if resp.Type != TypeCheckpointDeleteDone {
				return fmt.Errorf("unexpected checkpoint delete reply %q", resp.Type)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// Large management inputs use the same bounded frames on one connection.
// No graph subset reaches the deletion handler: the complete request and an
// explicit commit frame are required. Disconnects discard an unfinished input.
type checkpointDeleteFrame struct {
	Type string `json:"type"`
	Data []byte `json:"data,omitempty"`
}

func writeCheckpointDeleteRequest(c io.ReadWriter, req Request) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(body) <= MaxMessageBytes {
		return WriteMessage(c, req)
	}
	if err := WriteMessage(c, Request{Type: TypeCheckpointDeleteStreamRequest, BodyBytes: len(body)}); err != nil {
		return err
	}
	var ready Response
	if err := ReadMessage(c, &ready); err != nil {
		return err
	}
	if ready.Type == TypeError {
		return CaptureError(ready)
	}
	if ready.Type != TypeCheckpointDeleteReady {
		return fmt.Errorf("unexpected checkpoint delete readiness %q", ready.Type)
	}
	// JSON base64 expansion of 32 KiB remains below the 64 KiB frame limit.
	for start := 0; start < len(body); start += 32 * 1024 {
		if err := WriteMessage(c, checkpointDeleteFrame{Type: typeCheckpointDeleteChunk, Data: body[start:min(start+32*1024, len(body))]}); err != nil {
			return err
		}
	}
	return WriteMessage(c, checkpointDeleteFrame{Type: typeCheckpointDeleteCommit})
}

func readCheckpointDeleteRequest(r io.Reader, size int) (Request, error) {
	if size <= 0 {
		return Request{}, errors.New("checkpoint delete: invalid input size")
	}
	// Do not allocate the sender's claimed size before receiving its bytes.
	var body []byte
	for {
		var frame checkpointDeleteFrame
		if err := ReadMessage(r, &frame); err != nil {
			return Request{}, fmt.Errorf("checkpoint delete incomplete: %w", err)
		}
		switch frame.Type {
		case typeCheckpointDeleteChunk:
			if len(frame.Data) == 0 || len(frame.Data) > size-len(body) {
				return Request{}, errors.New("checkpoint delete: invalid data frame")
			}
			body = append(body, frame.Data...)
		case typeCheckpointDeleteCommit:
			if len(frame.Data) != 0 || len(body) != size {
				return Request{}, errors.New("checkpoint delete: invalid completion size")
			}
			var req Request
			if err := json.Unmarshal(body, &req); err != nil {
				return Request{}, err
			}
			if req.Type != TypeCheckpointDeleteRequest || req.BodyBytes != 0 {
				return Request{}, errors.New("checkpoint delete: invalid complete request")
			}
			return req, nil
		default:
			return Request{}, fmt.Errorf("checkpoint delete: unexpected frame %q", frame.Type)
		}
	}
}

type CheckpointOwner struct {
	ID     string `json:"id"`
	Socket string `json:"socket"`
}

// PublishCheckpointOwner is called only while holding the directory's writer
// ownership, after the control socket is ready. It never confers ownership.
func PublishCheckpointOwner(directory, socket string) (CheckpointOwner, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return CheckpointOwner{}, err
	}
	owner := CheckpointOwner{ID: hex.EncodeToString(random[:]), Socket: socket}
	body, err := json.Marshal(owner)
	if err != nil {
		return owner, err
	}
	f, err := os.CreateTemp(directory, ".owner-*")
	if err != nil {
		return owner, err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(body); err != nil {
		_ = f.Close()
		return owner, err
	}
	if err = f.Close(); err != nil {
		return owner, err
	}
	return owner, os.Rename(name, filepath.Join(directory, "owner.json"))
}

func ReadCheckpointOwner(directory string) (CheckpointOwner, error) {
	var owner CheckpointOwner
	body, err := os.ReadFile(filepath.Join(directory, "owner.json"))
	if err != nil {
		return owner, err
	}
	if err = json.Unmarshal(body, &owner); err != nil {
		return owner, err
	}
	if owner.ID == "" || !filepath.IsAbs(owner.Socket) {
		return owner, errors.New("invalid checkpoint owner route")
	}
	return owner, nil
}

func RemoveCheckpointOwner(directory string, owner CheckpointOwner) error {
	current, err := ReadCheckpointOwner(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.ID != owner.ID {
		return nil
	}
	return os.Remove(filepath.Join(directory, "owner.json"))
}

// DeleteOwnedDiffCheckpoints is for cleanup forwarding after a failed ownership
// acquisition. Explicit offline directory management must not call it.
func DeleteOwnedDiffCheckpoints(ctx context.Context, directory string, refs []DiffCheckpointRef, sources ...CheckpointSources) error {
	var last error
	for attempt := 0; attempt < 20; attempt++ {
		owner, err := ReadCheckpointOwner(directory)
		if err == nil {
			err = DeleteDiffCheckpoints(ctx, owner.Socket, owner.ID, refs, sources...)
		}
		if err == nil {
			return nil
		}
		last = err
		// Only ownership handoff and transport failures are retryable; version and
		// storage errors remain useful failures to the caller.
		var op *net.OpError
		if !errors.As(err, &op) && !os.IsNotExist(err) && !strings.Contains(err.Error(), "checkpoint owner changed") {
			return err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("checkpoint owner routing: %w", last)
}

// Compatibility names preserve source signatures, but cannot imply that an
// omitted retention set is empty. All management uses the v2 protocol.
func ListDiffCheckpoints(ctx context.Context, socket string) ([]DiffCheckpointRef, error) {
	return ListCheckpoints(ctx, socket)
}
func DeleteDiffCheckpoints(ctx context.Context, socket, owner string, refs []DiffCheckpointRef, sources ...CheckpointSources) error {
	if len(refs) == 0 {
		return nil
	}
	if len(sources) != 1 {
		return errors.New("checkpoint deletion requires an explicit complete retention set")
	}
	return DeleteCheckpoints(ctx, socket, owner, refs, sources[0])
}

package ctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	TypeCheckpointSnapshotRequest = "checkpoint_snapshot_request"
	TypeCheckpointExportRequest   = "checkpoint_export_request"
	TypeCheckpointListRequest     = "checkpoint_list_request"
	TypeCheckpointListChunk       = "checkpoint_list_chunk"
	TypeCheckpointListDone        = "checkpoint_list_done"
	TypeCheckpointDeleteRequest   = "checkpoint_delete_request"
	TypeCheckpointDeleteDone      = "checkpoint_delete_done"
	CheckpointUnsupportedCode     = "checkpoint_unsupported"
)

var ErrCheckpointUnsupported = errors.New(CheckpointUnsupportedCode)

// DiffCheckpointRef identifies one immutable version, never an input alias.
type DiffCheckpointRef struct {
	DiffID string `json:"diff_id"`
	ID     string `json:"id"`
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

func ListDiffCheckpoints(ctx context.Context, socketPath string) ([]DiffCheckpointRef, error) {
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

func DeleteDiffCheckpoints(ctx context.Context, socketPath, ownerID string, refs []DiffCheckpointRef) error {
	// Freeze the caller's exact set before making any progress. Each batch is
	// idempotent, so a failed connection can safely be retried with the same set.
	refs = append([]DiffCheckpointRef(nil), refs...)
	for start := 0; start < len(refs); start += 128 {
		err := func() error {
			c, close, err := dialContext(ctx, socketPath)
			if err != nil {
				return err
			}
			defer close()
			req := Request{Type: TypeCheckpointDeleteRequest, CheckpointOwner: ownerID, CheckpointRefs: refs[start:min(start+128, len(refs))]}
			if err := WriteMessage(c, req); err != nil {
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
func DeleteOwnedDiffCheckpoints(ctx context.Context, directory string, refs []DiffCheckpointRef) error {
	var last error
	for attempt := 0; attempt < 20; attempt++ {
		owner, err := ReadCheckpointOwner(directory)
		if err == nil {
			err = DeleteDiffCheckpoints(ctx, owner.Socket, owner.ID, refs)
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

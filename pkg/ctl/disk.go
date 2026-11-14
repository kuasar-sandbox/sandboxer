package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/kuasar-sandbox/sandboxer/internal/wireio"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

const (
	TypeDiskBind      = "disk_bind_request"
	TypeDiskBindReady = "disk_bind_ready"
	TypeDiskBindDone  = "disk_bind_done"
	TypeDiskList      = "disk_list"
	TypeDiskListDone  = "disk_list_done"
)

type BindDiskRequest struct {
	SlotName     string              `json:"slot_name"`
	Disk         config.DiskV1       `json:"disk"`
	RefLocations config.RefLocations `json:"ref_locations,omitempty"`
	ExpectedRun  string              `json:"expected_run,omitempty"`
}

type DiskState struct {
	Name      string `json:"name"`
	Ordinal   int    `json:"ordinal"`
	State     string `json:"state"`
	SizeBytes *int64 `json:"size_bytes,omitempty"`
}

type DiskStateReport struct {
	RunID string      `json:"run_id"`
	Disks []DiskState `json:"disks"`
}

func (req BindDiskRequest) MarshalBounded() ([]byte, error) {
	if req.SlotName == "" {
		return nil, errors.New("bind requires slot_name")
	}
	if err := req.Disk.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(req)
	if err == nil && len(b) > config.MaxPortableConfigBytes {
		err = errors.New("disk bind request exceeds message limit")
	}
	return b, err
}

func diskResponse(conn net.Conn, want string) (Response, error) {
	var resp Response
	if err := ReadMessage(conn, &resp); err != nil {
		return resp, err
	}
	if resp.Type == TypeError {
		return resp, errors.New(resp.Msg)
	}
	if resp.Type != want {
		return resp, fmt.Errorf("ctl: got %q, expected %q", resp.Type, want)
	}
	return resp, nil
}

func BindDisk(ctx context.Context, path string, req BindDiskRequest) error {
	data, err := req.MarshalBounded()
	if err != nil {
		return err
	}
	conn, close, err := dialContext(ctx, path)
	if err != nil {
		return err
	}
	defer close()
	if err := WriteMessage(conn, Request{Type: TypeDiskBind, BodyBytes: len(data)}); err != nil {
		return err
	}
	if _, err = diskResponse(conn, TypeDiskBindReady); err != nil {
		return err
	}
	if err = wireio.WriteAll(conn, data); err != nil {
		return err
	}
	_, err = diskResponse(conn, TypeDiskBindDone)
	return err
}

func ListDataDisks(ctx context.Context, path, expectedRun string) (DiskStateReport, error) {
	conn, close, err := dialContext(ctx, path)
	if err != nil {
		return DiskStateReport{}, err
	}
	defer close()
	if err := WriteMessage(conn, Request{Type: TypeDiskList, ExpectedRun: expectedRun}); err != nil {
		return DiskStateReport{}, err
	}
	resp, err := diskResponse(conn, TypeDiskListDone)
	if err != nil {
		return DiskStateReport{}, err
	}
	if resp.DiskState == nil || resp.DiskState.RunID == "" || expectedRun != "" && resp.DiskState.RunID != expectedRun {
		return DiskStateReport{}, errors.New("ctl: invalid disk state or owner mismatch")
	}
	return *resp.DiskState, nil
}

func (s *Server) serveDisk(conn net.Conn, req Request) error {
	switch req.Type {
	case TypeDiskList:
		if s.DiskListHandler == nil {
			return errors.New("disk list unavailable")
		}
		report, err := s.DiskListHandler(req.ExpectedRun)
		if err != nil {
			return err
		}
		return WriteMessage(conn, Response{Type: TypeDiskListDone, DiskState: &report})
	default:
		if s.DiskBindHandler == nil {
			return errors.New("disk bind unavailable")
		}
		if req.BodyBytes <= 0 || req.BodyBytes > config.MaxPortableConfigBytes {
			return errors.New("invalid disk bind body length")
		}
		if err := WriteMessage(conn, Response{Type: TypeDiskBindReady}); err != nil {
			return err
		}
		data := make([]byte, req.BodyBytes)
		if _, err := io.ReadFull(conn, data); err != nil {
			return err
		}
		var bind BindDiskRequest
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err := d.Decode(&bind); err != nil {
			return err
		}
		if d.Decode(new(any)) != io.EOF {
			return errors.New("trailing disk bind data")
		}
		if err := s.DiskBindHandler(bind); err != nil {
			return err
		}
		return WriteMessage(conn, Response{Type: TypeDiskBindDone})
	}
}

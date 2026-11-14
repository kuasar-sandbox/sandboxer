package ctl

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// DiskErrorCode classifies a disk bind/list failure in the SDK and on ctl.sock.
// Use errors.Is to match a code while retaining the underlying local error.
type DiskErrorCode string

const (
	ErrDiskInvalid     DiskErrorCode = "invalid_disk"
	ErrDiskNotFound    DiskErrorCode = "disk_not_found"
	ErrDiskUnsupported DiskErrorCode = "disk_unsupported"
	ErrDiskConflict    DiskErrorCode = "disk_conflict"
	ErrDiskCapacity    DiskErrorCode = "disk_capacity"
	ErrDiskPending     DiskErrorCode = "disk_confirmation_pending"
	ErrDiskBusy        DiskErrorCode = "disk_busy"
	ErrDiskPermission  DiskErrorCode = "disk_permission"
)

func (c DiskErrorCode) Error() string { return string(c) }

func diskWireError(err error) Response {
	resp := Response{Type: TypeError, Msg: err.Error()}
	var code DiskErrorCode
	switch {
	case errors.As(err, &code):
		resp.Code = string(code)
	case errors.Is(err, fs.ErrPermission):
		resp.Code = string(ErrDiskPermission)
	}
	return resp
}

func diskRemoteError(resp Response) error {
	code := DiskErrorCode(resp.Code)
	msg := strings.TrimPrefix(resp.Msg, string(code)+": ")
	switch code {
	case ErrDiskInvalid, ErrDiskNotFound, ErrDiskUnsupported, ErrDiskConflict, ErrDiskCapacity, ErrDiskPending, ErrDiskBusy:
		return fmt.Errorf("%w: %s", code, msg)
	case ErrDiskPermission:
		return fmt.Errorf("%w: %w: %s", code, fs.ErrPermission, msg)
	default:
		return errors.New(resp.Msg)
	}
}

// Package vhost implements a minimal vhost-user-blk backend in Go.
//
// The protocol is the vhost-user master/slave wire format spoken over a
// SOCK_STREAM unix socket: a 12-byte header (request, flags, size)
// followed by the payload, with file descriptors carried via SCM_RIGHTS
// in the ancillary message data.
//
// This package implements only the slave (backend) side and only the
// subset of messages required by cloud-hypervisor's virtio-blk client.
package vhost

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
)

// vhost-user message request codes (subset relevant to our backend).
const (
	MsgGetFeatures         uint32 = 1
	MsgSetFeatures         uint32 = 2
	MsgSetOwner            uint32 = 3
	MsgResetOwner          uint32 = 4
	MsgSetMemTable         uint32 = 5
	MsgSetVringNum         uint32 = 8
	MsgSetVringAddr        uint32 = 9
	MsgSetVringBase        uint32 = 10
	MsgGetVringBase        uint32 = 11
	MsgSetVringKick        uint32 = 12
	MsgSetVringCall        uint32 = 13
	MsgGetProtocolFeatures uint32 = 15
	MsgSetProtocolFeatures uint32 = 16
	MsgGetQueueNum         uint32 = 17
	MsgSetVringEnable      uint32 = 18
	MsgSetBackendReqFd     uint32 = 21
	MsgGetConfig           uint32 = 24
	MsgSetConfig           uint32 = 25
	MsgGetMaxMemSlots      uint32 = 36
	MsgAddMemReg           uint32 = 37
	MsgRemMemReg           uint32 = 38
)

// MsgName returns a human-readable name for a message code; "?" for unknown.
// Used in logs only.
func MsgName(req uint32) string {
	names := map[uint32]string{
		MsgGetFeatures:         "GET_FEATURES",
		MsgSetFeatures:         "SET_FEATURES",
		MsgSetOwner:            "SET_OWNER",
		MsgResetOwner:          "RESET_OWNER",
		MsgSetMemTable:         "SET_MEM_TABLE",
		MsgSetVringNum:         "SET_VRING_NUM",
		MsgSetVringAddr:        "SET_VRING_ADDR",
		MsgSetVringBase:        "SET_VRING_BASE",
		MsgGetVringBase:        "GET_VRING_BASE",
		MsgSetVringKick:        "SET_VRING_KICK",
		MsgSetVringCall:        "SET_VRING_CALL",
		MsgGetProtocolFeatures: "GET_PROTOCOL_FEATURES",
		MsgSetProtocolFeatures: "SET_PROTOCOL_FEATURES",
		MsgGetQueueNum:         "GET_QUEUE_NUM",
		MsgSetVringEnable:      "SET_VRING_ENABLE",
		MsgSetBackendReqFd:     "SET_BACKEND_REQ_FD",
		MsgGetConfig:           "GET_CONFIG",
		MsgSetConfig:           "SET_CONFIG",
		MsgGetMaxMemSlots:      "GET_MAX_MEM_SLOTS",
		MsgAddMemReg:           "ADD_MEM_REG",
		MsgRemMemReg:           "REM_MEM_REG",
	}
	if n, ok := names[req]; ok {
		return n
	}
	return fmt.Sprintf("?(%d)", req)
}

// Flag bits in the header flags field.
const (
	FlagVersion1   uint32 = 1
	FlagReply      uint32 = 1 << 2
	FlagNeedReply  uint32 = 1 << 3
	FlagVersionMsk uint32 = 0x3
)

// Header is the 12-byte fixed header at the start of every vhost-user
// message.
type Header struct {
	Request uint32
	Flags   uint32
	Size    uint32
}

const HeaderSize = 12

// MaxPayloadSize bounds payload acceptance. SET_MEM_TABLE with up to
// VHOST_USER_MAX_RAM_SLOTS regions (~32) is the largest realistic message,
// at most a few KiB. We round up generously for safety.
const MaxPayloadSize = 64 * 1024

// MaxFds bounds fd attachment per message. SET_MEM_TABLE attaches one
// fd per region; vhost-user spec caps regions at 32 (VHOST_USER_MAX_RAM_SLOTS).
const MaxFds = 32

// Message is a parsed vhost-user message from the wire.
type Message struct {
	Header  Header
	Payload []byte
	Fds     []int // ancillary fds; caller must close when done
}

// IsReply reports whether the REPLY flag is set.
func (m *Message) IsReply() bool { return m.Header.Flags&FlagReply != 0 }

// NeedsReply reports whether the master expects a reply for this message.
func (m *Message) NeedsReply() bool { return m.Header.Flags&FlagNeedReply != 0 }

// ReadMessage reads exactly one vhost-user message from c, including
// ancillary fds. The returned fds are owned by the caller.
func ReadMessage(c *net.UnixConn) (*Message, error) {
	hdrBuf := make([]byte, HeaderSize)
	oob := make([]byte, syscall.CmsgSpace(MaxFds*4))

	n, oobn, _, _, err := c.ReadMsgUnix(hdrBuf, oob)
	if err != nil {
		return nil, err
	}
	if n != HeaderSize {
		return nil, fmt.Errorf("vhost: short header read: %d != %d", n, HeaderSize)
	}

	hdr := Header{
		Request: binary.LittleEndian.Uint32(hdrBuf[0:4]),
		Flags:   binary.LittleEndian.Uint32(hdrBuf[4:8]),
		Size:    binary.LittleEndian.Uint32(hdrBuf[8:12]),
	}
	if hdr.Size > MaxPayloadSize {
		return nil, fmt.Errorf("vhost: payload size %d exceeds max %d", hdr.Size, MaxPayloadSize)
	}

	var fds []int
	if oobn > 0 {
		scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return nil, fmt.Errorf("vhost: parse SCM: %w", err)
		}
		for _, scm := range scms {
			if scm.Header.Level == syscall.SOL_SOCKET && scm.Header.Type == syscall.SCM_RIGHTS {
				rights, err := syscall.ParseUnixRights(&scm)
				if err != nil {
					return nil, fmt.Errorf("vhost: parse rights: %w", err)
				}
				fds = append(fds, rights...)
			}
		}
	}

	payload := make([]byte, hdr.Size)
	if hdr.Size > 0 {
		if _, err := io.ReadFull(c, payload); err != nil {
			closeFds(fds)
			return nil, fmt.Errorf("vhost: read payload: %w", err)
		}
	}

	return &Message{Header: hdr, Payload: payload, Fds: fds}, nil
}

// WriteMessage writes one message (no fds; replies don't carry fds).
func WriteMessage(c *net.UnixConn, hdr Header, payload []byte) error {
	hdr.Size = uint32(len(payload))
	buf := make([]byte, HeaderSize+len(payload))
	binary.LittleEndian.PutUint32(buf[0:4], hdr.Request)
	binary.LittleEndian.PutUint32(buf[4:8], hdr.Flags)
	binary.LittleEndian.PutUint32(buf[8:12], hdr.Size)
	copy(buf[HeaderSize:], payload)
	_, err := c.Write(buf)
	return err
}

// SendReply assembles a reply message in response to req and sends it.
// The reply uses the same Request code as the original message and sets
// the REPLY flag.
func SendReply(c *net.UnixConn, req uint32, payload []byte) error {
	return WriteMessage(c, Header{
		Request: req,
		Flags:   FlagVersion1 | FlagReply,
	}, payload)
}

// SendU64Reply is a shortcut for replies whose payload is a single
// little-endian uint64 (GET_FEATURES, GET_PROTOCOL_FEATURES, GET_QUEUE_NUM).
func SendU64Reply(c *net.UnixConn, req uint32, value uint64) error {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, value)
	return SendReply(c, req, buf)
}

// closeFds is a defensive helper to close a batch of fds.
func closeFds(fds []int) {
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
}

// ParseU64 reads a little-endian uint64 from the start of payload.
func ParseU64(payload []byte) (uint64, error) {
	if len(payload) < 8 {
		return 0, errors.New("vhost: payload too short for u64")
	}
	return binary.LittleEndian.Uint64(payload[:8]), nil
}

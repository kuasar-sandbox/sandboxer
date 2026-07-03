package vhost

import (
	"encoding/binary"
	"fmt"
)

// virtio-blk request types.
const (
	BlkTypeIn        uint32 = 0 // read
	BlkTypeOut       uint32 = 1 // write
	BlkTypeFlush     uint32 = 4
	BlkTypeGetID     uint32 = 8
	BlkTypeDiscard   uint32 = 11
	BlkTypeWriteZero uint32 = 13
)

// virtio-blk request status (single byte at end of descriptor chain).
const (
	BlkStatusOK     byte = 0
	BlkStatusIOErr  byte = 1
	BlkStatusUnsupp byte = 2
)

// SectorSize is the virtio-blk sector size (always 512 by spec).
const SectorSize = 512

// BlkReqHeader is the 16-byte virtio_blk_req prefix.
type BlkReqHeader struct {
	Type     uint32
	Reserved uint32
	Sector   uint64
}

// ParseBlkReqHeader extracts the request type and starting sector from
// the first 16 bytes of the request descriptor.
func ParseBlkReqHeader(b []byte) (BlkReqHeader, error) {
	if len(b) < 16 {
		return BlkReqHeader{}, fmt.Errorf("vhost: blk req header too short: %d", len(b))
	}
	return BlkReqHeader{
		Type:     binary.LittleEndian.Uint32(b[0:4]),
		Reserved: binary.LittleEndian.Uint32(b[4:8]),
		Sector:   binary.LittleEndian.Uint64(b[8:16]),
	}, nil
}

// BlkReqTypeName returns a human-readable type for logs.
func BlkReqTypeName(t uint32) string {
	switch t {
	case BlkTypeIn:
		return "IN"
	case BlkTypeOut:
		return "OUT"
	case BlkTypeFlush:
		return "FLUSH"
	case BlkTypeGetID:
		return "GET_ID"
	case BlkTypeDiscard:
		return "DISCARD"
	case BlkTypeWriteZero:
		return "WRITE_ZEROES"
	default:
		return fmt.Sprintf("?(%d)", t)
	}
}

// virtio-blk config struct (returned by GET_CONFIG).
// Layout matches Linux's struct virtio_blk_config.
type BlkConfig struct {
	Capacity   uint64 // in 512-byte sectors
	SizeMax    uint32
	SegMax     uint32
	Cylinders  uint16
	Heads      uint8
	Sectors    uint8
	BlkSize    uint32
	PhysBlkExp uint8
	AlignOffs  uint8
	MinIOSize  uint16
	OptIOSize  uint32
	Writeback  uint8
	Unused0    uint8
	NumQueues  uint16
	MaxDiscSec uint32
	MaxDiscSeg uint32
	DiscSecAln uint32
	MaxWrZSec  uint32
	MaxWrZSeg  uint32
	WrZeroMayU uint8
	Unused1    [3]uint8
	MaxSecSec  uint32
	MaxSecSeg  uint32
	SecSecAln  uint32
}

// BlkConfigSize is the wire size of BlkConfig (60 bytes per virtio 1.2 spec).
const BlkConfigSize = 60

// Marshal writes BlkConfig to a 60-byte buffer in the virtio wire format.
func (c *BlkConfig) Marshal() []byte {
	buf := make([]byte, BlkConfigSize)
	binary.LittleEndian.PutUint64(buf[0:8], c.Capacity)
	binary.LittleEndian.PutUint32(buf[8:12], c.SizeMax)
	binary.LittleEndian.PutUint32(buf[12:16], c.SegMax)
	binary.LittleEndian.PutUint16(buf[16:18], c.Cylinders)
	buf[18] = c.Heads
	buf[19] = c.Sectors
	binary.LittleEndian.PutUint32(buf[20:24], c.BlkSize)
	buf[24] = c.PhysBlkExp
	buf[25] = c.AlignOffs
	binary.LittleEndian.PutUint16(buf[26:28], c.MinIOSize)
	binary.LittleEndian.PutUint32(buf[28:32], c.OptIOSize)
	buf[32] = c.Writeback
	buf[33] = c.Unused0
	binary.LittleEndian.PutUint16(buf[34:36], c.NumQueues)
	binary.LittleEndian.PutUint32(buf[36:40], c.MaxDiscSec)
	binary.LittleEndian.PutUint32(buf[40:44], c.MaxDiscSeg)
	binary.LittleEndian.PutUint32(buf[44:48], c.DiscSecAln)
	binary.LittleEndian.PutUint32(buf[48:52], c.MaxWrZSec)
	binary.LittleEndian.PutUint32(buf[52:56], c.MaxWrZSeg)
	buf[56] = c.WrZeroMayU
	// Unused1 is 3 bytes of padding (left zero).
	return buf
}

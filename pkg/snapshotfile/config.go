package snapshotfile

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/tailzip"
)

// ReadConfig reads only snapshot.cfg and the bounded ZIP headers required to
// locate it. It validates the canonical three-entry layout, limits and the
// selected entry's CRC. CPU/state bodies belong to execution restore, not a
// checkpoint keep graph; they and the memory payload are never read here.
// The caller retains stream ownership. Carrier authentication/retries remain
// those of the same Stream and tailReaderAt used by Open.
func ReadConfig(ctx context.Context, stream fetch.Stream) ([]byte, error) {
	if stream == nil {
		return nil, errors.New("snapshot config: nil stream")
	}
	footer, err := tailzip.ReadFooter(ctx, tailReaderAt{ctx: ctx, stream: stream}, stream.Size())
	if err != nil {
		return nil, err
	}
	names := []string{ConfigJSONName, StateJSONName, SnapshotCfgName}
	centralSize := uint64(0)
	for _, name := range names {
		centralSize += centralHeaderSize + uint64(len(name))
	}
	if footer.Base == 0 || footer.Count != len(names) || footer.CentralSize != centralSize {
		return nil, errors.New("snapshot config: invalid ZIP geometry")
	}
	central, err := readAt(ctx, stream, footer.CentralStart, footer.CentralSize)
	if err != nil {
		return nil, err
	}
	local, pos := footer.Base, uint64(0)
	var cfg []byte
	for _, name := range names {
		h := central[pos : pos+centralHeaderSize]
		u16 := func(n int) uint16 { return binary.LittleEndian.Uint16(h[n:]) }
		u32 := func(n int) uint32 { return binary.LittleEndian.Uint32(h[n:]) }
		if u32(0) != centralHeaderSignature || u16(4) != 20 || u16(6) != 20 || u16(8) != 0 || u16(10) != 0 ||
			u16(12) != 0 || u16(14) != zipEpochDate || u16(28) != uint16(len(name)) || u16(30) != 0 || u16(32) != 0 ||
			u16(34) != 0 || u16(36) != 0 || u32(38) != 0 || string(central[pos+centralHeaderSize:pos+centralHeaderSize+uint64(len(name))]) != name {
			return nil, errors.New("snapshot config: noncanonical central header")
		}
		size := uint64(u32(24))
		if size == 0 || size > uint64(entryLimit(name)) || uint64(u32(20)) != size || uint64(u32(42)) != local-footer.Base {
			return nil, errors.New("snapshot config: invalid entry size/offset")
		}
		headerSize := uint64(localHeaderSize + len(name))
		if local > footer.CentralStart || headerSize > footer.CentralStart-local || size > footer.CentralStart-local-headerSize {
			return nil, errors.New("snapshot config: entry overlaps central directory")
		}
		header, err := readAt(ctx, stream, local, headerSize)
		if err != nil {
			return nil, err
		}
		if binary.LittleEndian.Uint32(header) != localHeaderSignature || !bytes.Equal(header[4:26], h[6:28]) ||
			binary.LittleEndian.Uint16(header[26:]) != uint16(len(name)) || binary.LittleEndian.Uint16(header[28:]) != 0 || string(header[30:]) != name {
			return nil, errors.New("snapshot config: local/central header mismatch")
		}
		if name == SnapshotCfgName {
			cfg, err = readAt(ctx, stream, local+headerSize, size)
			if err != nil {
				return nil, err
			}
			if crc32.ChecksumIEEE(cfg) != u32(16) {
				return nil, errors.New("snapshot config: CRC mismatch")
			}
		}
		local += headerSize + size
		pos += centralHeaderSize + uint64(len(name))
	}
	if local != footer.CentralStart {
		return nil, errors.New("snapshot config: gap before central directory")
	}
	return cfg, nil
}

package emptyext4

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// These values freeze the independently fsck/kernel-validated v1 profile,
// including the authoritative sparse map. No hole payload is read to hash it.
func TestProfileGoldenContentAndLayout(t *testing.T) {
	for _, tc := range []struct {
		size         int64
		layout, data string
	}{
		{64 << 20, "0a2c995ddadcc25300a9c11c14205c4559348c443e365d10ac4691d00fee4309", "cbb67da61331e1413713102cd6fb33bf9d43d8dd4aba865e686729ed83a9342c"},
		{32 << 30, "6205fd60596f1e64e9ee126bc66fea32d805b146fef423c800e23ec1097fa82f", "57e768a3e4b2dce7a8ac6be041c82fbda87ec2929af9765a2dd120ee4aa08280"},
		{1 << 40, "36ab5aba006ce660d1bb69903dbb76cbadc9e299d87c5cfff81520c47c3bc38d", "9b681d6120d97e343ebcaae64601ecc9d6330f5f9cc06e8443968824b208a260"},
	} {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			s, err := New(tc.size)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			layout, data := sha256.New(), sha256.New()
			var description [17]byte
			buffer := make([]byte, 128*1024)
			for offset := uint64(0); offset < s.Size(); {
				run, err := s.RunAt(offset, s.Size()-offset)
				if err != nil {
					t.Fatal(err)
				}
				description[0] = byte(run.Kind())
				binary.LittleEndian.PutUint64(description[1:9], offset)
				binary.LittleEndian.PutUint64(description[9:], run.End())
				layout.Write(description[:])
				if run.Kind() == sparse.Data {
					for at := offset; at < run.End(); {
						n := min(uint64(len(buffer)), run.End()-at)
						if _, err := s.ReadAt(context.Background(), buffer[:n], at); err != nil {
							t.Fatal(err)
						}
						data.Write(buffer[:n])
						at += n
					}
				}
				offset = run.End()
			}
			if got := hex.EncodeToString(layout.Sum(nil)); got != tc.layout {
				t.Fatalf("%s sparse layout changed: %s", Profile, got)
			}
			if got := hex.EncodeToString(data.Sum(nil)); got != tc.data {
				t.Fatalf("%s logical data changed: %s", Profile, got)
			}
		})
	}
}

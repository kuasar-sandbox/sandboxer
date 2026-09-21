package snapshotfile

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

type configReadStream struct {
	sparse.Source
	forbiddenStart, forbiddenEnd uint64
	readBytes                    int
}

func (s *configReadStream) Close() error { return nil }
func (s *configReadStream) ReadAt(ctx context.Context, b []byte, off uint64) (int, error) {
	if off < s.forbiddenEnd && off+uint64(len(b)) > s.forbiddenStart {
		return 0, errors.New("read execution-state body")
	}
	s.readBytes += len(b)
	return s.Source.ReadAt(ctx, b, off)
}

func TestReadConfigSkipsMemoryAndExecutionBodies(t *testing.T) {
	memory := sparse.Dense(bytes.NewReader(make([]byte, 4096)), 4096)
	configBody := []byte(`{"state":"` + string(bytes.Repeat([]byte{'x'}, 1<<20)) + `"}`)
	source, err := BuildSource(memory, configBody, configBody, []byte("version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	// The forbidden interval spans config.json's complete body. Other headers
	// must still be validated without reading any bytes from that body.
	start := uint64(4096 + localHeaderSize + len(ConfigJSONName))
	stream := &configReadStream{Source: source, forbiddenStart: start, forbiddenEnd: start + uint64(len(configBody))}
	got, err := ReadConfig(context.Background(), stream)
	if err != nil || string(got) != "version: 1\n" {
		t.Fatalf("config read: %q %v", got, err)
	}
	t.Logf("ReadConfig: %d bytes read; memory and two %d-byte execution bodies excluded", stream.readBytes, len(configBody))
	if stream.readBytes > 1024 {
		t.Fatalf("read %d bytes for tiny keep metadata", stream.readBytes)
	}
}

func TestReadConfigRejectsMalformedHeaderAndCRC(t *testing.T) {
	tail, err := BuildZIP([]byte("{}"), []byte("{}"), []byte("version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"crc", "local", "count", "central"} {
		t.Run(mutation, func(t *testing.T) {
			body := append(make([]byte, 4096), tail...)
			switch mutation {
			case "crc":
				body[4096+30+len(ConfigJSONName)+2+30+len(StateJSONName)+2+30+len(SnapshotCfgName)] ^= 1
			case "local":
				body[4096+4] ^= 1
			case "count":
				binary.LittleEndian.PutUint16(body[len(body)-12:], 2)
			case "central":
				body[len(body)-22-46-len(SnapshotCfgName)+8] = 1
			}
			s := &configReadStream{Source: sparse.Dense(bytes.NewReader(body), uint64(len(body)))}
			if _, err := ReadConfig(context.Background(), s); err == nil {
				t.Fatal("malformed metadata accepted")
			}
		})
	}
}

// Measure bounded metadata reads across very different execution-tail sizes.
// Setup is excluded; no guest runs or freeze intervals are measured here.
func BenchmarkReadConfigMetadata(b *testing.B) {
	for _, size := range []int{1024, 1 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("state-%dKiB", size/1024), func(b *testing.B) {
			memory := sparse.Dense(bytes.NewReader(make([]byte, 4096)), 4096)
			body := []byte(`{"state":"` + string(bytes.Repeat([]byte{'x'}, size)) + `"}`)
			source, err := BuildSource(memory, body, body, []byte("version: 1\n"))
			if err != nil {
				b.Fatal(err)
			}
			start := uint64(4096 + localHeaderSize + len(ConfigJSONName))
			stream := &configReadStream{Source: source, forbiddenStart: start, forbiddenEnd: start + uint64(len(body))}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := ReadConfig(context.Background(), stream); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(stream.readBytes)/float64(b.N), "read-B/op")
		})
	}
}

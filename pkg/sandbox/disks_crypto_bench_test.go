package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

func BenchmarkOpenDiskStreamRandomRead(b *testing.B) {
	const artifactSize = 32 << 20
	payload := bytes.Repeat([]byte("kuasar-local-crypto"), artifactSize/len("kuasar-local-crypto")+1)[:artifactSize]
	codec, err := manifestcrypto.NewTarStreamCodec([32]byte{0x8a})
	if err != nil {
		b.Fatal(err)
	}
	plainPath, plainScheme, plainDigest := writeBenchDiskArtifact(b, b.TempDir(), payload, nil)
	encryptedPath, encryptedScheme, encryptedDigest := writeBenchDiskArtifact(b, b.TempDir(), payload, codec)
	tests := []struct {
		name     string
		path     string
		scheme   string
		digest   string
		codec    tarstream.Codec
		required bool
	}{
		{name: "plaintext-off", path: plainPath, scheme: plainScheme, digest: plainDigest},
		{name: "plaintext-auto", path: plainPath, scheme: plainScheme, digest: plainDigest, codec: codec},
		{name: "encrypted-auto", path: encryptedPath, scheme: encryptedScheme, digest: encryptedDigest, codec: codec},
		{name: "encrypted-required", path: encryptedPath, scheme: encryptedScheme, digest: encryptedDigest, codec: codec, required: true},
	}
	for _, tc := range tests {
		for _, blockSize := range []int{4 << 10, 1 << 20} {
			b.Run(tc.name+"/"+benchBlockName(blockSize), func(b *testing.B) {
				ref := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: tc.path, DigestScheme: tc.scheme, Digest: tc.digest}
				stream, _, err := OpenDiskStream(context.Background(), ref.String(), nil, nil, tc.codec, tc.required)
				if err != nil {
					b.Fatal(err)
				}
				defer stream.Close()
				buf := make([]byte, blockSize)
				maxOffset := uint64(artifactSize - blockSize)
				b.SetBytes(int64(blockSize))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					offset := (uint64(i) * 104729) % (maxOffset + 1)
					if n, err := stream.ReadAt(context.Background(), buf, offset); err != nil || n != len(buf) {
						b.Fatalf("ReadAt=%d err=%v", n, err)
					}
				}
			})
		}
	}
}

func writeBenchDiskArtifact(b *testing.B, dir string, payload []byte, codec tarstream.Codec) (string, string, string) {
	b.Helper()
	f, err := os.CreateTemp(dir, "artifact-*.tmp")
	if err != nil {
		b.Fatal(err)
	}
	var options []tarstream.WriteOption
	if codec != nil {
		options = append(options, tarstream.WithCodec(codec, false))
	}
	scheme, digest, err := tarstream.WriteTo(context.Background(), f, "image", sparse.Dense(bytes.NewReader(payload), uint64(len(payload))), options...)
	if err != nil {
		_ = f.Close()
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(dir, digest+".image")
	if err := os.Rename(f.Name(), path); err != nil {
		b.Fatal(err)
	}
	return path, scheme, digest
}

func benchBlockName(size int) string {
	if size == 4<<10 {
		return "4KiB"
	}
	return "1MiB"
}

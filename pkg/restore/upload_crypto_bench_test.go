package restore

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

func BenchmarkSequentialTarValidation(b *testing.B) {
	const artifactSize = 32 << 20
	payload := bytes.Repeat([]byte{0x5d}, artifactSize)
	codec, err := manifestcrypto.NewTarStreamCodec([32]byte{0x9b})
	if err != nil {
		b.Fatal(err)
	}
	plain := writeBenchRestoreArtifact(b, b.TempDir(), payload, nil)
	encrypted := writeBenchRestoreArtifact(b, b.TempDir(), payload, codec)
	tests := []struct {
		name     string
		artifact benchRestoreArtifact
		codec    tarstream.Codec
		required bool
	}{
		{name: "plaintext-off", artifact: plain},
		{name: "plaintext-auto", artifact: plain, codec: codec},
		{name: "encrypted-auto", artifact: encrypted, codec: codec},
		{name: "encrypted-required", artifact: encrypted, codec: codec, required: true},
	}
	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			ref := manifest.Ref{
				Scheme: manifest.RefSchemeFile, Path: tc.artifact.path,
				DigestScheme: tc.artifact.scheme, Digest: tc.artifact.digest,
			}
			b.SetBytes(artifactSize)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				source, closer, _, _, _, err := openSequentialTarArtifact(tc.artifact.path, "image", ref, tc.codec, tc.required)
				if err != nil {
					b.Fatal(err)
				}
				if err := consumeSource(context.Background(), source, 0); err != nil {
					_ = closer.Close()
					b.Fatal(err)
				}
				if err := closer.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type benchRestoreArtifact struct {
	path, scheme, digest string
}

func writeBenchRestoreArtifact(b *testing.B, dir string, payload []byte, codec tarstream.Codec) benchRestoreArtifact {
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
	return benchRestoreArtifact{path: path, scheme: scheme, digest: digest}
}

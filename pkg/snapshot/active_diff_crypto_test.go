package snapshot

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

func TestEncryptedActiveDiffSnapshotToEncryptedFileSink(t *testing.T) {
	ctx := context.Background()
	var key [32]byte
	key[0] = 0x91
	codec, err := manifestcrypto.NewTarStreamCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	const size = 2 * 4096
	activePath := filepath.Join(t.TempDir(), "runtime-active.diff")
	cow, err := vhost.OpenBlockCOW(
		activePath,
		nil,
		vhost.DiffInit{CreateSize: size},
		vhost.WithCodec(codec, true),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()
	payload := bytes.Repeat([]byte{0x5c}, 512)
	if _, err := cow.WriteAt(payload, 512); err != nil {
		t.Fatal(err)
	}

	view, holes, err := cow.SnapshotView()
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	refRaw, artifactPath, err := NewFileSink(outDir, "encrypted-active", codec, true, nil).
		AbsorbOverlay(ctx, view, holes)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef(refRaw)
	if err != nil {
		t.Fatal(err)
	}
	if ref.DigestScheme != tarstream.DigestSchemeHMAC || filepath.Base(artifactPath) != ref.Digest+".overlay" {
		t.Fatalf("snapshot ref=%#v path=%s", ref, artifactPath)
	}
	if _, err := fetch.OpenTarStream(artifactPath); err == nil {
		t.Fatal("encrypted snapshot artifact opened without a codec")
	}
	stream, err := fetch.OpenTarStream(
		artifactPath,
		tarstream.WithCodec(codec, true),
		tarstream.WithExpectedDigest(ref.DigestScheme, ref.Digest),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := make([]byte, size)
	if n, err := stream.ReadAt(ctx, got, 0); err != nil || n != len(got) {
		t.Fatalf("ReadAt=%d err=%v", n, err)
	}
	want := make([]byte, size)
	copy(want[512:], payload)
	if !bytes.Equal(got, want) {
		t.Fatal("snapshot artifact did not contain the decrypted upper-only view")
	}
	if matches, _ := filepath.Glob(filepath.Join(outDir, "*.partial")); len(matches) != 0 {
		t.Fatalf("snapshot left plaintext staging candidates: %v", matches)
	}
	active, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(active[4096:], payload) {
		t.Fatal("active diff body contains guest plaintext")
	}
}

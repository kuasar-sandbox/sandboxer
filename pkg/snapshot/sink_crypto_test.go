package snapshot

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

func TestEncryptedFileSinkDeterministicAndNoReplace(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("encrypted-snapshot"), 8192)
	key := [32]byte{0x24, 0x25}
	codec, err := manifestcrypto.NewTarStreamCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(sid string, required bool) (string, string, error) {
		return NewFileSink(dir, sid, codec, required, nil).AbsorbOverlay(ctx, bytes.NewReader(payload), nil)
	}
	ref, path, err := write("first", false)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DigestScheme != tarstream.DigestSchemeHMAC || filepath.Base(path) != parsed.Digest+".overlay" {
		t.Fatalf("encrypted ref=%#v path=%s", parsed, path)
	}
	if _, err := fetch.OpenTarStream(path); !errors.Is(err, tarstream.ErrCodecRequired) {
		t.Fatalf("encrypted artifact without codec error = %v", err)
	}
	stream, err := fetch.OpenTarStream(path,
		tarstream.WithCodec(codec, true),
		tarstream.WithExpectedDigest(parsed.DigestScheme, parsed.Digest),
	)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if n, err := stream.ReadAt(ctx, got, 0); n != len(got) || err != nil {
		_ = stream.Close()
		t.Fatalf("ReadAt=%d err=%v", n, err)
	}
	_ = stream.Close()
	if !bytes.Equal(got, payload) {
		t.Fatal("encrypted FileSink payload mismatch")
	}

	firstBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ref2, path2, err := write("second", true)
	if err != nil {
		t.Fatalf("reuse existing final: %v", err)
	}
	if ref2 != ref || path2 != path {
		t.Fatalf("reuse ref/path = %q %q, want %q %q", ref2, path2, ref, path)
	}
	secondBytes, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("required changed deterministic encrypted encoding")
	}
	partials, err := filepath.Glob(filepath.Join(dir, "*.partial"))
	if err != nil || len(partials) != 0 {
		t.Fatalf("partial files=%v err=%v", partials, err)
	}
}

func TestEncryptedFileSinkRejectsInconsistentExistingFinal(t *testing.T) {
	ctx := context.Background()
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x33})
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0x6a}, 32*1024)
	sink := NewFileSink(dir, "first", codec, false, nil)
	_, path, err := sink.AbsorbOverlay(ctx, bytes.NewReader(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/2] ^= 0x80
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), body...)
	_, _, err = NewFileSink(dir, "second", codec, false, nil).AbsorbOverlay(ctx, bytes.NewReader(payload), nil)
	if err == nil {
		t.Fatal("inconsistent existing final was reused")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("inconsistent existing final was overwritten")
	}
	partials, _ := filepath.Glob(filepath.Join(dir, "*.partial"))
	if len(partials) != 0 {
		t.Fatalf("failed reuse left partial files: %v", partials)
	}
}

func TestEncryptedFileSinkRejectsPlaintextExistingFinalInAuto(t *testing.T) {
	ctx := context.Background()
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x34})
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0x6b}, 32*1024)

	plainRef, plainPath, err := NewFileSink(dir, "plain", nil, false, nil).AbsorbOverlay(ctx, bytes.NewReader(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.ParseRef(plainRef)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(parsed.Digest)
	if err != nil {
		t.Fatal(err)
	}
	var raw [32]byte
	copy(raw[:], decoded)
	keyed := codec.KeyedDigest(raw)
	final := filepath.Join(dir, hex.EncodeToString(keyed[:])+".overlay")
	if err := os.Rename(plainPath, final); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := NewFileSink(dir, "encrypted", codec, false, nil).AbsorbOverlay(ctx, bytes.NewReader(payload), nil); !errors.Is(err, tarstream.ErrPlaintextForbidden) {
		t.Fatalf("auto plaintext collision error=%v", err)
	}
	after, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("plaintext existing final was modified")
	}
}

func TestFileSinkUsesUniqueTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "same.overlay.partial")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, final, err := NewFileSink(dir, "same", nil, false, nil).AbsorbOverlay(
		context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x71}, 4096)), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(stale); err != nil || string(body) != "stale" {
		t.Fatalf("stale partial changed: %q err=%v", body, err)
	}
	info, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("final mode = %o, want 644", got)
	}
}

func TestFileSinkPolicyAndKeyIdentity(t *testing.T) {
	payload := bytes.Repeat([]byte{0x7b}, 4096)
	ctx := context.Background()
	if _, _, err := NewFileSink(t.TempDir(), "sid", nil, true, nil).AbsorbOverlay(ctx, bytes.NewReader(payload), nil); err == nil {
		t.Fatal("required FileSink accepted a nil codec")
	}
	codecA, _ := manifestcrypto.NewTarStreamCodec([32]byte{1})
	codecB, _ := manifestcrypto.NewTarStreamCodec([32]byte{2})
	refA, pathA, err := NewFileSink(t.TempDir(), "a", codecA, false, nil).AbsorbOverlay(ctx, bytes.NewReader(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	refB, pathB, err := NewFileSink(t.TempDir(), "b", codecB, false, nil).AbsorbOverlay(ctx, bytes.NewReader(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	if refA == refB {
		t.Fatal("different customer keys produced the same external identity")
	}
	bodyA, _ := os.ReadFile(pathA)
	bodyB, _ := os.ReadFile(pathB)
	if bytes.Equal(bodyA, bodyB) {
		t.Fatal("different customer keys produced identical ciphertext")
	}
}

func TestEncryptedFileSinkRejectsSymlinkFinal(t *testing.T) {
	ctx := context.Background()
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x44})
	payload := bytes.Repeat([]byte{0x19}, 16*1024)
	_, target, err := NewFileSink(t.TempDir(), "source", codec, false, nil).AbsorbOverlay(ctx, bytes.NewReader(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, filepath.Base(target))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewFileSink(dir, "destination", codec, false, nil).AbsorbOverlay(ctx, bytes.NewReader(payload), nil); err == nil {
		t.Fatal("FileSink reused a symlink final")
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink final changed: info=%v err=%v", info, err)
	}
}

package vhost

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

func TestBlockCOWCodecOptions(t *testing.T) {
	codec := testDiffCodec(t, 0x11)
	for _, test := range []struct {
		name    string
		options []BlockCOWOption
	}{
		{name: "nil codec", options: []BlockCOWOption{WithCodec(nil, false)}},
		{name: "duplicate codec", options: []BlockCOWOption{WithCodec(codec, false), WithCodec(codec, true)}},
		{name: "nil option", options: []BlockCOWOption{nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "diff")
			if _, err := OpenBlockCOW(path, nil, DiffInit{CreateSize: cowBlockSize}, test.options...); err == nil {
				t.Fatal("invalid option was accepted")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid option created target: %v", err)
			}
		})
	}
}

func TestEncryptedBlockCOWPolicyAndReopen(t *testing.T) {
	const size = 4 * cowBlockSize
	dir := t.TempDir()
	path := filepath.Join(dir, "active.diff")
	baseData := patternedBytes(size)
	base := &fakeReader{data: baseData}
	codec := testDiffCodec(t, 0x21)

	cow, err := OpenBlockCOW(path, base, DiffInit{CreateSize: size}, WithCodec(codec, false))
	if err != nil {
		t.Fatal(err)
	}
	if !cow.diff.encrypted || cow.Size() != size || cow.DirtyCount() != 0 {
		t.Fatalf("fresh encrypted diff: encrypted=%t size=%d dirty=%d", cow.diff.encrypted, cow.Size(), cow.DirtyCount())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != diffHeaderRegionSize+size {
		t.Fatalf("physical size=%d want=%d", info.Size(), diffHeaderRegionSize+size)
	}

	payload := bytes.Repeat([]byte{0xa5}, 512)
	const writeOffset = cowBlockSize + 512
	if n, err := cow.WriteAt(payload, writeOffset); err != nil || n != len(payload) {
		t.Fatalf("WriteAt=%d err=%v", n, err)
	}
	want := append([]byte(nil), baseData...)
	copy(want[writeOffset:], payload)
	got := make([]byte, size)
	if n, err := cow.ReadAt(got, 0); err != nil || n != len(got) {
		t.Fatalf("ReadAt=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("encrypted COW logical view mismatch")
	}
	if err := cow.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := cow.Close(); err != nil {
		t.Fatal(err)
	}

	physical, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(physical[:len(diffMagic)], diffMagic[:]) {
		t.Fatalf("encrypted magic=%x", physical[:len(diffMagic)])
	}
	bodyBlock := physical[diffHeaderRegionSize+cowBlockSize : diffHeaderRegionSize+2*cowBlockSize]
	if bytes.Equal(bodyBlock, want[cowBlockSize:2*cowBlockSize]) {
		t.Fatal("encrypted body stored plaintext")
	}

	if _, err := OpenBlockCOW(path, base, DiffInit{Existing: true}); !errors.Is(err, tarstream.ErrCodecRequired) {
		t.Fatalf("off encrypted open error=%v", err)
	}
	wrong := testDiffCodec(t, 0x22)
	if _, err := OpenBlockCOW(path, base, DiffInit{Existing: true}, WithCodec(wrong, true)); !errors.Is(err, tarstream.ErrAuthentication) {
		t.Fatalf("wrong key error=%v", err)
	}
	reopened, err := OpenBlockCOW(path, base, DiffInit{Existing: true}, WithCodec(codec, true))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.DirtyCount() != 1 {
		t.Fatalf("reopened dirty=%d want=1", reopened.DirtyCount())
	}
	clear(got)
	if _, err := reopened.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("reopened encrypted COW logical view mismatch")
	}

	plainPath := filepath.Join(dir, "legacy.diff")
	plain, err := OpenBlockCOW(plainPath, nil, DiffInit{CreateSize: size})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.WriteAt(bytes.Repeat([]byte{0x42}, cowBlockSize), 0); err != nil {
		t.Fatal(err)
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(plainPath)
	auto, err := OpenBlockCOW(plainPath, nil, DiffInit{Existing: true}, WithCodec(codec, false))
	if err != nil {
		t.Fatalf("auto plaintext: %v", err)
	}
	if auto.diff.encrypted {
		t.Fatal("auto rewrote an existing plaintext diff")
	}
	if err := auto.Close(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(plainPath)
	if !bytes.Equal(after, before) {
		t.Fatal("auto modified an existing plaintext diff")
	}
	if _, err := OpenBlockCOW(plainPath, nil, DiffInit{Existing: true}, WithCodec(codec, true)); !errors.Is(err, tarstream.ErrPlaintextForbidden) {
		t.Fatalf("required plaintext error=%v", err)
	}
}

func TestEncryptedDiffHeaderValidation(t *testing.T) {
	codec := testDiffCodec(t, 0x31)
	create := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "active.diff")
		cow, err := OpenBlockCOW(path, nil, DiffInit{CreateSize: cowBlockSize}, WithCodec(codec, false))
		if err != nil {
			t.Fatal(err)
		}
		if err := cow.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	open := func(path string, required bool) error {
		cow, err := OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithCodec(codec, required))
		if cow != nil {
			_ = cow.Close()
		}
		return err
	}

	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{name: "version", mutate: func(t *testing.T, path string) {
			mutateDiffBytes(t, path, func(body []byte) { binaryPutUint16(body[8:10], 2) })
		}},
		{name: "prefix-size", mutate: func(t *testing.T, path string) {
			mutateDiffBytes(t, path, func(body []byte) { binaryPutUint16(body[10:12], 15) })
		}},
		{name: "flags", mutate: func(t *testing.T, path string) { mutateDiffBytes(t, path, func(body []byte) { body[15] = 1 }) }},
		{name: "authentication", mutate: func(t *testing.T, path string) {
			mutateDiffBytes(t, path, func(body []byte) { body[diffPrefixSize+7] ^= 0x80 })
		}},
		{name: "padding", mutate: func(t *testing.T, path string) { mutateDiffBytes(t, path, func(body []byte) { body[300] = 1 }) }},
		{name: "truncated", mutate: func(t *testing.T, path string) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, info.Size()-1); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "appended", mutate: func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte{0}); err != nil {
				_ = f.Close()
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := create(t)
			test.mutate(t, path)
			if err := open(path, false); err == nil {
				t.Fatal("damaged encrypted diff was accepted")
			}
		})
	}

	for _, test := range []struct {
		name   string
		mutate func([]byte)
	}{
		{name: "logical-size", mutate: func(header []byte) { clear(header[0:8]) }},
		{name: "block-size", mutate: func(header []byte) { header[11] ^= 1 }},
		{name: "data-unit-size", mutate: func(header []byte) { header[15] ^= 1 }},
		{name: "body-offset", mutate: func(header []byte) { header[19] ^= 1 }},
		{name: "key-size", mutate: func(header []byte) { header[23] ^= 1 }},
		{name: "reserved", mutate: func(header []byte) { header[88] = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := create(t)
			rewrapDiffHeader(t, path, codec, test.mutate)
			if err := open(path, false); err == nil {
				t.Fatal("invalid authenticated metadata was accepted")
			}
		})
	}

	t.Run("magic-required", func(t *testing.T) {
		path := create(t)
		mutateDiffBytes(t, path, func(body []byte) { body[0] ^= 1 })
		if err := open(path, true); !errors.Is(err, tarstream.ErrPlaintextForbidden) {
			t.Fatalf("required damaged magic error=%v", err)
		}
	})
}

func TestEncryptedDiffGoldenAndRandomKeys(t *testing.T) {
	codec := testDiffCodec(t, 0x41)
	rawKey := mustDecodeHex(t, "27182818284590452353602874713526624977572470936999595749669676273141592653589793238462643383279502884197169399375105820974944592")
	path := filepath.Join(t.TempDir(), "golden.diff")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const vectorSector = int64(0xff)
	const goldenSize = (vectorSector + 1) * diffDataUnitSize
	diff, err := createEncryptedDiffFile(f, goldenSize, codec, bytes.NewReader(rawKey))
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	plaintext := make([]byte, diffDataUnitSize)
	for i := range plaintext {
		plaintext[i] = byte(i)
	}
	vectorOffset := vectorSector * diffDataUnitSize
	if _, err := diff.WriteAt(plaintext, vectorOffset); err != nil {
		t.Fatal(err)
	}
	if err := diff.Sync(); err != nil {
		t.Fatal(err)
	}
	physical, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const wantXTSFirst32 = "1c3b3a102f770386e4836c99e370cf9bea00803f5e482357a4ae12d414a3e63b"
	if got := hex.EncodeToString(physical[diffHeaderRegionSize+vectorOffset : diffHeaderRegionSize+vectorOffset+32]); got != wantXTSFirst32 {
		t.Fatalf("AES-256-XTS vector prefix=%s want=%s", got, wantXTSFirst32)
	}
	gotPlaintext := make([]byte, len(plaintext))
	if _, err := diff.ReadAt(gotPlaintext, vectorOffset); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPlaintext, plaintext) {
		t.Fatal("AES-256-XTS round trip failed")
	}
	const wantHeader = "894b44585453310a00010010000000000170cd897df89cc598b26de657afb7b70b74d8d642451e27f413a9db206efb46a51c2090af40fd8401a2e25f681a19320b5bc87531fb226083b31876d4c89902deb97766b2a600065e3f92ba3323a39a72dce34d4ef9f5e1f59201522f607b0e43cb11d0f1fc948e49f35f8d7dd1f09d2f8f93d97561147dfce80130e919d719c496e7703c11bb326cc2c9479f61c7f42c"
	gotHeader := hex.EncodeToString(physical[:diffPrefixSize+codec.CiphertextSize(diffHeaderPlainSize)])
	if gotHeader != wantHeader {
		t.Fatalf("encrypted diff header=%s want=%s", gotHeader, wantHeader)
	}
	if err := diff.Close(); err != nil {
		t.Fatal(err)
	}

	paths := []string{filepath.Join(t.TempDir(), "a.diff"), filepath.Join(t.TempDir(), "b.diff")}
	files := make([][]byte, len(paths))
	for i, candidate := range paths {
		cow, err := OpenBlockCOW(candidate, nil, DiffInit{CreateSize: cowBlockSize}, WithCodec(codec, false))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cow.WriteAt(bytes.Repeat([]byte{0x55}, cowBlockSize), 0); err != nil {
			t.Fatal(err)
		}
		if err := cow.Close(); err != nil {
			t.Fatal(err)
		}
		files[i], err = os.ReadFile(candidate)
		if err != nil {
			t.Fatal(err)
		}
	}
	if bytes.Equal(files[0][:diffHeaderRegionSize], files[1][:diffHeaderRegionSize]) {
		t.Fatal("independent encrypted diffs reused the same wrapped header/key")
	}
	if bytes.Equal(files[0][diffHeaderRegionSize:], files[1][diffHeaderRegionSize:]) {
		t.Fatal("independent encrypted diffs produced equal body ciphertext")
	}
}

func TestEncryptedBlockCOWArbitraryIOAndShortIO(t *testing.T) {
	const size = 4 * cowBlockSize
	baseData := patternedBytes(size)
	cow, err := OpenBlockCOW(
		filepath.Join(t.TempDir(), "active.diff"),
		&fakeReader{data: baseData},
		DiffInit{CreateSize: size},
		WithCodec(testDiffCodec(t, 0x51), false),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()
	want := append([]byte(nil), baseData...)
	updates := []struct {
		offset int64
		body   []byte
	}{
		{offset: 123, body: bytes.Repeat([]byte{0x11}, 257)},
		{offset: 600, body: bytes.Repeat([]byte{0x22}, 99)},
		{offset: cowBlockSize - 100, body: bytes.Repeat([]byte{0x33}, 300)},
		{offset: 2 * cowBlockSize, body: bytes.Repeat([]byte{0x44}, 2*cowBlockSize)},
		{offset: 2*cowBlockSize + 777, body: bytes.Repeat([]byte{0x55}, 513)},
	}
	for _, update := range updates {
		if n, err := cow.WriteAt(update.body, update.offset); err != nil || n != len(update.body) {
			t.Fatalf("WriteAt(%d)=%d err=%v", update.offset, n, err)
		}
		copy(want[update.offset:], update.body)
	}
	for _, read := range []struct {
		offset int64
		length int
	}{{0, 17}, {119, 777}, {cowBlockSize - 333, 999}, {2*cowBlockSize + 511, 4097}} {
		got := make([]byte, read.length)
		if n, err := cow.ReadAt(got, read.offset); err != nil || n != len(got) {
			t.Fatalf("ReadAt(%d)=%d err=%v", read.offset, n, err)
		}
		if !bytes.Equal(got, want[read.offset:read.offset+int64(read.length)]) {
			t.Fatalf("arbitrary read mismatch at %d", read.offset)
		}
	}

	failed, err := OpenBlockCOW(
		filepath.Join(t.TempDir(), "short.diff"),
		&fakeReader{data: baseData[:cowBlockSize]},
		DiffInit{CreateSize: cowBlockSize},
		WithCodec(testDiffCodec(t, 0x52), false),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer failed.Close()
	failed.diff.bodyIO = shortDiffBodyIO{inner: failed.diff.bodyIO, shortWrite: true}
	if _, err := failed.WriteAt([]byte("partial"), 17); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short encrypted write error=%v", err)
	}
	if failed.blockDirty(0) {
		t.Fatal("short encrypted materialization marked block dirty")
	}
	failed.diff.bodyIO = failed.diff.f
	if _, err := failed.WriteAt(bytes.Repeat([]byte{0x66}, cowBlockSize), 0); err != nil {
		t.Fatal(err)
	}
	failed.diff.bodyIO = shortDiffBodyIO{inner: failed.diff.bodyIO, shortRead: true}
	if _, err := failed.ReadAt(make([]byte, 512), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short encrypted read error=%v", err)
	}
}

func TestEncryptedBlockCOWConcurrentOverlappingWrites(t *testing.T) {
	const blocks = 64
	const size = blocks * cowBlockSize
	baseData := patternedBytes(size)
	cow, err := OpenBlockCOW(
		filepath.Join(t.TempDir(), "active.diff"),
		&fakeReader{data: baseData},
		DiffInit{CreateSize: size},
		WithCodec(testDiffCodec(t, 0x53), false),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()

	start := make(chan struct{})
	var wait sync.WaitGroup
	for block := 0; block < blocks; block++ {
		offset := int64(block * cowBlockSize)
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			if _, err := cow.WriteAt(bytes.Repeat([]byte{'x'}, 1024), offset); err != nil {
				t.Errorf("first overlapping write: %v", err)
			}
		}()
		go func() {
			defer wait.Done()
			<-start
			if _, err := cow.WriteAt(bytes.Repeat([]byte{'y'}, 1024), offset+512); err != nil {
				t.Errorf("second overlapping write: %v", err)
			}
		}()
	}
	close(start)
	wait.Wait()

	for block := 0; block < blocks; block++ {
		offset := block * cowBlockSize
		got := make([]byte, cowBlockSize)
		if _, err := cow.ReadAt(got, int64(offset)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[:512], bytes.Repeat([]byte{'x'}, 512)) {
			t.Fatalf("block %d lost first writer's non-overlap", block)
		}
		overlap := got[512:1024]
		if !bytes.Equal(overlap, bytes.Repeat([]byte{'x'}, 512)) &&
			!bytes.Equal(overlap, bytes.Repeat([]byte{'y'}, 512)) {
			t.Fatalf("block %d has torn encrypted overlap", block)
		}
		if !bytes.Equal(got[1024:1536], bytes.Repeat([]byte{'y'}, 512)) {
			t.Fatalf("block %d lost second writer's non-overlap", block)
		}
		if !bytes.Equal(got[1536:], baseData[offset+1536:offset+cowBlockSize]) {
			t.Fatalf("block %d lost untouched base tail", block)
		}
	}
}

func TestEncryptedBitmapRebuildSkipsHeaderAndPreservesSparseBlocks(t *testing.T) {
	const size = 6 * cowBlockSize
	path := filepath.Join(t.TempDir(), "active.diff")
	codec := testDiffCodec(t, 0x54)
	cow, err := OpenBlockCOW(path, nil, DiffInit{CreateSize: size}, WithCodec(codec, false))
	if err != nil {
		t.Fatal(err)
	}
	if cow.DirtyCount() != 0 {
		t.Fatalf("header marked body dirty: %d", cow.DirtyCount())
	}
	for _, block := range []int64{1, 4} {
		if _, err := cow.WriteAt(bytes.Repeat([]byte{byte(block)}, cowBlockSize), block*cowBlockSize); err != nil {
			t.Fatal(err)
		}
	}
	if err := cow.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := cow.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithCodec(codec, true))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.DirtyCount() != 2 || !reopened.blockDirty(1) || !reopened.blockDirty(4) {
		t.Fatalf("rebuilt bitmap dirty=%d block1=%t block4=%t", reopened.DirtyCount(), reopened.blockDirty(1), reopened.blockDirty(4))
	}
	for _, block := range []int64{0, 2, 3, 5} {
		if reopened.blockDirty(block) {
			t.Fatalf("rebuild marked clean block %d dirty", block)
		}
	}
}

func TestDiffTemplateMatrixAndAtomicCommit(t *testing.T) {
	const size = 4 * cowBlockSize
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.ext4")
	want := make([]byte, size)
	copy(want[cowBlockSize+123:], bytes.Repeat([]byte{0x71}, 257))
	copy(want[3*cowBlockSize:], bytes.Repeat([]byte{0x72}, cowBlockSize))
	writeSparseTemplate(t, templatePath, want, []sparse.Extent{
		{Offset: cowBlockSize + 123, Size: 257},
		{Offset: 3 * cowBlockSize, Size: cowBlockSize},
	})
	codec := testDiffCodec(t, 0x61)

	openSeeded := func(t *testing.T, name string, required bool) (*BlockCOW, string) {
		t.Helper()
		path := filepath.Join(dir, name)
		var options []BlockCOWOption
		if name != "plain.diff" {
			options = append(options, WithCodec(codec, required))
		}
		cow, err := OpenBlockCOW(path, nil, DiffInit{TemplatePath: templatePath}, options...)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, size)
		if _, err := cow.ReadAt(got, 0); err != nil {
			_ = cow.Close()
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			_ = cow.Close()
			t.Fatalf("seeded logical content mismatch for %s", name)
		}
		if cow.DirtyCount() != 2 {
			_ = cow.Close()
			t.Fatalf("seeded dirty blocks=%d want=2", cow.DirtyCount())
		}
		return cow, path
	}

	plain, plainPath := openSeeded(t, "plain.diff", false)
	if plain.diff.encrypted {
		t.Fatal("off produced encrypted target")
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	encrypted, encryptedPath := openSeeded(t, "auto.diff", false)
	if !encrypted.diff.encrypted {
		t.Fatal("auto produced plaintext target")
	}
	if err := encrypted.Close(); err != nil {
		t.Fatal(err)
	}
	required, _ := openSeeded(t, "required.diff", true)
	if err := required.Close(); err != nil {
		t.Fatal(err)
	}

	baseData := patternedBytes(size)
	layered, err := OpenBlockCOW(filepath.Join(dir, "layered.diff"), &fakeReader{data: baseData},
		DiffInit{TemplatePath: templatePath}, WithCodec(codec, true))
	if err != nil {
		t.Fatal(err)
	}
	layeredView := make([]byte, size)
	if _, err := layered.ReadAt(layeredView, 0); err != nil {
		_ = layered.Close()
		t.Fatal(err)
	}
	wantLayered := append([]byte(nil), baseData...)
	copy(wantLayered[cowBlockSize:2*cowBlockSize], want[cowBlockSize:2*cowBlockSize])
	copy(wantLayered[3*cowBlockSize:4*cowBlockSize], want[3*cowBlockSize:4*cowBlockSize])
	if !bytes.Equal(layeredView, wantLayered) {
		_ = layered.Close()
		t.Fatal("template holes did not remain clean base fall-through blocks")
	}
	if err := layered.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenBlockCOW(filepath.Join(dir, "off-from-encrypted.diff"), nil,
		DiffInit{TemplatePath: encryptedPath}); !errors.Is(err, tarstream.ErrCodecRequired) {
		t.Fatalf("off encrypted template error=%v", err)
	}
	reencryptedPath := filepath.Join(dir, "reencrypted.diff")
	reencrypted, err := OpenBlockCOW(reencryptedPath, nil,
		DiffInit{TemplatePath: encryptedPath}, WithCodec(codec, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := reencrypted.Close(); err != nil {
		t.Fatal(err)
	}
	sourceBytes, _ := os.ReadFile(encryptedPath)
	targetBytes, _ := os.ReadFile(reencryptedPath)
	if bytes.Equal(sourceBytes[:diffHeaderRegionSize], targetBytes[:diffHeaderRegionSize]) {
		t.Fatal("encrypted template target reused the wrapped XTS key")
	}
	if bytes.Equal(sourceBytes[diffHeaderRegionSize:], targetBytes[diffHeaderRegionSize:]) {
		t.Fatal("encrypted template was raw-copied instead of re-encrypted")
	}

	existing, err := OpenBlockCOW(plainPath, nil,
		DiffInit{Existing: true, TemplatePath: filepath.Join(dir, "missing-template")})
	if err != nil {
		t.Fatalf("existing target did not ignore template: %v", err)
	}
	_ = existing.Close()

	collision := filepath.Join(dir, "collision.diff")
	if err := os.WriteFile(collision, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBlockCOW(collision, nil, DiffInit{TemplatePath: templatePath}, WithCodec(codec, false)); err == nil {
		t.Fatal("fresh initialization replaced an existing final")
	}
	if body, err := os.ReadFile(collision); err != nil || string(body) != "sentinel" {
		t.Fatalf("collision final changed: %q err=%v", body, err)
	}
	if partials, _ := filepath.Glob(filepath.Join(dir, ".collision.diff.*.partial")); len(partials) != 0 {
		t.Fatalf("collision left temporary files: %v", partials)
	}

	empty := filepath.Join(dir, "empty-placeholder.diff")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fromEmpty, err := OpenBlockCOW(empty, nil, DiffInit{TemplatePath: templatePath}, WithCodec(codec, false))
	if err != nil {
		t.Fatal(err)
	}
	_ = fromEmpty.Close()
	if info, err := os.Stat(empty); err != nil || info.Size() != diffHeaderRegionSize+size {
		t.Fatalf("empty placeholder final size=%v err=%v", info, err)
	}

	seedFailure := filepath.Join(dir, "seed-failure.diff")
	if _, err := initializeFreshDiffFile(seedFailure, size, codec, failingDiffTemplate{size: size}); err == nil {
		t.Fatal("injected template read failure was accepted")
	}
	if _, err := os.Stat(seedFailure); !os.IsNotExist(err) {
		t.Fatalf("seed failure modified final target: %v", err)
	}
	if partials, _ := filepath.Glob(filepath.Join(dir, ".seed-failure.diff.*.partial")); len(partials) != 0 {
		t.Fatalf("seed failure left temporary files: %v", partials)
	}
}

func TestEncryptedSnapshotViewIsDecryptedUpperOnly(t *testing.T) {
	const size = 4 * cowBlockSize
	baseData := patternedBytes(size)
	cow, err := OpenBlockCOW(
		filepath.Join(t.TempDir(), "active.diff"),
		&fakeReader{data: baseData},
		DiffInit{CreateSize: size},
		WithCodec(testDiffCodec(t, 0x71), true),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()
	payload := bytes.Repeat([]byte{0x7a}, 512)
	if _, err := cow.WriteAt(payload, cowBlockSize+512); err != nil {
		t.Fatal(err)
	}
	view, holes, err := cow.SnapshotView()
	if err != nil {
		t.Fatal(err)
	}
	wantHoles := []sparse.Extent{{Offset: 0, Size: cowBlockSize}, {Offset: 2 * cowBlockSize, Size: 2 * cowBlockSize}}
	if fmt.Sprint(holes) != fmt.Sprint(wantHoles) {
		t.Fatalf("holes=%v want=%v", holes, wantHoles)
	}
	got := make([]byte, size)
	if _, err := io.ReadFull(view, got); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, size)
	copy(want[cowBlockSize:2*cowBlockSize], baseData[cowBlockSize:2*cowBlockSize])
	copy(want[cowBlockSize+512:], payload)
	if !bytes.Equal(got, want) {
		t.Fatal("encrypted SnapshotView exposed ciphertext/base or lost dirty plaintext")
	}
}

type shortDiffBodyIO struct {
	inner                 diffBodyIO
	shortRead, shortWrite bool
}

type failingDiffTemplate struct{ size uint64 }

func (s failingDiffTemplate) Size() uint64 { return s.size }
func (s failingDiffTemplate) RunAt(offset, limit uint64) (sparse.RunKind, uint64, error) {
	if offset >= s.size {
		return 0, 0, io.EOF
	}
	end := offset + limit
	if end < offset || end > s.size {
		end = s.size
	}
	return sparse.Data, end, nil
}
func (failingDiffTemplate) ReadAt(context.Context, []byte, uint64) (int, error) {
	return 0, errors.New("injected template read failure")
}
func (failingDiffTemplate) Close() error { return nil }

func (s shortDiffBodyIO) ReadAt(buf []byte, offset int64) (int, error) {
	if !s.shortRead || len(buf) == 0 {
		return s.inner.ReadAt(buf, offset)
	}
	n, err := s.inner.ReadAt(buf[:len(buf)-1], offset)
	if err != nil {
		return n, err
	}
	return n, nil
}

func (s shortDiffBodyIO) WriteAt(buf []byte, offset int64) (int, error) {
	if !s.shortWrite || len(buf) == 0 {
		return s.inner.WriteAt(buf, offset)
	}
	n, err := s.inner.WriteAt(buf[:len(buf)-1], offset)
	if err != nil {
		return n, err
	}
	return n, nil
}

func testDiffCodec(t *testing.T, first byte) tarstream.Codec {
	t.Helper()
	var key [32]byte
	key[0] = first
	codec, err := manifestcrypto.NewTarStreamCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func mutateDiffBytes(t *testing.T, path string, mutate func([]byte)) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutate(body)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func rewrapDiffHeader(t *testing.T, path string, codec tarstream.Codec, mutate func([]byte)) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	region := make([]byte, diffHeaderRegionSize)
	if err := readFullAt(f, region, 0); err != nil {
		t.Fatal(err)
	}
	prefix := append([]byte(nil), region[:diffPrefixSize]...)
	wrappedSize := codec.CiphertextSize(diffHeaderPlainSize)
	header, err := codec.DecryptInPlace(region[diffPrefixSize:diffPrefixSize+wrappedSize], diffHeaderAAD(prefix))
	if err != nil {
		t.Fatal(err)
	}
	mutate(header)
	sealed, err := codec.Encrypt(nil, header, diffHeaderAAD(prefix))
	if err != nil {
		t.Fatal(err)
	}
	clear(region)
	copy(region, prefix)
	copy(region[diffPrefixSize:], sealed)
	if err := writeFullAt(f, region, 0); err != nil {
		t.Fatal(err)
	}
}

func writeSparseTemplate(t *testing.T, path string, logical []byte, data []sparse.Extent) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(len(logical))); err != nil {
		t.Fatal(err)
	}
	for _, extent := range data {
		start := int(extent.Offset)
		end := start + int(extent.Size)
		if _, err := f.WriteAt(logical[start:end], int64(start)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

func mustDecodeHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func binaryPutUint16(target []byte, value uint16) {
	target[0] = byte(value >> 8)
	target[1] = byte(value)
}

func TestDiffReadErrorsDoNotExposeKeyMaterial(t *testing.T) {
	codec := testDiffCodec(t, 0x81)
	path := filepath.Join(t.TempDir(), "active.diff")
	cow, err := OpenBlockCOW(path, nil, DiffInit{CreateSize: cowBlockSize}, WithCodec(codec, false))
	if err != nil {
		t.Fatal(err)
	}
	_ = cow.Close()
	body, _ := os.ReadFile(path)
	body[diffPrefixSize+1] ^= 1
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithCodec(codec, false))
	if err == nil {
		t.Fatal("tampered header was accepted")
	}
	message := err.Error()
	if strings.Contains(message, hex.EncodeToString(body[diffPrefixSize:diffPrefixSize+32])) {
		t.Fatalf("error exposed wrapped key material: %v", err)
	}
}

func FuzzEncryptedDiffHeader(f *testing.F) {
	var key [32]byte
	key[0] = 0x91
	codec, err := manifestcrypto.NewTarStreamCodec(key)
	if err != nil {
		f.Fatal(err)
	}
	seedPath := filepath.Join(f.TempDir(), "seed.diff")
	seed, err := OpenBlockCOW(seedPath, nil, DiffInit{CreateSize: cowBlockSize}, WithCodec(codec, false))
	if err != nil {
		f.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		f.Fatal(err)
	}
	valid, err := os.ReadFile(seedPath)
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte{})
	f.Add([]byte{0, 1})
	f.Add([]byte{8, 0xff, 15, 1})
	f.Add([]byte{byte(diffPrefixSize + 7), 0x80})
	f.Fuzz(func(t *testing.T, mutations []byte) {
		if len(mutations) > 4096 {
			t.Skip()
		}
		body := append([]byte(nil), valid...)
		for i := 0; i+1 < len(mutations); i += 2 {
			position := int(mutations[i])
			body[position%len(body)] ^= mutations[i+1]
		}
		path := filepath.Join(t.TempDir(), "mutated.diff")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		cow, _ := OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithCodec(codec, false))
		if cow != nil {
			_ = cow.Close()
		}
	})
}

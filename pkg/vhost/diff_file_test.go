package vhost

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"golang.org/x/sys/unix"
)

func TestBlockCOWDiffEncryptionOptions(t *testing.T) {
	key := testDiffKey(0x11)
	for _, test := range []struct {
		name    string
		options []BlockCOWOption
	}{
		{name: "duplicate encryption", options: []BlockCOWOption{WithDiffEncryption(key, false), WithDiffEncryption(key, true)}},
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
	key := testDiffKey(0x21)

	cow, err := OpenBlockCOW(path, base, DiffInit{CreateSize: size}, WithDiffEncryption(key, false))
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

	if _, err := OpenBlockCOW(path, base, DiffInit{Existing: true}); !errors.Is(err, ErrDiffEncryptionRequired) {
		t.Fatalf("off encrypted open error=%v", err)
	}
	wrong := testDiffKey(0x22)
	if _, err := OpenBlockCOW(path, base, DiffInit{Existing: true}, WithDiffEncryption(wrong, true)); !errors.Is(err, ErrDiffAuthentication) {
		t.Fatalf("wrong key error=%v", err)
	}
	reopened, err := OpenBlockCOW(path, base, DiffInit{Existing: true}, WithDiffEncryption(key, true))
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
	auto, err := OpenBlockCOW(plainPath, nil, DiffInit{Existing: true}, WithDiffEncryption(key, false))
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
	if _, err := OpenBlockCOW(plainPath, nil, DiffInit{Existing: true}, WithDiffEncryption(key, true)); !errors.Is(err, ErrDiffPlaintextForbidden) {
		t.Fatalf("required plaintext error=%v", err)
	}
}

func TestTransientDiffFlushAndReopen(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "transient.diff")
			var options []BlockCOWOption
			if encrypted {
				options = append(options, WithDiffEncryption(testDiffKey(0x23), true))
			}
			cow, err := OpenBlockCOW(path, nil, DiffInit{CreateSize: 2 * cowBlockSize, Transient: true}, options...)
			if err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte{0x57}, cowBlockSize)
			if _, err := cow.WriteAt(payload, cowBlockSize); err != nil {
				_ = cow.Close()
				t.Fatal(err)
			}
			if err := cow.Flush(); err != nil {
				_ = cow.Close()
				t.Fatal(err)
			}
			if err := cow.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenBlockCOW(path, nil, DiffInit{Existing: true}, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got := make([]byte, 2*cowBlockSize)
			if _, err := reopened.ReadAt(got, 0); err != nil {
				t.Fatal(err)
			}
			if !allZero(got[:cowBlockSize]) || !bytes.Equal(got[cowBlockSize:], payload) || reopened.DirtyCount() != 1 {
				t.Fatal("transient creation changed sparse data or flush/reopen behavior")
			}
		})
	}
}

func TestEncryptedDiffHeaderValidation(t *testing.T) {
	key := testDiffKey(0x31)
	create := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "active.diff")
		cow, err := OpenBlockCOW(path, nil, DiffInit{CreateSize: cowBlockSize}, WithDiffEncryption(key, false))
		if err != nil {
			t.Fatal(err)
		}
		if err := cow.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	open := func(path string, required bool) error {
		cow, err := OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithDiffEncryption(key, required))
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
			rewrapDiffHeader(t, path, key, test.mutate)
			if err := open(path, false); err == nil {
				t.Fatal("invalid authenticated metadata was accepted")
			}
		})
	}

	t.Run("magic-required", func(t *testing.T) {
		path := create(t)
		mutateDiffBytes(t, path, func(body []byte) { body[0] ^= 1 })
		if err := open(path, true); !errors.Is(err, ErrDiffPlaintextForbidden) {
			t.Fatalf("required damaged magic error=%v", err)
		}
	})

	t.Run("near-magic-plaintext-auto", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy.diff")
		body := make([]byte, cowBlockSize)
		copy(body, diffMagic[:])
		body[len(diffMagic)-1] ^= 1
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		cow, err := OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithDiffEncryption(key, false))
		if err != nil {
			t.Fatalf("auto rejected non-magic legacy plaintext: %v", err)
		}
		if cow.diff.encrypted {
			t.Fatal("near-magic plaintext was classified as encrypted")
		}
		_ = cow.Close()
	})

	t.Run("old-aes-siv-v1", func(t *testing.T) {
		const oldHeader = "894b44585453310a00010010000000000170cd897df89cc598b26de657afb7b70b74d8d642451e27f413a9db206efb46a51c2090af40fd8401a2e25f681a19320b5bc87531fb226083b31876d4c89902deb97766b2a600065e3f92ba3323a39a72dce34d4ef9f5e1f59201522f607b0e43cb11d0f1fc948e49f35f8d7dd1f09d2f8f93d97561147dfce80130e919d719c496e7703c11bb326cc2c9479f61c7f42c"
		const oldLogicalSize = 256 * diffDataUnitSize
		path := filepath.Join(t.TempDir(), "old-siv.diff")
		body := make([]byte, diffHeaderRegionSize+oldLogicalSize)
		copy(body, mustDecodeHex(t, oldHeader))
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithDiffEncryption(testDiffKey(0x41), false)); !errors.Is(err, ErrDiffAuthentication) {
			t.Fatalf("old AES-SIV diff error=%v", err)
		}
	})
}

func TestEncryptedDiffGoldenAndRandomKeys(t *testing.T) {
	key := testDiffKey(0x41)
	rawKey := mustDecodeHex(t, "27182818284590452353602874713526624977572470936999595749669676273141592653589793238462643383279502884197169399375105820974944592")
	nonce := mustDecodeHex(t, "000102030405060708090a0b")
	randomMaterial := append(append([]byte(nil), rawKey...), nonce...)
	path := filepath.Join(t.TempDir(), "golden.diff")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const vectorSector = int64(0xff)
	const goldenSize = (vectorSector + 1) * diffDataUnitSize
	diff, err := createEncryptedDiffFile(f, goldenSize, mustDiffEncryption(t, key), bytes.NewReader(randomMaterial))
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
	const wantHeader = "894b44585453310a0001001000000000000102030405060708090a0bb2ce352abb439fafdbbf9e7c711227cc2036bd773685b370447336476c95bb845f52cee4acacc2d1646d48dfd014d36e7dc33a90e76d34e75d0184af8385189bf92ece9956134546003eb26ed378aaa619a77ea3a2d77937494a2a7676f8b809edac3e7a68311a760dd05f4907647a2490ee18f0b4e69dde13e4121c1f0f80d6d5c82996b62be812fb2960d05b3cffc6"
	gotHeader := hex.EncodeToString(physical[:diffPrefixSize+diffHeaderSealedSize])
	if gotHeader != wantHeader {
		t.Fatalf("encrypted diff header=%s want=%s", gotHeader, wantHeader)
	}
	if err := diff.Close(); err != nil {
		t.Fatal(err)
	}

	paths := []string{filepath.Join(t.TempDir(), "a.diff"), filepath.Join(t.TempDir(), "b.diff")}
	files := make([][]byte, len(paths))
	for i, candidate := range paths {
		cow, err := OpenBlockCOW(candidate, nil, DiffInit{CreateSize: cowBlockSize}, WithDiffEncryption(key, false))
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
		WithDiffEncryption(testDiffKey(0x51), false),
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
		WithDiffEncryption(testDiffKey(0x52), false),
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
		WithDiffEncryption(testDiffKey(0x53), false),
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
	key := testDiffKey(0x54)
	cow, err := OpenBlockCOW(path, nil, DiffInit{CreateSize: size}, WithDiffEncryption(key, false))
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
	reopened, err := OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithDiffEncryption(key, true))
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

func TestFreshEncryptedDiffRejectsPreallocatedBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.diff")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	key := testDiffKey(0x55)
	diff, err := createEncryptedDiffFile(f, 2*cowBlockSize, mustDiffEncryption(t, key), strings.NewReader(strings.Repeat("k", diffXTSKeySize+diffHeaderNonceSize)))
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	defer diff.Close()
	if _, err := f.WriteAt([]byte{1}, diffHeaderRegionSize); err != nil {
		t.Fatal(err)
	}
	if err := diff.validateFreshEncryptedBodySparse(); err == nil {
		t.Fatal("filesystem body allocation before seed was accepted")
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
	key := testDiffKey(0x61)
	encryption := mustDiffEncryption(t, key)

	openSeeded := func(t *testing.T, name string, required bool) (*BlockCOW, string) {
		t.Helper()
		path := filepath.Join(dir, name)
		var options []BlockCOWOption
		if name != "plain.diff" {
			options = append(options, WithDiffEncryption(key, required))
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
	if info, err := os.Stat(plainPath); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("fresh plaintext diff permissions=%v err=%v", info, err)
	}
	encrypted, encryptedPath := openSeeded(t, "auto.diff", false)
	if !encrypted.diff.encrypted {
		t.Fatal("auto produced plaintext target")
	}
	if err := encrypted.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(encryptedPath); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("fresh encrypted diff permissions=%v err=%v", info, err)
	}
	required, _ := openSeeded(t, "required.diff", true)
	if err := required.Close(); err != nil {
		t.Fatal(err)
	}

	baseData := patternedBytes(size)
	layered, err := OpenBlockCOW(filepath.Join(dir, "layered.diff"), &fakeReader{data: baseData},
		DiffInit{TemplatePath: templatePath}, WithDiffEncryption(key, true))
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
		DiffInit{TemplatePath: encryptedPath}); !errors.Is(err, ErrDiffEncryptionRequired) {
		t.Fatalf("off encrypted template error=%v", err)
	}
	reencryptedPath := filepath.Join(dir, "reencrypted.diff")
	reencrypted, err := OpenBlockCOW(reencryptedPath, nil,
		DiffInit{TemplatePath: encryptedPath}, WithDiffEncryption(key, true))
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
	if _, err := OpenBlockCOW(collision, nil, DiffInit{TemplatePath: templatePath}, WithDiffEncryption(key, false)); err == nil {
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
	fromEmpty, err := OpenBlockCOW(empty, nil, DiffInit{TemplatePath: templatePath}, WithDiffEncryption(key, false))
	if err != nil {
		t.Fatal(err)
	}
	_ = fromEmpty.Close()
	if info, err := os.Stat(empty); err != nil || info.Size() != diffHeaderRegionSize+size {
		t.Fatalf("empty placeholder final size=%v err=%v", info, err)
	}

	seedFailure := filepath.Join(dir, "seed-failure.diff")
	if _, err := initializeFreshDiffFile(seedFailure, size, encryption, failingDiffTemplate{size: size}, false); err == nil {
		t.Fatal("injected template read failure was accepted")
	}
	if _, err := os.Stat(seedFailure); !os.IsNotExist(err) {
		t.Fatalf("seed failure modified final target: %v", err)
	}
	if partials, _ := filepath.Glob(filepath.Join(dir, ".seed-failure.diff.*.partial")); len(partials) != 0 {
		t.Fatalf("seed failure left temporary files: %v", partials)
	}
}

func TestValidateDiffExt4UsesLogicalPlaintextAndBaseFallback(t *testing.T) {
	const size = 2 * cowBlockSize
	ctx := context.Background()
	dir := t.TempDir()
	ext4 := make([]byte, size)
	ext4[ext4MagicOffset], ext4[ext4MagicOffset+1] = 0x53, 0xef
	plainPath := filepath.Join(dir, "plain.ext4")
	writeSparseTemplate(t, plainPath, ext4, []sparse.Extent{{Offset: 1024, Size: 1024}})

	if err := ValidateDiffTemplateExt4(ctx, plainPath, nil); err != nil {
		t.Fatalf("validate plaintext template: %v", err)
	}
	if err := ValidateExistingDiffExt4(ctx, plainPath, nil,
		WithDiffEncryption(testDiffKey(0x81), true)); !errors.Is(err, ErrDiffPlaintextForbidden) {
		t.Fatalf("required policy error = %v, want ErrDiffPlaintextForbidden", err)
	}

	key := testDiffKey(0x82)
	encryptedPath := filepath.Join(dir, "encrypted.diff")
	cow, err := OpenBlockCOW(encryptedPath, nil,
		DiffInit{TemplatePath: plainPath}, WithDiffEncryption(key, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := cow.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExistingDiffExt4(ctx, encryptedPath, nil,
		WithDiffEncryption(key, true)); err != nil {
		t.Fatalf("validate encrypted active diff: %v", err)
	}

	holeTemplate := filepath.Join(dir, "upper-delta")
	upper := make([]byte, size)
	upper[cowBlockSize] = 0x7a
	writeSparseTemplate(t, holeTemplate, upper,
		[]sparse.Extent{{Offset: cowBlockSize, Size: 1}})
	if err := ValidateDiffTemplateExt4(ctx, holeTemplate, &fakeReader{data: ext4}); err != nil {
		t.Fatalf("validate template with base fallback: %v", err)
	}

	invalidPath := filepath.Join(dir, "invalid")
	if err := os.WriteFile(invalidPath, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExistingDiffExt4(ctx, invalidPath, nil); err == nil ||
		!strings.Contains(err.Error(), "formatted ext4") {
		t.Fatalf("invalid filesystem error = %v", err)
	}
}

func TestEmptyPlaceholderConcurrentInitializationDoesNotReplaceWinner(t *testing.T) {
	const size = 2 * cowBlockSize
	dir := t.TempDir()
	path := filepath.Join(dir, "active.diff")
	template := filepath.Join(dir, "template.ext4")
	want := patternedBytes(size)
	writeSparseTemplate(t, template, want, []sparse.Extent{{Offset: 0, Size: size}})
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	key := testDiffKey(0x62)

	const creators = 16
	start := make(chan struct{})
	results := make(chan error, creators)
	var wait sync.WaitGroup
	for range creators {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			cow, err := OpenBlockCOW(path, nil, DiffInit{TemplatePath: template}, WithDiffEncryption(key, true))
			if cow != nil {
				err = errors.Join(err, cow.Close())
			}
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		if !errors.Is(err, unix.EEXIST) {
			t.Fatalf("competing creator error=%v, want EEXIST", err)
		}
	}
	if winners != 1 {
		t.Fatalf("successful creators=%d want=1", winners)
	}

	reopened, err := OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithDiffEncryption(key, true))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := make([]byte, size)
	if _, err := reopened.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("winning atomic initialization was replaced or corrupted")
	}
	if partials, _ := filepath.Glob(filepath.Join(dir, ".active.diff.*.partial")); len(partials) != 0 {
		t.Fatalf("competing creators left temporary files: %v", partials)
	}
}

func TestEncryptedSnapshotViewIsDecryptedUpperOnly(t *testing.T) {
	const size = 4 * cowBlockSize
	baseData := patternedBytes(size)
	cow, err := OpenBlockCOW(
		filepath.Join(t.TempDir(), "active.diff"),
		&fakeReader{data: baseData},
		DiffInit{CreateSize: size},
		WithDiffEncryption(testDiffKey(0x71), true),
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
func (s failingDiffTemplate) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.size {
		return nil, io.EOF
	}
	end := offset + limit
	if end < offset || end > s.size {
		end = s.size
	}
	return failingDiffRun{source: s, offset: offset, end: end}, nil
}
func (failingDiffTemplate) ReadAt(context.Context, []byte, uint64) (int, error) {
	return 0, errors.New("injected template read failure")
}
func (failingDiffTemplate) Close() error { return nil }

type failingDiffRun struct {
	source failingDiffTemplate
	offset uint64
	end    uint64
}

func (r failingDiffRun) Offset() uint64     { return r.offset }
func (r failingDiffRun) End() uint64        { return r.end }
func (failingDiffRun) Kind() sparse.RunKind { return sparse.Data }
func (r failingDiffRun) ReadAt(ctx context.Context, buf []byte, innerOffset uint64) (int, error) {
	if innerOffset > r.end-r.offset || uint64(len(buf)) > r.end-r.offset-innerOffset {
		return 0, errors.New("run read out of bounds")
	}
	return r.source.ReadAt(ctx, buf, r.offset+innerOffset)
}

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

func testDiffKey(first byte) [32]byte {
	var key [32]byte
	key[0] = first
	return key
}

func mustDiffEncryption(t *testing.T, key [32]byte) *diffEncryption {
	t.Helper()
	encryption, err := newDiffEncryption(key)
	if err != nil {
		t.Fatal(err)
	}
	return encryption
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

func rewrapDiffHeader(t *testing.T, path string, key [32]byte, mutate func([]byte)) {
	t.Helper()
	encryption := mustDiffEncryption(t, key)
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
	nonceEnd := diffPrefixSize + diffHeaderNonceSize
	wrapperEnd := diffPrefixSize + diffHeaderSealedSize
	header, err := encryption.header.Open(
		nil,
		region[diffPrefixSize:nonceEnd],
		region[nonceEnd:wrapperEnd],
		diffHeaderAAD(prefix),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(header)
	mutate(header)
	clear(region)
	copy(region, prefix)
	if _, err := io.ReadFull(cryptorand.Reader, region[diffPrefixSize:nonceEnd]); err != nil {
		t.Fatal(err)
	}
	encryption.header.Seal(
		region[nonceEnd:nonceEnd],
		region[diffPrefixSize:nonceEnd],
		header,
		diffHeaderAAD(prefix),
	)
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
	key := testDiffKey(0x81)
	path := filepath.Join(t.TempDir(), "active.diff")
	cow, err := OpenBlockCOW(path, nil, DiffInit{CreateSize: cowBlockSize}, WithDiffEncryption(key, false))
	if err != nil {
		t.Fatal(err)
	}
	_ = cow.Close()
	body, _ := os.ReadFile(path)
	body[diffPrefixSize+1] ^= 1
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithDiffEncryption(key, false))
	if err == nil {
		t.Fatal("tampered header was accepted")
	}
	message := err.Error()
	if strings.Contains(message, hex.EncodeToString(body[diffPrefixSize:diffPrefixSize+32])) {
		t.Fatalf("error exposed wrapped key material: %v", err)
	}
}

func FuzzEncryptedDiffHeader(f *testing.F) {
	key := testDiffKey(0x91)
	seedPath := filepath.Join(f.TempDir(), "seed.diff")
	seed, err := OpenBlockCOW(seedPath, nil, DiffInit{CreateSize: cowBlockSize}, WithDiffEncryption(key, false))
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
		cow, _ := OpenBlockCOW(path, nil, DiffInit{Existing: true}, WithDiffEncryption(key, false))
		if cow != nil {
			_ = cow.Close()
		}
	})
}

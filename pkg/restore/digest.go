package restore

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/runtimebundle"
)

func openTarArtifact(path string, ref manifest.Ref, codec tarstream.Codec, required bool) (fetch.Stream, string, string, error) {
	options, err := tarReadOptions(ref, codec, required)
	if err != nil {
		return nil, "", "", err
	}
	stream, err := fetch.OpenTarStream(path, options...)
	if err != nil {
		return nil, "", "", protectArtifactReadError(codec, "open local artifact", err)
	}
	scheme, digest, err := sourceDigest(stream)
	if err != nil {
		_ = stream.Close()
		return nil, "", "", err
	}
	if ref.Digest == "" && !contentAddressedNameMatches(path, digest) {
		_ = stream.Close()
		return nil, "", "", fmt.Errorf("artifact content name does not match identity")
	}
	return stream, scheme, digest, nil
}

func openSequentialTarArtifact(path, name string, ref manifest.Ref, codec tarstream.Codec, required bool) (sparse.Source, io.Closer, string, string, string, error) {
	random, scheme, digest, err := openTarArtifact(path, ref, codec, required)
	if err != nil {
		return nil, nil, "", "", "", err
	}
	if err := random.Close(); err != nil {
		return nil, nil, "", "", "", err
	}
	identity := manifest.Ref{
		Scheme: manifest.RefSchemeFile, Path: path,
		DigestScheme: scheme, Digest: digest,
	}
	options, err := tarReadOptions(identity, codec, required)
	if err != nil {
		return nil, nil, "", "", "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, "", "", "", protectArtifactReadError(codec, "open local artifact sequentially", err)
	}
	source, payloadName, err := tarstream.SourceFrom(f, name, options...)
	if err != nil {
		_ = f.Close()
		return nil, nil, "", "", "", err
	}
	return source, f, payloadName, scheme, digest, nil
}

type artifactReadError struct {
	op  string
	err error
}

func (e *artifactReadError) Error() string { return e.op + " failed" }
func (e *artifactReadError) Unwrap() error { return e.err }

func protectArtifactReadError(codec tarstream.Codec, op string, err error) error {
	if codec == nil || err == nil {
		return err
	}
	return &artifactReadError{op: op, err: err}
}

func tarReadOptions(ref manifest.Ref, codec tarstream.Codec, required bool) ([]tarstream.ReadOption, error) {
	if required && codec == nil {
		return nil, fmt.Errorf("local tarstream: required policy has no codec")
	}
	var options []tarstream.ReadOption
	if codec != nil {
		options = append(options, tarstream.WithCodec(codec, required))
	}
	if ref.Digest == "" {
		return options, nil
	}
	return append(options, tarstream.WithExpectedDigest(ref.DigestScheme, ref.Digest)), nil
}

func sourceDigest(source any) (string, string, error) {
	digester, ok := source.(tarstream.Digester)
	if !ok {
		return "", "", fmt.Errorf("tarstream artifact has no declared digest")
	}
	scheme, digest := digester.Digest()
	if !validCarrierDigest(scheme, digest) {
		return "", "", fmt.Errorf("tarstream artifact has invalid declared digest")
	}
	return scheme, digest, nil
}

func readRuntimeBundleDigest(path string) (string, string, error) {
	info, err := runtimebundle.Inspect(path)
	if err != nil {
		return "", "", err
	}
	scheme, digest, ok := strings.Cut(info.Digest, ":")
	if !ok || scheme != tarstream.DigestScheme || !validHexDigest(digest) {
		return "", "", fmt.Errorf("runtime bundle has invalid digest")
	}
	return scheme, digest, nil
}

func matchDigest(gotScheme, gotDigest, wantScheme, wantDigest string) error {
	if gotScheme != wantScheme || !digestEqual(gotDigest, wantDigest) {
		return fmt.Errorf("artifact identity mismatch")
	}
	return nil
}

func contentAddressedNameMatches(path, digest string) bool {
	real := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		real = resolved
	}
	base := filepath.Base(real)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	return digestEqual(stem, digest)
}

func validCarrierDigest(scheme, digest string) bool {
	if scheme != tarstream.DigestScheme && scheme != tarstream.DigestSchemeHMAC {
		return false
	}
	return validHexDigest(digest)
}

func validHexDigest(digest string) bool {
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func digestEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func consumeSource(ctx context.Context, source sparse.Source, start uint64) error {
	if start > source.Size() {
		return fmt.Errorf("source offset out of bounds")
	}
	buffer := make([]byte, 128*1024)
	for offset := start; offset < source.Size(); {
		if err := ctx.Err(); err != nil {
			return err
		}
		run, err := source.RunAt(offset, source.Size()-offset)
		if err != nil {
			return err
		}
		kind, end := run.Kind(), run.End()
		if end <= offset || end > source.Size() {
			return fmt.Errorf("invalid sparse run")
		}
		if kind != sparse.Hole {
			for position := offset; position < end; {
				length := min(uint64(len(buffer)), end-position)
				n, readErr := source.ReadAt(ctx, buffer[:int(length)], position)
				if readErr != nil && readErr != io.EOF {
					return readErr
				}
				if n != int(length) {
					return io.ErrUnexpectedEOF
				}
				position += length
			}
		}
		offset = end
	}
	return nil
}

package tartransition

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type oldDigester string

func (d oldDigester) Digest() string { return string(d) }

type newDigester struct {
	scheme string
	digest string
}

func (d newDigester) Digest() (string, string) { return d.scheme, d.digest }

type badDigester struct{}

func (badDigester) Digest() int { return 1 }

func TestDigestSignatures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  any
		want   string
		wantOK bool
	}{
		{name: "legacy", value: oldDigester("sha256:" + testDigest), want: "sha256:" + testDigest, wantOK: true},
		{name: "final", value: newDigester{scheme: "hmac", digest: testDigest}, want: "hmac:" + testDigest, wantOK: true},
		{name: "missing", value: struct{}{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := Digest(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("Digest() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
	if _, _, err := Digest(badDigester{}); err == nil {
		t.Fatal("invalid Digest signature accepted")
	}
}

func TestSHA256DigestRejectsHMAC(t *testing.T) {
	got, err := SHA256Digest("sha256:" + testDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got != testDigest {
		t.Fatalf("SHA256Digest() = %q, want %q", got, testDigest)
	}
	if _, err := SHA256Digest("hmac:" + testDigest); err == nil {
		t.Fatal("HMAC digest accepted by legacy SHA-256 caller")
	}
}

func TestCallWriteToSignatures(t *testing.T) {
	ctx := context.Background()
	src := sparse.Dense(bytes.NewReader(nil), 0)
	oldWrite := func(context.Context, io.Writer, string, sparse.Source) (string, error) {
		return "sha256:" + testDigest, nil
	}
	type option string
	newWrite := func(context.Context, io.Writer, string, sparse.Source, ...option) (string, string, error) {
		return "hmac", testDigest, nil
	}
	for _, tc := range []struct {
		name string
		fn   any
		want string
	}{
		{name: "legacy", fn: oldWrite, want: "sha256:" + testDigest},
		{name: "final", fn: newWrite, want: "hmac:" + testDigest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := callWriteTo(tc.fn, ctx, io.Discard, "image", src)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("callWriteTo() = %q, want %q", got, tc.want)
			}
		})
	}

	wantErr := errors.New("write failed")
	failing := func(context.Context, io.Writer, string, sparse.Source) (string, error) {
		return "", wantErr
	}
	if _, err := callWriteTo(failing, ctx, io.Discard, "image", src); !errors.Is(err, wantErr) {
		t.Fatalf("callWriteTo error = %v, want %v", err, wantErr)
	}
	if _, err := callWriteTo(func() {}, ctx, io.Discard, "image", src); err == nil {
		t.Fatal("invalid WriteTo signature accepted")
	}
}

func TestWriteToUsesCurrentTarstream(t *testing.T) {
	var out bytes.Buffer
	got, err := WriteTo(context.Background(), &out, "image", sparse.Dense(bytes.NewReader(nil), 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Fatalf("WriteTo digest = %q", got)
	}
	if out.Len() == 0 {
		t.Fatal("WriteTo emitted no artifact")
	}
}

func BenchmarkTransitionDispatch(b *testing.B) {
	ctx := context.Background()
	src := sparse.Dense(bytes.NewReader(nil), 0)
	fn := func(context.Context, io.Writer, string, sparse.Source) (string, error) {
		return "sha256:" + testDigest, nil
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := callWriteTo(fn, ctx, io.Discard, "image", src); err != nil {
			b.Fatal(err)
		}
	}
}

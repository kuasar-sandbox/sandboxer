package runtimebundle

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

func TestInspect(t *testing.T) {
	digest := strings.Repeat("a", 64)
	var footer bytes.Buffer
	zw := zip.NewWriter(&footer)
	h := &zip.FileHeader{
		Name:     tarstream.SHA256MarkerPrefix + digest,
		Method:   zip.Store,
		Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	h.SetMode(0o444)
	if _, err := zw.CreateHeader(h); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := make([]byte, pmemAlignment)
	copy(bundle[len(bundle)-footer.Len():], footer.Bytes())
	path := filepath.Join(t.TempDir(), "runtime.bundle")
	if err := os.WriteFile(path, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != pmemAlignment || info.Digest != "sha256:"+digest {
		t.Fatalf("info = %+v", info)
	}
}

func TestInspectRejectsRawAndUnaligned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.erofs")
	if err := os.WriteFile(path, []byte("raw"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(path); err == nil {
		t.Fatal("raw unaligned EROFS accepted as runtime bundle")
	}
}

func TestInspectRejectsInvalidMarkerZIP(t *testing.T) {
	tests := []struct {
		name    string
		entries map[string]string
		trailer []byte
	}{
		{name: "invalid-name", entries: map[string]string{tarstream.SHA256MarkerPrefix + "bad": ""}},
		{name: "nonempty", entries: map[string]string{tarstream.SHA256MarkerPrefix + strings.Repeat("a", 64): "x"}},
		{name: "duplicate", entries: map[string]string{
			tarstream.SHA256MarkerPrefix + strings.Repeat("a", 64): "",
			tarstream.SHA256MarkerPrefix + strings.Repeat("b", 64): "",
		}},
		{name: "trailing-data", entries: map[string]string{tarstream.SHA256MarkerPrefix + strings.Repeat("a", 64): ""}, trailer: []byte("junk")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var footer bytes.Buffer
			zw := zip.NewWriter(&footer)
			for name, body := range tc.entries {
				h := &zip.FileHeader{Name: name, Method: zip.Store, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
				w, err := zw.CreateHeader(h)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write([]byte(body)); err != nil {
					t.Fatal(err)
				}
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			footer.Write(tc.trailer)
			bundle := make([]byte, pmemAlignment)
			copy(bundle[len(bundle)-footer.Len():], footer.Bytes())
			path := filepath.Join(t.TempDir(), "runtime.bundle")
			if err := os.WriteFile(path, bundle, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(path); err == nil {
				t.Fatal("invalid runtime bundle accepted")
			}
		})
	}
}

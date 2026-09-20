package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
)

type pairUploadStore struct {
	mu        sync.Mutex
	manifests map[store.ContentKey]bool
}

func (*pairUploadStore) AdmitWrite(context.Context) (store.WriteAdmission, error) {
	salt, err := store.SaltForGeneration("NONE")
	return store.WriteAdmission{Generation: "NONE", Salt: salt}, err
}
func (s *pairUploadStore) Put(_ context.Context, _ store.WriteAdmission, p store.Partition, k store.ContentKey, _ []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == store.PartitionManifest {
		s.manifests[k] = true
	}
	return true, nil
}

func TestNativeCaptureReturnsCommittedPairForEverySink(t *testing.T) {
	for _, mode := range []string{"local", "bundle", "upload"} {
		for _, snapshotCapture := range []bool{false, true} {
			for _, resume := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/snapshot=%t/resume=%t", mode, snapshotCapture, resume), func(t *testing.T) {
					ctx := context.Background()
					dir, err := os.MkdirTemp("", "native-pair-")
					if err != nil {
						t.Fatal(err)
					}
					defer os.RemoveAll(dir)
					staging := filepath.Join(dir, "staging")
					if err := os.Mkdir(staging, 0700); err != nil {
						t.Fatal(err)
					}
					listener, err := net.Listen("unix", filepath.Join(dir, "ch.sock"))
					if err != nil {
						t.Fatal(err)
					}
					server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/api/v1/vm.snapshot" {
							for _, name := range []string{"config.json", "state.json"} {
								if err := os.WriteFile(filepath.Join(staging, name), []byte("{}"), 0600); err != nil {
									http.Error(w, err.Error(), 500)
									return
								}
							}
						}
						w.WriteHeader(http.StatusNoContent)
					})}
					go server.Serve(listener)
					defer server.Close()
					cfg := &manifest.Config{Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}}, Crypto: manifestcrypto.Config{Chunk: "aes", Manifest: "aes"}}
					key := func() ([32]byte, error) { return [32]byte{42}, nil }
					var sink ArtifactSink
					var upload *IngestSink
					var bundle *BundleSink
					backend := &pairUploadStore{manifests: make(map[store.ContentKey]bool)}
					switch mode {
					case "local":
						sink = NewFileSink(dir, "sid", nil, false, nil)
					case "bundle":
						bundle, err = NewBundleSink(ctx, dir, "sid", cfg, key, nil)
						sink = bundle
					case "upload":
						writer, e := cfg.NewIngesterWithWriter(key, nil, backend)
						err = e
						if e == nil {
							upload = NewIngestSink(writer, nil)
							sink = upload
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					portable := exportTestPortable(t)
					portable.Boot.Root = config.PortableRootConfig{Base: "self"}
					portable.Boot.Disks = nil
					portable.Mounts = nil
					diffs := []DiskDiff{{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
						return bytes.NewReader(bytes.Repeat([]byte{71}, 4096)), nil, nil
					}}}
					q := &freezeTrackingQuiescer{}
					var sref, eref, spath, epath string
					if snapshotCapture {
						mem, err := memory.Create("paired-native", 4096)
						if err != nil {
							t.Fatal(err)
						}
						defer mem.Close()
						copy(mem.Bytes(), bytes.Repeat([]byte{93}, 4096))
						result, err := Take(Sources{Context: ctx, SandboxID: "sid", APISock: listener.Addr().String(), MemfdFD: mem.FD(), MemfdSize: int64(mem.Size()), StagingDir: staging, PortableConfig: portable, Diffs: diffs, CHApiDeadline: time.Second, Quiescer: q}, sink, resume)
						if err != nil {
							t.Fatal(err)
						}
						sref, eref, spath, epath = result.SnapshotRef, result.SandboxRef, result.SnapshotPath, result.SandboxPath
					} else {
						result, err := Export(ctx, ExportSources{SandboxID: "sid", APISock: listener.Addr().String(), PortableConfig: portable, Diffs: diffs, CHApiDeadline: time.Second, Quiescer: q}, sink, resume)
						if err != nil {
							t.Fatal(err)
						}
						eref, epath = result.SandboxRef, result.SandboxPath
					}
					if eref == "" || (snapshotCapture && sref == "") {
						t.Fatalf("torn output S=%q E=%q", sref, eref)
					}
					if mode == "upload" {
						if eref != "manifest://"+HexKey(upload.SandboxResult().ManifestKey) {
							t.Fatal("E differs from actual upload key")
						}
						if snapshotCapture {
							_, s := upload.Results()
							if sref != "manifest://"+HexKey(s.ManifestKey) {
								t.Fatal("S differs from actual upload key")
							}
						}
						if epath != "" || spath != "" {
							t.Fatal("upload has local path")
						}
					} else {
						e, err := manifest.ParseRef(eref)
						if err != nil {
							t.Fatal(err)
						}
						if e.Path != filepath.Base(epath) || e.Digest == "" {
							t.Fatalf("unbound E=%s path=%s", eref, epath)
						}
						if mode == "bundle" {
							if e.DigestScheme != "manifest" || e.Digest != HexKey(bundle.SandboxResult().ManifestKey) {
								t.Fatal("E selector differs from producer")
							}
							if snapshotCapture {
								s, _ := manifest.ParseRef(sref)
								_, sr := bundle.Results()
								if spath != epath || s.Path != e.Path || s.Digest != HexKey(sr.ManifestKey) || s.Digest == e.Digest {
									t.Fatalf("Bundle pair S=%s E=%s", sref, eref)
								}
							}
						} else if !strings.HasSuffix(e.Path, ".sandbox") {
							t.Fatal("wrong E role")
						}
					}
				})
			}
		}
	}
}

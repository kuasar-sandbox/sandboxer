package artifact

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/wire"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/store/pb"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// One ordered script is emitted at real backend boundaries. The unchanged
// cache/gRPC protocols carry their native equivalents of the file errors.
var sourceStages = []struct {
	name  string
	local error
	rpc   codes.Code
}{
	{"access failure", errors.New("temporary access failure"), codes.Unavailable},
	{"attempt timeout", readerr.Mark(context.DeadlineExceeded, true), codes.DeadlineExceeded},
	{"backend cancellation", context.Canceled, codes.Canceled},
}

type sourceScript struct {
	armed atomic.Bool
	calls atomic.Int32
}

func (s *sourceScript) next() int {
	if !s.armed.Load() {
		return -1
	}
	n := int(s.calls.Add(1)) - 1
	if n >= len(sourceStages) {
		return -1
	}
	return n
}

type scriptedFile struct {
	*os.File
	script *sourceScript
}

func (s scriptedFile) ReadAt(p []byte, off int64) (int, error) {
	if step := s.script.next(); step >= 0 {
		for i := range p[:len(p)/2] {
			p[i] = 0xff
		}
		return len(p) / 2, sourceStages[step].local
	}
	return s.File.ReadAt(p, off)
}

type sourceStream struct {
	sparse.Source
	io.Closer
}
type observedStream struct {
	fetch.Stream
	attempts int
	failures []error
}

func (s *observedStream) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	s.attempts++
	n, err := s.Stream.ReadAt(ctx, p, off)
	if err != nil {
		s.failures = append(s.failures, err)
	}
	return n, err
}

type matrixWriter struct {
	writer  *bundle.Writer
	objects map[store.ContentKey][]byte
	mu      sync.Mutex
}

func (w *matrixWriter) AdmitWrite(ctx context.Context) (store.WriteAdmission, error) {
	return w.writer.AdmitWrite(ctx)
}
func (w *matrixWriter) Put(ctx context.Context, a store.WriteAdmission, p store.Partition, k store.ContentKey, b []byte) (bool, error) {
	w.mu.Lock()
	w.objects[k] = bytes.Clone(b)
	w.mu.Unlock()
	return w.writer.Put(ctx, a, p, k, b)
}

type matrixStore struct {
	pb.UnimplementedStoreServer
	objects map[store.ContentKey][]byte
	script  *sourceScript
}

func (s *matrixStore) Get(req *pb.GetRequest, out grpc.ServerStreamingServer[pb.GetResponse]) error {
	var k store.ContentKey
	copy(k[:], req.Key)
	if req.Partition == pb.Partition_PARTITION_CHUNK {
		if step := s.script.next(); step >= 0 {
			if err := out.Send(&pb.GetResponse{Data: []byte("discard incomplete object")}); err != nil {
				return err
			}
			return status.Error(sourceStages[step].rpc, sourceStages[step].name)
		}
	}
	data, ok := s.objects[k]
	if !ok {
		return status.Error(codes.NotFound, "missing fixture object")
	}
	return out.Send(&pb.GetResponse{Data: data})
}

func startMatrixCache(t *testing.T, objects map[store.ContentKey][]byte, script *sourceScript) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	conns := map[net.Conn]bool{}
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns[c] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				defer func() { mu.Lock(); delete(conns, c); mu.Unlock() }()
				r := bufio.NewReader(c)
				for {
					req, err := wire.ReadRequest(r)
					if err != nil {
						return
					}
					step := -1
					if req.Opcode == wire.OpcodeObjectGet && req.Namespace == wire.NSChunk {
						step = script.next()
					}
					response := &wire.Response{Status: wire.StatusHit}
					if step >= 0 {
						if step == 1 {
							req.Release()
							time.Sleep(150 * time.Millisecond)
							return
						} // actual socket read timeout
						if step == 2 {
							response.Status = wire.StatusCancelled
						} else {
							response.Status = wire.StatusError
							response.ErrMsg = sourceStages[step].name
						}
					} else if req.Opcode != wire.OpcodePing {
						value, ok := objects[req.Hash]
						if !ok {
							response.Status = wire.StatusMiss
						} else {
							response.Value = cache.NewMemBlob(value)
						}
					}
					req.Release()
					if wire.WriteResponse(c, response) != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		<-done
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return path
}

func TestSameReadScriptAcrossFileBundleCacheAndStore(t *testing.T) {
	ctx := context.Background()
	plain := bytes.Repeat([]byte{0x42}, 4096)
	customer := [32]byte{1, 2, 3}
	var archive bytes.Buffer
	salt, err := store.SaltForGeneration("G1")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := bundle.NewWriter(&archive, store.WriteAdmission{Generation: "G1", Salt: salt}, bundle.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	objects := map[store.ContentKey][]byte{}
	enc, dec, err := manifestcrypto.New(manifestcrypto.Config{Chunk: "aes", Manifest: "aes"})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := chunker.New(chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}})
	if err != nil {
		t.Fatal(err)
	}
	src, _ := sparse.NewSource(bytes.NewReader(plain), uint64(len(plain)), nil)
	result, err := ingest.NewIngester(func() ([32]byte, error) { return customer, nil }, nil, &matrixWriter{writer: writer, objects: objects}, chk, enc).Ingest(ctx, src, ingest.IngestOption{})
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	var tar bytes.Buffer
	if _, _, err = tarstream.WriteTo(ctx, &tar, "image", src); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"file/tarstream", "Bundle", "cache", "store"} {
		t.Run(kind, func(t *testing.T) {
			script := &sourceScript{}
			var stream fetch.Stream
			if kind == "file/tarstream" || kind == "Bundle" {
				data := tar.Bytes()
				if kind == "Bundle" {
					data = archive.Bytes()
				}
				path := filepath.Join(t.TempDir(), "source")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { file.Close() })
				ra := scriptedFile{file, script}
				if kind == "file/tarstream" {
					source, _, err := tarstream.SourceAt(ra, int64(len(data)), "image")
					if err != nil {
						t.Fatal(err)
					}
					stream = sourceStream{source, file}
				} else {
					reader, err := bundle.NewReader(ra, int64(len(data)))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { reader.Close() })
					stream, err = fetch.NewFetcherWithOptions(customer, reader.Getter(), dec, fetch.Options{VerifyContent: true}).OpenManifest(ctx, result.ManifestKey)
					if err != nil {
						t.Fatal(err)
					}
				}
			} else {
				cfg := &config.ManifestConfig{Crypto: manifestcrypto.Config{Chunk: "aes", Manifest: "aes", Local: manifestcrypto.LocalOff}}
				if kind == "cache" {
					cfg.Cache = manifest.CacheConfig{Endpoint: startMatrixCache(t, objects, script), Pool: 1, Timeout: "50ms"}
				} else {
					l, err := net.Listen("unix", filepath.Join(t.TempDir(), "store.sock"))
					if err != nil {
						t.Fatal(err)
					}
					server := grpc.NewServer()
					pb.RegisterStoreServer(server, &matrixStore{objects: objects, script: script})
					done := make(chan struct{})
					go func() { defer close(done); server.Serve(l) }()
					t.Cleanup(func() { server.Stop(); l.Close(); <-done })
					cfg.Store = manifest.StoreConfig{Endpoint: l.Addr().String(), Pool: 1, Timeout: "1s"}
				}
				storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { return customer, nil })
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { storage.Close() })
				stream, err = storage.Fetcher().OpenManifest(ctx, result.ManifestKey)
				if err != nil {
					t.Fatal(err)
				}
			}
			observed := &observedStream{Stream: stream}
			readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			reader := vhost.NewStreamReader(readCtx, observed, int64(len(plain)))
			defer reader.Close()
			script.armed.Store(true)
			dst := bytes.Repeat([]byte{0xaa}, len(plain))
			n, err := reader.ReadAt(dst, 0)
			if err != nil || n != len(plain) || !bytes.Equal(dst, plain) || observed.attempts != 4 || len(observed.failures) != 3 {
				t.Fatalf("script not preserved: bytes=%d error=%v attempts=%d failures=%v", n, err, observed.attempts, observed.failures)
			}
			for i, err := range observed.failures {
				if readerr.IsPermanent(err) {
					t.Fatalf("%s incorrectly permanent: %v", sourceStages[i].name, err)
				}
			}
			t.Logf("%s: access failure -> attempt timeout -> backend cancellation -> complete original bytes; 4 synchronous attempts", kind)
		})
	}
}

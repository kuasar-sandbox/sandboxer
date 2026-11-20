package artifact

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

// checkpointFilePool bounds physical descriptors independently of immutable
// parser metadata. Only callers holding the source directory's GC pin may use it.
type checkpointPoolForPath func(string) (*checkpointFilePool, error)

type checkpointFilePool struct {
	mu      sync.Mutex
	cond    *sync.Cond
	entries map[string]*checkpointFileEntry
	clock   uint64
	closed  bool
	err     error
}
type checkpointFileEntry struct {
	file     *os.File
	identity os.FileInfo
	users    int
	used     uint64
}
type checkpointFileReader struct {
	pool     *checkpointFilePool
	path     string
	identity os.FileInfo
}

const checkpointFileSlots = 8

func newCheckpointFilePool() *checkpointFilePool {
	p := &checkpointFilePool{entries: make(map[string]*checkpointFileEntry)}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *checkpointFilePool) source(path string) (*checkpointFileReader, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("checkpoint carrier is not a regular file")
	}
	return &checkpointFileReader{pool: p, path: path, identity: info}, nil
}

func sameCheckpointFile(a, b os.FileInfo) bool {
	if !os.SameFile(a, b) || a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) {
		return false
	}
	// The immutable carrier may not be rewritten in place with its mtime restored.
	x, xok := a.Sys().(*syscall.Stat_t)
	y, yok := b.Sys().(*syscall.Stat_t)
	return !xok || !yok || x.Ctim == y.Ctim
}

func (r *checkpointFileReader) ReadAt(b []byte, off int64) (int, error) {
	p := r.pool
	p.mu.Lock()
	for {
		if p.closed {
			p.mu.Unlock()
			return 0, os.ErrClosed
		}
		if e := p.entries[r.path]; e != nil {
			if !sameCheckpointFile(e.identity, r.identity) {
				p.mu.Unlock()
				return 0, readerr.Mark(errors.New("checkpoint carrier identity changed"), false)
			}
			e.users++
			p.clock++
			e.used = p.clock
			p.mu.Unlock()
			n, err := e.file.ReadAt(b, off)
			p.mu.Lock()
			e.users--
			p.cond.Broadcast()
			p.mu.Unlock()
			return n, err
		}
		if len(p.entries) >= checkpointFileSlots {
			var oldest *checkpointFileEntry
			var path string
			for k, e := range p.entries {
				if e.users == 0 && (oldest == nil || e.used < oldest.used) {
					oldest, path = e, k
				}
			}
			if oldest == nil {
				p.cond.Wait()
				continue
			}
			if err := oldest.file.Close(); err != nil {
				p.err = errors.Join(p.err, err)
			}
			delete(p.entries, path)
		}
		f, err := os.OpenFile(r.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			p.mu.Unlock()
			return 0, readerr.Mark(err, false)
		}
		info, err := f.Stat()
		if err == nil && !sameCheckpointFile(r.identity, info) {
			err = errors.New("checkpoint carrier identity changed on reopen")
		}
		if err != nil {
			_ = f.Close()
			p.mu.Unlock()
			return 0, readerr.Mark(err, false)
		}
		p.entries[r.path] = &checkpointFileEntry{file: f, identity: info}
	}
}
func (p *checkpointFilePool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cond.Broadcast()
	for {
		busy := false
		for _, e := range p.entries {
			busy = busy || e.users != 0
		}
		if !busy {
			break
		}
		p.cond.Wait()
	}
	for k, e := range p.entries {
		p.err = errors.Join(p.err, e.file.Close())
		delete(p.entries, k)
	}
	return p.err
}

// The existing parser retains its run and crypto capabilities. The pool owns
// physical files; this wrapper owns only this parsed source's resources.
type checkpointParsedSource struct{ sparse.Source }

func (s *checkpointParsedSource) Close() error {
	if c, ok := s.Source.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
func (s *checkpointParsedSource) Digest() (string, string) {
	if d, ok := s.Source.(tarstream.Digester); ok {
		return d.Digest()
	}
	return "", ""
}
func (s *checkpointParsedSource) TarStreamDigest(name string) ([32]byte, bool) {
	if d, ok := s.Source.(tarstream.IdentityProvider); ok {
		return d.TarStreamDigest(name)
	}
	return [32]byte{}, false
}
func (s *checkpointParsedSource) PayloadCommitment() (uint64, [32]byte, bool) {
	if d, ok := s.Source.(tarstream.IdentityProvider); ok {
		return d.PayloadCommitment()
	}
	return s.Size(), [32]byte{}, false
}
func checkpointTarSource(r *checkpointFileReader, options ...tarstream.ReadOption) (*checkpointParsedSource, error) {
	source, _, err := tarstream.SourceAt(r, r.identity.Size(), "", options...)
	if err != nil {
		return nil, err
	}
	if _, ok := source.(tarstream.Digester); !ok {
		if c, ok := source.(io.Closer); ok {
			_ = c.Close()
		}
		return nil, fmt.Errorf("checkpoint tarstream is missing content identity")
	}
	return &checkpointParsedSource{source}, nil
}

package artifact

import (
	"context"
	"errors"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// OpenOwnedSnapshot transfers both a private/borrowed storage view and stream
// to the opened root. Reader watchers finish before that storage view closes.
// Callers with a process-owned storage use its OpenSnapshot method instead.
func OpenOwnedSnapshot(ctx context.Context, storage *ProcessStorage, stream fetch.Stream, opts SnapshotOpenOptions) (*snapshotfile.Root, error) {
	if storage == nil || stream == nil {
		var err error
		if stream != nil {
			err = stream.Close()
		}
		if storage != nil {
			err = errors.Join(err, storage.Close())
		}
		return nil, errors.Join(errors.New("owned snapshot requires storage and stream"), err)
	}
	owned := &storageSnapshotStream{Stream: stream, storage: storage}
	return storage.OpenSnapshot(ctx, owned, opts)
}

type storageSnapshotStream struct {
	fetch.Stream
	storage *ProcessStorage
	once    sync.Once
	err     error
}

func (s *storageSnapshotStream) ResolvedPath() string {
	if opened, ok := s.Stream.(interface{ ResolvedPath() string }); ok {
		return opened.ResolvedPath()
	}
	return ""
}

func (s *storageSnapshotStream) Close() error {
	s.once.Do(func() { s.err = errors.Join(s.Stream.Close(), s.storage.Close()) })
	return s.err
}

func (s *storageSnapshotStream) TarStreamDigest(name string) ([32]byte, bool) {
	if provider, ok := s.Stream.(tarstream.IdentityProvider); ok {
		return provider.TarStreamDigest(name)
	}
	return [32]byte{}, false
}

func (s *storageSnapshotStream) PayloadCommitment() (uint64, [32]byte, bool) {
	if provider, ok := s.Stream.(tarstream.IdentityProvider); ok {
		return provider.PayloadCommitment()
	}
	return s.Size(), [32]byte{}, false
}

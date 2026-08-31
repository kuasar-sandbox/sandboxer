package artifact

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"golang.org/x/sys/unix"
)

// locationPublishTarget is deliberately independent from snapshot.FileSink.
// FileSink is a node-local capture sink whose same-directory temporary file is
// committed with an atomic no-replace rename. A named location is a shared
// immutable-object target: it determines the canonical identity without a
// target write, exclusively creates the content-addressed final, and encodes
// directly into that final exactly once. Fresh publication relies on checked
// writes, fsync, and a lightweight final-path identity check; only an O_EXCL
// collision is independently opened and fully validated before reuse.
type locationPublishTarget struct {
	location  string
	directory string
	codec     tarstream.Codec
	required  bool
	logf      func(string, ...any)

	fs       locationFileSystem
	retry    locationRetryPolicy
	validate locationValidator
}

type locationRetryPolicy struct {
	window  time.Duration
	initial time.Duration
	maximum time.Duration
}

var defaultLocationRetryPolicy = locationRetryPolicy{
	// A large snapshot can remain partial for substantially longer than a
	// sub-second corruption probe on a shared filesystem. Keep the wait finite,
	// but long enough for a healthy concurrent writer to make useful progress.
	window:  2 * time.Minute,
	initial: 25 * time.Millisecond,
	maximum: time.Second,
}

type locationValidator func(
	context.Context,
	string,
	string,
	uint64,
	string,
	string,
	bool,
) error

type locationWriteFile interface {
	io.Writer
	Stat() (os.FileInfo, error)
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

type locationReadFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Sync() error
	Close() error
}

type locationFileSystem interface {
	createExclusive(string) (locationWriteFile, error)
	openNoFollow(string) (locationReadFile, error)
	lstat(string) (os.FileInfo, error)
	remove(string) error
	syncDirectory(string) error
}

type osLocationFileSystem struct{}

func (osLocationFileSystem) createExclusive(path string) (locationWriteFile, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
}

func (osLocationFileSystem) openNoFollow(path string) (locationReadFile, error) {
	// O_NONBLOCK makes an unexpected FIFO fail closed at fstat instead of
	// waiting for a writer. It has no effect on regular-file reads.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (osLocationFileSystem) lstat(path string) (os.FileInfo, error) {
	return os.Lstat(path)
}

func (osLocationFileSystem) remove(path string) error { return os.Remove(path) }

func (osLocationFileSystem) syncDirectory(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), path)
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func newLocationPublishTarget(location, directory string, codec tarstream.Codec, required bool, logf func(string, ...any)) *locationPublishTarget {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &locationPublishTarget{
		location: location, directory: directory, codec: codec, required: required, logf: logf,
		fs: osLocationFileSystem{}, retry: defaultLocationRetryPolicy,
	}
}

func (t *locationPublishTarget) Put(ctx context.Context, role LogicalRole, source sparse.Source) (string, error) {
	if source == nil {
		return "", fmt.Errorf("publish location %s: source is nil", role)
	}
	if t.required && t.codec == nil {
		return "", fmt.Errorf("publish location %s: required policy has no codec", role)
	}
	payload, err := locationPayloadName(role)
	if err != nil {
		return "", err
	}

	// tarstream identity covers canonical bytes before the marker and is only
	// known after reading the logical source. Publisher constructs every source
	// passed to this private target from a random-access fetch.Stream (possibly
	// with a deterministic in-memory tail), so two reads are supported even
	// though sparse.Source's baseline contract also permits one-pass sources.
	// The first pass writes nowhere; the second pass is the sole complete write
	// in the shared target.
	scheme, digest, err := t.determineIdentity(ctx, payload, source)
	if err != nil {
		return "", fmt.Errorf("publish location %s: determine identity: %w", role, err)
	}
	basename := digest + "." + payload
	ref := manifest.Ref{
		Scheme: manifest.RefSchemeFile, Path: basename,
		DigestScheme: scheme, Digest: digest, Location: t.location,
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	destination := filepath.Join(t.directory, basename)

	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("publish location %s: %w", role, err)
		}
		created, err := t.fs.createExclusive(destination)
		if err != nil {
			if !os.IsExist(err) {
				return "", fmt.Errorf("publish location %s: create final exclusively: %w", role, err)
			}
			err = t.reuseExisting(ctx, destination, payload, source.Size(), scheme, digest)
			if errors.Is(err, errLocationFinalVanished) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("publish location %s: %w", role, err)
			}
			t.logf("publish: %s -> %s (reused)", role, ref.String())
			return ref.String(), nil
		}
		if err := t.publishFresh(ctx, created, destination, payload, source, scheme, digest); err != nil {
			return "", fmt.Errorf("publish location %s: %w", role, err)
		}
		t.logf("publish: %s -> %s", role, ref.String())
		return ref.String(), nil
	}
}

func (*locationPublishTarget) Close() error { return nil }

func locationPayloadName(role LogicalRole) (string, error) {
	switch role {
	case RoleOverlay:
		return "overlay", nil
	case RoleSandbox:
		return "sandbox", nil
	case RoleSnapshot:
		return "snapshot", nil
	default:
		return "", fmt.Errorf("unsupported publish role %q", role)
	}
}

func (t *locationPublishTarget) determineIdentity(ctx context.Context, payload string, source sparse.Source) (string, string, error) {
	scheme, digest, err := tarstream.WriteTo(ctx, io.Discard, payload, source)
	if err != nil || t.codec == nil {
		return scheme, digest, err
	}
	var plain [32]byte
	if _, err := hex.Decode(plain[:], []byte(digest)); err != nil {
		return "", "", fmt.Errorf("invalid canonical identity: %w", err)
	}
	keyed := t.codec.KeyedDigest(plain)
	return tarstream.DigestSchemeHMAC, hex.EncodeToString(keyed[:]), nil
}

func (t *locationPublishTarget) publishFresh(
	ctx context.Context,
	created locationWriteFile,
	destination string,
	payload string,
	source sparse.Source,
	scheme string,
	digest string,
) (retErr error) {
	ownedInfo, err := created.Stat()
	if err != nil {
		closeErr := created.Close()
		// Without fstat identity, path-based removal could delete a replacement.
		// Leave the unknown entry for explicit cleanup rather than guessing.
		return fmt.Errorf("stat exclusively-created final: %w", errors.Join(err, closeErr))
	}
	owned := true
	defer func() {
		if !owned {
			return
		}
		if cleanupErr := t.removeOwned(destination, ownedInfo); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove owned incomplete final: %w", cleanupErr))
		}
	}()

	closed := false
	closeCreated := func() error {
		if closed {
			return nil
		}
		closed = true
		return created.Close()
	}
	if err := created.Chmod(0o644); err != nil {
		return fmt.Errorf("set final permissions: %w", errors.Join(err, closeCreated()))
	}
	options := t.writeOptions()
	writtenScheme, writtenDigest, err := tarstream.WriteTo(ctx, created, payload, source, options...)
	if err != nil {
		return fmt.Errorf("write final: %w", errors.Join(err, closeCreated()))
	}
	if writtenScheme != scheme || writtenDigest != digest {
		return errors.Join(
			errors.New("write final: logical source changed between identity and publication"),
			closeCreated(),
		)
	}
	if err := created.Sync(); err != nil {
		return fmt.Errorf("sync final: %w", errors.Join(err, closeCreated()))
	}
	if err := closeCreated(); err != nil {
		return fmt.Errorf("close final: %w", err)
	}
	// validate is an optional fault-injection seam. Production fresh publication
	// deliberately does not reopen and reread the shared final: WriteTo checked
	// every source read and destination write, reproduced the identity selected
	// by the first pass, and the exclusively-created fd has been synced and
	// closed. Readers authenticate and verify the tarstream on use. A collision,
	// whose inode is not owned by this publisher, still requires the independent
	// full validation in reuseExisting.
	if t.validate != nil {
		if err := t.validateFinal(ctx, destination, payload, source.Size(), scheme, digest, false); err != nil {
			return fmt.Errorf("validate final: %w", err)
		}
	}
	current, err := t.fs.lstat(destination)
	if os.IsNotExist(err) {
		return errors.Join(errLocationFinalVanished, err)
	}
	if err != nil {
		return fmt.Errorf("stat fresh final path: %w", err)
	}
	if !os.SameFile(ownedInfo, current) {
		return fmt.Errorf("%w: path changed after write", errLocationFinalVanished)
	}
	// The owned final is complete and its canonical path still names the same
	// inode. Do not remove it if only the subsequent directory durability step
	// fails; a concurrent publisher may already have reused it.
	owned = false
	if err := t.fs.syncDirectory(t.directory); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}

var (
	errLocationFinalMismatch   = errors.New("location final mismatch")
	errLocationFinalNonRegular = errors.New("location final is not a regular file")
	errLocationFinalVanished   = errors.New("location final vanished")
)

func (t *locationPublishTarget) reuseExisting(ctx context.Context, path, payload string, logicalSize uint64, scheme, digest string) error {
	started := time.Now()
	delay := t.retry.initial
	if delay <= 0 {
		delay = time.Millisecond
	}
	maximum := t.retry.maximum
	if maximum < delay {
		maximum = delay
	}
	window := t.retry.window
	if window <= 0 {
		window = delay
	}

	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := t.validateFinal(ctx, path, payload, logicalSize, scheme, digest, true)
		if err == nil {
			if err := t.fs.syncDirectory(t.directory); err != nil {
				return fmt.Errorf("sync parent directory after reuse: %w", err)
			}
			return nil
		}
		if os.IsNotExist(err) {
			return errors.Join(errLocationFinalVanished, err)
		}
		if errors.Is(err, errLocationFinalVanished) {
			return err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, errLocationFinalNonRegular) || !isRetryableLocationMismatch(err) {
			return fmt.Errorf("validate existing content-addressed final: %w", err)
		}
		lastErr = err
		remaining := window - time.Since(started)
		if remaining <= 0 {
			return stableInvalidLocationFinal(lastErr)
		}
		wait := min(delay, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		if delay < maximum {
			delay = min(delay*2, maximum)
		}
	}
}

func stableInvalidLocationFinal(cause error) error {
	return fmt.Errorf(
		"existing content-addressed final is incomplete or invalid; cleanup/repair is required before retry: %w",
		cause,
	)
}

func isRetryableLocationMismatch(err error) bool {
	for _, mismatch := range []error{
		errLocationFinalMismatch,
		io.EOF,
		io.ErrUnexpectedEOF,
		tarstream.ErrCodecRequired,
		tarstream.ErrPlaintextForbidden,
		tarstream.ErrUnsupportedVersion,
		tarstream.ErrMalformedEnvelope,
		tarstream.ErrAuthentication,
		tarstream.ErrDigestMismatch,
		tarstream.ErrInvalidCanonicalTarstream,
		tarstream.ErrUnsupportedEncoding,
		tarstream.ErrNotFound,
	} {
		if errors.Is(err, mismatch) {
			return true
		}
	}
	return false
}

func (t *locationPublishTarget) validateFinal(ctx context.Context, path, payload string, logicalSize uint64, scheme, digest string, syncFile bool) error {
	if t.validate != nil {
		return t.validate(ctx, path, payload, logicalSize, scheme, digest, syncFile)
	}
	return t.validateLocationFinal(ctx, path, payload, logicalSize, scheme, digest, syncFile)
}

func (t *locationPublishTarget) validateLocationFinal(ctx context.Context, path, payload string, logicalSize uint64, scheme, digest string, syncFile bool) (retErr error) {
	file, err := t.fs.openNoFollow(path)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return fmt.Errorf("%w: symlink", errLocationFinalNonRegular)
		}
		if info, statErr := t.fs.lstat(path); statErr == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("%w: mode %s", errLocationFinalNonRegular, info.Mode())
		}
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: mode %s", errLocationFinalNonRegular, info.Mode())
	}
	options := []tarstream.ReadOption{tarstream.WithExpectedDigest(scheme, digest)}
	if t.codec != nil {
		// A codec-backed target always writes ciphertext, including under auto.
		// Plaintext at the keyed final name must never satisfy reuse.
		options = append([]tarstream.ReadOption{tarstream.WithCodec(t.codec, true)}, options...)
	}
	source, name, err := tarstream.SourceFrom(file, payload, options...)
	if err != nil {
		return classifyLocationContentError(err)
	}
	if name != payload {
		return fmt.Errorf("%w: payload name %q", errLocationFinalMismatch, name)
	}
	if source.Size() != logicalSize {
		return fmt.Errorf("%w: logical size", errLocationFinalMismatch)
	}
	if err := consumeLocationSource(ctx, source); err != nil {
		return classifyLocationContentError(err)
	}
	if syncFile {
		if err := file.Sync(); err != nil {
			return fmt.Errorf("sync existing final: %w", err)
		}
	}
	current, err := t.fs.lstat(path)
	if os.IsNotExist(err) {
		return errors.Join(errLocationFinalVanished, err)
	}
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return fmt.Errorf("%w: path changed during validation", errLocationFinalVanished)
	}
	return nil
}

func classifyLocationContentError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// os.File surfaces filesystem failures as PathError/SyscallError or an
	// errno. They are operational failures, not evidence that the immutable
	// content is corrupt, and must not enter the stable-invalid retry category.
	var pathErr *os.PathError
	var syscallErr *os.SyscallError
	var errno syscall.Errno
	if errors.As(err, &pathErr) || errors.As(err, &syscallErr) || errors.As(err, &errno) {
		return err
	}
	return errors.Join(errLocationFinalMismatch, err)
}

func (t *locationPublishTarget) writeOptions() []tarstream.WriteOption {
	if t.codec == nil {
		return nil
	}
	return []tarstream.WriteOption{tarstream.WithCodec(t.codec, t.required)}
}

func (t *locationPublishTarget) removeOwned(path string, owned os.FileInfo) error {
	current, err := t.fs.lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(owned, current) {
		return nil
	}
	return t.fs.remove(path)
}

func consumeLocationSource(ctx context.Context, source sparse.Source) error {
	buffer := make([]byte, 128*1024)
	for offset := uint64(0); offset < source.Size(); {
		if err := ctx.Err(); err != nil {
			return err
		}
		run, err := source.RunAt(offset, source.Size()-offset)
		if err != nil {
			return err
		}
		if run == nil || run.Offset() != offset || run.End() <= offset || run.End() > source.Size() {
			return fmt.Errorf("invalid sparse run at offset %d", offset)
		}
		kind, end := run.Kind(), run.End()
		if kind != sparse.Hole {
			for position := offset; position < end; {
				chunk := min(uint64(len(buffer)), end-position)
				n, readErr := source.ReadAt(ctx, buffer[:int(chunk)], position)
				if readErr != nil && readErr != io.EOF {
					return readErr
				}
				if n != int(chunk) {
					return io.ErrUnexpectedEOF
				}
				position += chunk
			}
		}
		offset = end
	}
	return nil
}

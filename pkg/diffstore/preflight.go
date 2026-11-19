package diffstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// Filesystem preflight is a composition helper above the storage backends.
// Neither Single nor Versioned interprets filesystem bytes.
// ValidateExistingDiffExt4 verifies the effective filesystem seen through an
// existing active diff and its optional immutable base. It opens the diff
// read-only, applies the same local-encryption policy as OpenDisk, and
// checks the ext4 superblock magic in the combined view without mutating the
// diff or inventing sparse data.
func ValidateExistingDiffExt4(ctx context.Context, path string, base BlockReader, rawOptions ...DiskOption) error {
	options, err := parseDiskOptions(rawOptions)
	if err != nil {
		return err
	}
	if options.checkpoint != nil {
		source, err := openCheckpointPreflight(ctx, path, options)
		if err != nil {
			return err
		}
		return errors.Join(ValidateDiffTemplateExt4(ctx, source, base), source.Close())
	}
	diff, err := openExistingDiffFile(path, options.encryption, options.required, true)
	if err != nil {
		return err
	}
	if err := diff.enableDirect(); err != nil {
		return errors.Join(err, diff.Close())
	}
	return errors.Join(ValidateDiffTemplateExt4(ctx, &singleDiffSource{diff: diff}, base), diff.Close())
}

// ValidateDiffTemplateExt4 checks the ext4 magic in the effective template/base
// view. It borrows both sources and never closes them. This filesystem preflight
// is not full content-identity verification; TemplateVerifier runs separately as
// part of successful materialization.
func ValidateDiffTemplateExt4(ctx context.Context, source sparse.Source, base BlockReader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if source == nil || source.Size() < uint64(ext4MagicOffset+2) {
		return fmt.Errorf("diffstore: ext4 source is too small")
	}
	if source.Size() > uint64(^uint64(0)>>1) {
		return fmt.Errorf("diffstore: ext4 source size overflows int64")
	}
	if base != nil && base.Size() > int64(source.Size()) {
		return fmt.Errorf("diffstore: base size %d > diff size %d", base.Size(), source.Size())
	}
	var magic [2]byte
	if _, err := readTemplateBase(ctx, source, base, magic[:], uint64(ext4MagicOffset)); err != nil {
		return fmt.Errorf("diffstore: read ext4 superblock: %w", err)
	}
	if magic != [2]byte{0x53, 0xef} {
		return fmt.Errorf("diffstore: effective writable disk is not a formatted ext4 filesystem")
	}
	return nil
}

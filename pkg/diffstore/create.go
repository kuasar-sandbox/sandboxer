package diffstore

import (
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/versioned"
	"golang.org/x/sys/unix"
)

// createFreshDiff owns claim/template/temp resources from acquisition through
// atomic publication. Only format initialization differs between backends.
func createFreshDiff(path string, init DiffInit, options diskOptions) (*freshDiffInit, error) {
	if init.Template == nil {
		if err := validateDiffLogicalSize(init.CreateSize); err != nil {
			return nil, err
		}
	}
	check := func() error {
		_, err := os.Lstat(path)
		if err == nil {
			return unix.EEXIST
		}
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := check(); err != nil {
		return nil, err
	}
	claim, claimPath, err := acquireFreshDiffClaim(path)
	if err != nil {
		return nil, err
	}
	fresh := &freshDiffInit{finalPath: path, claim: claim, claimPath: claimPath}
	fail := func(err error) (*freshDiffInit, error) { return nil, errors.Join(err, fresh.abort()) }
	if err := check(); err != nil {
		return fail(err)
	}
	size := init.CreateSize
	if init.Template != nil {
		template, err := init.Template()
		if err != nil {
			return fail(err)
		}
		if template == nil {
			return fail(fmt.Errorf("diffstore: template factory returned a nil source"))
		}
		fresh.template = template
		if template.Size() > uint64(^uint64(0)>>1) {
			return fail(fmt.Errorf("diffstore: template size overflows int64"))
		}
		size = int64(template.Size())
	}
	if err := validateDiffLogicalSize(size); err != nil {
		return fail(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.partial")
	if err != nil {
		return fail(err)
	}
	fresh.tmpPath = tmp.Name()
	if options.checkpoint != nil {
		if err := tmp.Close(); err != nil {
			return fail(err)
		}
		if err := os.Remove(fresh.tmpPath); err != nil {
			return fail(err)
		}
		store, err := versioned.CreatePending(fresh.tmpPath, checkpointStoreOptions(options, size, path))
		if err != nil {
			return fail(err)
		}
		fresh.diff = wrapCheckpointDiff(store, path)
	} else {
		target, err := createDiffFile(tmp, size, options.encryption, cryptorand.Reader)
		if err != nil {
			return fail(errors.Join(err, tmp.Close()))
		}
		target.path = path
		fresh.diff = target
		if err := target.validateFreshBodySparse(); err != nil {
			return fail(err)
		}
		if err := target.enableDirect(); err != nil {
			return fail(err)
		}
	}
	return fresh, nil
}

// publish completes materialization durably before releasing the creation claim.
// A failure after rename leaves the committed data in place for inspection.
func (f *freshDiffInit) publish() error {
	if err := f.publishHook("before-sync"); err != nil {
		return err
	}
	if err := f.diff.Sync(); err != nil {
		return err
	}
	return f.commit()
}

func (f *freshDiffInit) commit() error {
	if err := commitFreshDiff(f.tmpPath, f.finalPath); err != nil {
		return err
	}
	f.committed = true
	if err := f.publishHook("renamed"); err != nil {
		return err
	}
	if err := f.releaseClaim(); err != nil {
		return err
	}
	if err := f.publishHook("directory-sync"); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(f.finalPath))
}

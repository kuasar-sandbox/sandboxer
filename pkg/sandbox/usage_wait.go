package sandbox

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// observeCHExit is called only by the existing cmd.Wait owner. The narrow
// waitID seam permits EINTR/error tests without a second process reaper.
// cmd.Wait must still follow, including on waitID failure, to drain output.
func observeCHExit(waitID func() error, observed func(), final func(context.Context)) error {
	var err error
	for {
		err = waitID()
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err == nil {
		observed() // Runtime observations end before native final sampling.
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err != nil {
		// A canceled observation closes admission and marks unknown native
		// terminal segments; it must not pretend proc retained the endpoint.
		cancel()
	}
	final(ctx)
	return err
}

package sandbox

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type runSignalContextKey struct{}

type runSignalStream struct {
	signals <-chan os.Signal
}

// NotifyRunContext registers the sandbox lifecycle's signal source before
// admission. The first SIGTERM/SIGINT cancels ctx so controller retries stop;
// every signal is also retained for ServeAndWait's existing CH shutdown and
// second-signal escalation protocol.
func NotifyRunContext(parent context.Context) (context.Context, context.CancelFunc) {
	source := make(chan os.Signal, 4)
	signal.Notify(source, syscall.SIGTERM, syscall.SIGINT)
	return newRunSignalContext(parent, source, func() { signal.Stop(source) })
}

func newRunSignalContext(parent context.Context, source <-chan os.Signal, stopSource func()) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	forwarded := make(chan os.Signal, 4)
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		for {
			select {
			case sig, ok := <-source:
				if !ok {
					return
				}
				cancel()
				select {
				case forwarded <- sig:
				default:
				}
			case <-stop:
				return
			}
		}
	}()
	ctx = context.WithValue(ctx, runSignalContextKey{}, runSignalStream{signals: forwarded})
	return ctx, func() {
		once.Do(func() {
			if stopSource != nil {
				stopSource()
			}
			close(stop)
			<-done
			cancel()
		})
	}
}

func runSignalsFromContext(ctx context.Context) <-chan os.Signal {
	if ctx == nil {
		return nil
	}
	stream, _ := ctx.Value(runSignalContextKey{}).(runSignalStream)
	return stream.signals
}

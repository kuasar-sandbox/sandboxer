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
	signals           <-chan os.Signal
	controllerContext context.Context
}

// NotifyRunContext registers the sandbox lifecycle's signal source before
// admission. The first SIGTERM/SIGINT cancels ControllerWorkContext(ctx) so
// controller retries stop; ctx itself remains live until ServeAndWait performs
// its existing graceful CH shutdown. Every signal is retained for that shutdown
// and the second-signal escalation protocol.
func NotifyRunContext(parent context.Context) (context.Context, context.CancelFunc) {
	source := make(chan os.Signal, 4)
	signal.Notify(source, syscall.SIGTERM, syscall.SIGINT)
	return newRunSignalContext(parent, source, func() { signal.Stop(source) })
}

func newRunSignalContext(parent context.Context, source <-chan os.Signal, stopSource func()) (context.Context, context.CancelFunc) {
	controllerCtx, cancelController := context.WithCancel(parent)
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
				cancelController()
				select {
				case forwarded <- sig:
				default:
				}
			case <-stop:
				return
			}
		}
	}()
	ctx := context.WithValue(parent, runSignalContextKey{}, runSignalStream{
		signals: forwarded, controllerContext: controllerCtx,
	})
	return ctx, func() {
		once.Do(func() {
			if stopSource != nil {
				stopSource()
			}
			close(stop)
			<-done
			cancelController()
		})
	}
}

// ControllerWorkContext returns the signal-cancelled context used only for
// resource-controller connection and enforcement work. Other VM services must
// keep using ctx so CH can drain while its vhost/vsock backends remain alive.
func ControllerWorkContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	stream, _ := ctx.Value(runSignalContextKey{}).(runSignalStream)
	if stream.controllerContext != nil {
		return stream.controllerContext
	}
	return ctx
}

func runSignalsFromContext(ctx context.Context) <-chan os.Signal {
	if ctx == nil {
		return nil
	}
	stream, _ := ctx.Value(runSignalContextKey{}).(runSignalStream)
	return stream.signals
}

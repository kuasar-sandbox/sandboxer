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
	vmContext         context.Context
}

// NotifyRunContext registers the sandbox lifecycle's signal source before
// admission. The first SIGTERM/SIGINT cancels ctx and
// ControllerWorkContext(ctx), aborting work which has not spawned CH yet. Once
// CH exists, ServeAndWait keeps its backend services on VMLifecycleContext(ctx)
// and consumes every retained signal through the existing graceful shutdown and
// second-signal escalation protocol.
func NotifyRunContext(parent context.Context) (context.Context, context.CancelFunc) {
	source := make(chan os.Signal, 4)
	signal.Notify(source, syscall.SIGTERM, syscall.SIGINT)
	return newRunSignalContext(parent, source, func() { signal.Stop(source) })
}

func newRunSignalContext(parent context.Context, source <-chan os.Signal, stopSource func()) (context.Context, context.CancelFunc) {
	preSpawnCtx, cancelPreSpawn := context.WithCancel(parent)
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
				cancelPreSpawn()
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
	ctx := context.WithValue(preSpawnCtx, runSignalContextKey{}, runSignalStream{
		signals: forwarded, controllerContext: controllerCtx, vmContext: parent,
	})
	return ctx, func() {
		once.Do(func() {
			if stopSource != nil {
				stopSource()
			}
			close(stop)
			<-done
			cancelPreSpawn()
			cancelController()
		})
	}
}

// VMLifecycleContext returns the context used by services which must remain
// alive while an already-spawned CH handles a retained shutdown signal. For
// callers without NotifyRunContext it is the original context.
func VMLifecycleContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	stream, _ := ctx.Value(runSignalContextKey{}).(runSignalStream)
	if stream.vmContext != nil {
		return stream.vmContext
	}
	return ctx
}

// ControllerWorkContext returns the signal-cancelled context used only for
// resource-controller connection and enforcement work. ServeAndWait derives
// already-spawned VM services from VMLifecycleContext so CH can drain while its
// vhost/vsock backends remain alive.
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

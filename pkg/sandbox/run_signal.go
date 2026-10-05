package sandbox

import (
	"context"
	"os"
	"sync"
)

type runSignalContextKey struct{}

type runSignalStream struct {
	signals           <-chan os.Signal
	shutdown          <-chan struct{}
	controllerContext context.Context
	vmContext         context.Context
}

// RunSignalContext adapts a caller-owned signal stream to executable shutdown
// policy. It never subscribes to OS signals. SDK lifecycle calls use a private
// stream driven only by Close; the CLI registers SIGTERM/SIGINT itself.
func RunSignalContext(parent context.Context, source <-chan os.Signal) (context.Context, context.CancelFunc) {
	return newRunSignalContext(parent, source, nil)
}

func newRunSignalContext(parent context.Context, source <-chan os.Signal, stopSource func()) (context.Context, context.CancelFunc) {
	preSpawnCtx, cancelPreSpawn := context.WithCancel(parent)
	controllerCtx, cancelController := context.WithCancel(parent)
	forwarded := make(chan os.Signal, 4)
	shutdown := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	var shutdownOnce sync.Once
	go func() {
		defer close(done)
		for {
			select {
			case sig, ok := <-source:
				if !ok {
					return
				}
				select {
				case forwarded <- sig:
				default:
				}
				shutdownOnce.Do(func() { close(shutdown) })
				cancelPreSpawn()
				cancelController()
			case <-stop:
				return
			}
		}
	}()
	ctx := context.WithValue(preSpawnCtx, runSignalContextKey{}, runSignalStream{
		signals: forwarded, shutdown: shutdown, controllerContext: controllerCtx, vmContext: parent,
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
// callers without RunSignalContext it is the original context.
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

func runShutdownRequested(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	stream, _ := ctx.Value(runSignalContextKey{}).(runSignalStream)
	if stream.shutdown == nil {
		return false
	}
	select {
	case <-stream.shutdown:
		return true
	default:
		return false
	}
}

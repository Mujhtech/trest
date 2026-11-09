package trest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// Runtime owns one started application lifecycle and implements http.Handler.
// Mount it on an existing server or mux. Shutdown rejects new requests, cancels
// active requests, drains their handlers, then closes resources in reverse order.
// The App itself remains usable without a lifecycle for in-memory request tests.
type Runtime struct {
	app            *App
	handler        http.Handler
	work           context.Context
	cancelWork     context.CancelFunc
	requests       context.Context
	cancelRequests context.CancelFunc
	mu             sync.Mutex
	active         int
	closing        bool
	drained        chan struct{}
	done           chan struct{}
	err            error
}

// Start acquires lifecycle resources without opening a listener. Mount the
// returned Runtime, and call its Shutdown even if the supplied context is
// canceled. The same App cannot have two active lifecycle owners.
func (a *App) Start(ctx context.Context) (*Runtime, error) {
	if ctx == nil {
		return nil, errors.New("trest: nil lifecycle context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.serveMu.Lock()
	if a.isRunning {
		a.serveMu.Unlock()
		return nil, errors.New("trest: app already running")
	}
	a.isRunning = true
	a.serveMu.Unlock()
	work, cancel := context.WithCancel(ctx)
	requests, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	r := &Runtime{app: a, handler: a, work: work, cancelWork: cancel, requests: requests, cancelRequests: cancelRequests, drained: make(chan struct{}), done: make(chan struct{})}
	started := 0
	for _, hook := range a.lifecycle {
		var err error
		if err = work.Err(); err == nil && hook.start != nil {
			err = callLifecycle(work, hook.start)
		}
		if err != nil {
			cancelRequests()
			cancel()
			cleanup := r.stop(started)
			r.release()
			return nil, errors.Join(err, cleanup)
		}
		started++
	}
	return r, nil
}

// ServeHTTP tracks the entire mounted request so resources remain alive until
// the handler returns, including streaming responses and middleware cleanup.
func (r *Runtime) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		writeError(w, req, scope{}, NewError(Unavailable, "server shutting down", nil), http.StatusServiceUnavailable)
		return
	}
	r.active++
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(r.requests, cancel)
	defer func() {
		stop()
		cancel()
		r.mu.Lock()
		r.active--
		if r.closing && r.active == 0 {
			close(r.drained)
		}
		r.mu.Unlock()
	}()
	r.handler.ServeHTTP(w, req.WithContext(ctx))
}

func (r *Runtime) beginShutdown() {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return
	}
	r.closing = true
	if r.active == 0 {
		close(r.drained)
	}
	r.cancelRequests()
	r.cancelWork()
	r.mu.Unlock()
	go func() {
		<-r.drained
		err := r.stop(len(r.app.lifecycle))
		r.release()
		r.mu.Lock()
		r.err = err
		close(r.done)
		r.mu.Unlock()
	}()
}

// Shutdown starts shutdown once and waits for requests and resource cleanup.
// ctx limits this wait. If it expires, cleanup continues after handlers drain;
// another Shutdown call may wait for completion. Stop hooks receive a separate
// five-second cooperative cleanup context, even when the request context expired.
// An uncooperative active handler retains resources instead of closing them while
// they are still in use. Such an application cannot restart until cleanup ends.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("trest: nil shutdown context")
	}
	r.beginShutdown()
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) stop(started int) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.work), 5*time.Second)
	defer cancel()
	var errs []error
	for i := started - 1; i >= 0; i-- {
		if stop := r.app.lifecycle[i].stop; stop != nil {
			if err := callLifecycle(ctx, stop); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (r *Runtime) release() {
	r.app.serveMu.Lock()
	r.app.isRunning = false
	r.app.serveMu.Unlock()
}

// RunServer owns listener while serving with an application-configured server.
// server must not already be serving. A nil Handler uses this app; an existing
// Handler may be a mux containing the app. Its configuration is preserved except
// for the lifecycle wrapper installed as Handler. A TLS listener can be
// supplied by the caller. Request draining precedes resource cleanup.
func (a *App) RunServer(ctx context.Context, server *http.Server, listener net.Listener) error {
	if listener == nil {
		return errors.New("trest: listener required")
	}
	defer func() { _ = listener.Close() }()
	if ctx == nil || server == nil {
		return errors.New("trest: serve context and server required")
	}
	runtime, err := a.Start(ctx)
	if err != nil {
		return err
	}
	original := server.Handler
	if original != nil {
		runtime.handler = original
	}
	server.Handler = runtime
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	var serveErr error
	select {
	case serveErr = <-result:
	case <-ctx.Done():
	}
	runtime.beginShutdown()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = server.Close()
	}
	if serveErr == nil {
		serveErr = <-result
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr, runtime.Shutdown(shutdownCtx))
}

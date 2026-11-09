package trest_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"trest.local/trest"
)

func TestRuntimeDrainsRequestsBeforeResourceCleanup(t *testing.T) {
	t.Parallel()
	var open, stopped atomic.Bool
	entered, canceled, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	app, err := trest.New(
		trest.Lifecycle(func(context.Context) error { open.Store(true); return nil }, func(context.Context) error {
			select {
			case <-returned:
			default:
				t.Error("resource stopped while handler still active")
			}
			open.Store(false)
			stopped.Store(true)
			return nil
		}),
		trest.Raw("GET", "/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
			close(canceled)
			<-release
			if !open.Load() {
				t.Error("active request lost its resource")
			}
			close(returned)
			w.WriteHeader(204)
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Start(context.Background()); err == nil {
		t.Fatal("two lifecycle owners admitted")
	}
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		runtime.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	<-entered
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err = runtime.Shutdown(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown: %v", err)
	}
	<-canceled
	if !open.Load() || stopped.Load() {
		t.Fatal("cleanup happened before draining")
	}
	w := httptest.NewRecorder()
	runtime.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 {
		t.Fatalf("new request admitted during shutdown: %d", w.Code)
	}
	close(release)
	<-requestDone
	ctx, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	if err = runtime.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if open.Load() || !stopped.Load() {
		t.Fatal("resource not cleaned up")
	}
	if err = runtime.Shutdown(ctx); err != nil {
		t.Fatalf("idempotent shutdown: %v", err)
	}
}

func TestLifecycleCanStartAfterCompleteShutdown(t *testing.T) {
	t.Parallel()
	var starts, stops atomic.Int32
	app, err := trest.New(trest.Lifecycle(func(context.Context) error { starts.Add(1); return nil }, func(context.Context) error { stops.Add(1); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		run, err := app.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = run.Shutdown(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 2 || stops.Load() != 2 {
		t.Fatalf("ownership: starts=%d stops=%d", starts.Load(), stops.Load())
	}
}

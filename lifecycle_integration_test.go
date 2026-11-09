//go:build integration

package trest_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"trest.local/trest"
	"trest.local/trest/plugins/schedule"
)

func TestCustomServerOwnsLifecycleAndDrains(t *testing.T) {
	t.Parallel()
	started, taskStopped, requestEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var available atomic.Bool
	background, err := schedule.New(schedule.Task{Name: "watch", Every: time.Hour, RunImmediately: true, Run: func(ctx context.Context) error { close(started); <-ctx.Done(); close(taskStopped); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	app, err := trest.New(
		trest.Lifecycle(func(context.Context) error { available.Store(true); return nil }, func(context.Context) error { available.Store(false); return nil }),
		background,
		trest.Raw("GET", "/work", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(requestEntered)
			<-r.Context().Done()
			if !available.Load() {
				t.Error("resource closed before handler finished")
			}
			w.WriteHeader(204)
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", app)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, MaxHeaderBytes: 8192}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- app.RunServer(ctx, server, listener) }()
	<-started
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		res, err := client.Get("http://" + listener.Addr().String() + "/work")
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode != 204 {
				t.Errorf("status %d", res.StatusCode)
			}
		}
		response <- err
	}()
	select {
	case <-requestEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not enter")
	}
	cancel()
	select {
	case err = <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("custom server did not stop")
	}
	if err = <-response; err != nil {
		t.Fatal(err)
	}
	select {
	case <-taskStopped:
	default:
		t.Fatal("background task retained ownership")
	}
	if available.Load() {
		t.Fatal("resource left open")
	}
	if server.ReadHeaderTimeout != 3*time.Second || server.MaxHeaderBytes != 8192 {
		t.Fatal("custom configuration overwritten")
	}
}

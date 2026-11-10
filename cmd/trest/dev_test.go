package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDevProcess struct {
	done  chan struct{}
	once  sync.Once
	stops *atomic.Int32
}

func (p *fakeDevProcess) Done() <-chan struct{} { return p.done }
func (p *fakeDevProcess) Err() error            { return nil }
func (p *fakeDevProcess) Stop(context.Context) error {
	p.once.Do(func() { p.stops.Add(1); close(p.done) })
	return nil
}

type eventWriter struct{ events chan string }

func (w eventWriter) Write(data []byte) (int, error) {
	if strings.Contains(string(data), `"event": "build_failed"`) {
		w.events <- "build_failed"
	}
	return len(data), nil
}

func TestDevReloadPreservesProcessOnBuildFailureAndStopsOnCancel(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "cmd", "invoices", "main.go")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var builds, starts, stops atomic.Int32
	ready := make(chan struct{}, 4)
	failed := make(chan string, 4)
	deps := devDependencies{
		build: func(context.Context, string, string) error {
			if builds.Add(1) == 2 {
				return errors.New("compile failed")
			}
			return nil
		},
		start: func(string, string, io.Writer) (devProcess, error) {
			starts.Add(1)
			return &fakeDevProcess{done: make(chan struct{}), stops: &stops}, nil
		},
		ready: func(context.Context, string, string) error { ready <- struct{}{}; return nil },
	}
	completed := make(chan error, 1)
	go func() {
		completed <- devLoop(ctx, devConfig{directory: directory, address: "127.0.0.1:18080", watch: true, poll: 50 * time.Millisecond, buildTimeout: time.Second, output: eventWriter{failed}}, deps)
	}()
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case err := <-completed:
			t.Fatalf("dev exited early: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("dev timed out")
		}
	}
	wait(ready)
	if err := os.WriteFile(source, []byte("package main // broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-failed:
	case <-time.After(3 * time.Second):
		t.Fatal("failed rebuild not reported")
	}
	if starts.Load() != 1 || stops.Load() != 0 {
		t.Fatal("failed rebuild stopped the existing process")
	}
	if err := os.WriteFile(source, []byte("package main // repaired source"), 0o600); err != nil {
		t.Fatal(err)
	}
	wait(ready)
	if starts.Load() != 2 || stops.Load() != 1 {
		t.Fatalf("restart counts: start=%d stop=%d", starts.Load(), stops.Load())
	}
	cancel()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not finish")
	}
	if stops.Load() != 2 {
		t.Fatal("cancel did not stop the replacement process")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadinessRequiresMatchingInstance(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		instance := "other-process"
		if calls == 2 {
			instance = "expected"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Trest-Dev-Instance": []string{instance}}, Body: io.NopCloser(strings.NewReader(`{"ready":true}`)), Request: r}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitReadyWithClient(ctx, "127.0.0.1:18080", "expected", client); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("unrelated listener was treated as ready")
	}
}

func TestDevRejectsNonlocalAddressBeforeExecution(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8080", "example.com:8080", "127.0.0.1:0", "[::]:8080"} {
		if err := validateDev(devConfig{address: address, poll: time.Second, buildTimeout: time.Second}); err == nil {
			t.Fatalf("unsafe address %q accepted", address)
		}
	}
}

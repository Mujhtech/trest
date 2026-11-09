package schedule

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunnerCancellationAndNoOverlap(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	var calls atomic.Int32
	r := &runner{task: Task{Name: "one", Every: time.Millisecond, Timeout: time.Second, RunImmediately: true, Run: func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	if err := r.start(ctx); err == nil {
		t.Fatal("concurrent start accepted")
	}
	shutdown, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := r.stop(shutdown); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("stop returned before callback exited")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}
func TestRunnerBoundedShutdownPreservesOwnership(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	r := &runner{task: Task{Name: "uncooperative", Every: time.Hour, Timeout: time.Hour, RunImmediately: true, Run: func(context.Context) error { close(entered); <-release; return nil }}}
	if err := r.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("did not enter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop=%v", err)
	}
	if err := r.start(context.Background()); err == nil {
		t.Fatal("restarted while previous callback still running")
	}
	close(release)
	done, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := r.stop(done); err != nil {
		t.Fatal(err)
	}
}
func TestRunnerReportsPanicAndStops(t *testing.T) {
	reported := make(chan error, 1)
	r := &runner{task: Task{Name: "panic", Every: time.Hour, Timeout: time.Second, RunImmediately: true, Run: func(context.Context) error { panic("secret") }, OnError: func(err error) { reported <- err }}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reported:
		if err.Error() != "schedule: task panicked" {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("no error report")
	}
	stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := r.stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}
func TestNewDoesNotRunOnConstruction(t *testing.T) {
	var calls atomic.Int32
	if _, err := New(Task{Name: "never-started", Every: time.Millisecond, RunImmediately: true, Run: func(context.Context) error { calls.Add(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("construction invoked callback")
	}
}

package lazycontext_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mujhtech/trest/internal/lazycontext"
)

type valueKey struct{}

func TestContextDeadlineAndValues(t *testing.T) {
	parent := context.WithValue(context.Background(), valueKey{}, "parent value")
	before := time.Now().Add(time.Minute)
	var ctx lazycontext.Context
	ctx.Init(parent, time.Minute)
	defer ctx.Cancel()
	after := time.Now().Add(time.Minute)
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before) || deadline.After(after) {
		t.Fatalf("deadline = %v, %v; want between %v and %v", deadline, ok, before, after)
	}
	if ctx.Err() != nil || context.Cause(&ctx) != nil {
		t.Fatalf("new context canceled: %v, %v", ctx.Err(), context.Cause(&ctx))
	}
	if got := ctx.Value(valueKey{}); got != "parent value" {
		t.Fatalf("parent value = %v", got)
	}
	if ctx.Value(struct{}{}) != nil {
		t.Fatal("unknown key has a value")
	}
	firstDone := ctx.Done()
	if firstDone != ctx.Done() {
		t.Fatal("Done channel identity changed")
	}
}

func TestContextRejectsNilParentAndReuse(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{name: "nil parent", call: func() {
			var ctx lazycontext.Context
			ctx.Init(nil, time.Minute) //nolint:staticcheck // Verify nil-parent rejection.
		}},
		{name: "reuse", call: func() {
			var ctx lazycontext.Context
			ctx.Init(context.Background(), time.Minute)
			defer ctx.Cancel()
			ctx.Init(context.Background(), time.Minute)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			test.call()
		})
	}
}

func TestContextPreservesParentCancellationCause(t *testing.T) {
	for _, parentFirst := range []bool{true, false} {
		name := "child first"
		if parentFirst {
			name = "parent first"
		}
		t.Run(name, func(t *testing.T) {
			parent, cancelParent := context.WithCancelCause(context.Background())
			defer cancelParent(nil)
			var ctx lazycontext.Context
			ctx.Init(parent, time.Minute)
			defer ctx.Cancel()
			cause := errors.New("parent stopped")
			wantCause := error(context.Canceled)
			if parentFirst {
				cancelParent(cause)
				ctx.Cancel()
				wantCause = cause
			} else {
				ctx.Cancel()
				cancelParent(cause)
			}
			if ctx.Err() != context.Canceled || context.Cause(&ctx) != wantCause {
				t.Fatalf("error/cause = %v/%v, want canceled/%v", ctx.Err(), context.Cause(&ctx), wantCause)
			}
			assertClosed(t, ctx.Done())
		})
	}
}

func TestContextPreservesEarlierParentDeadline(t *testing.T) {
	cause := errors.New("parent timed out")
	parent, cancelParent := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), cause)
	defer cancelParent()
	var ctx lazycontext.Context
	ctx.Init(parent, time.Minute)
	defer ctx.Cancel()
	parentDeadline, _ := parent.Deadline()
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(parentDeadline) {
		t.Fatalf("deadline = %v, want %v", deadline, parentDeadline)
	}
	if ctx.Err() != context.DeadlineExceeded || context.Cause(&ctx) != cause {
		t.Fatalf("error/cause = %v/%v", ctx.Err(), context.Cause(&ctx))
	}
	assertClosed(t, ctx.Done())
}

func TestContextDoneExpires(t *testing.T) {
	var ctx lazycontext.Context
	ctx.Init(context.Background(), 5*time.Millisecond)
	defer ctx.Cancel()
	done := ctx.Done()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deadline did not close Done")
	}
	if ctx.Err() != context.DeadlineExceeded || context.Cause(&ctx) != context.DeadlineExceeded {
		t.Fatalf("error/cause = %v/%v", ctx.Err(), context.Cause(&ctx))
	}
	ctx.Cancel()
	if ctx.Done() != done || ctx.Err() != context.DeadlineExceeded {
		t.Fatal("cancel changed an expired context")
	}
}

func TestContextExpiredBeforeObservation(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		name := "observe first"
		if cancelFirst {
			name = "cancel first"
		}
		t.Run(name, func(t *testing.T) {
			var ctx lazycontext.Context
			ctx.Init(context.Background(), -time.Second)
			defer ctx.Cancel()
			if cancelFirst {
				ctx.Cancel()
			}
			if ctx.Err() != context.DeadlineExceeded || context.Cause(&ctx) != context.DeadlineExceeded {
				t.Fatalf("error/cause = %v/%v", ctx.Err(), context.Cause(&ctx))
			}
			assertClosed(t, ctx.Done())
		})
	}
}

func TestContextCancellationStaysCanceledPastDeadline(t *testing.T) {
	var ctx lazycontext.Context
	ctx.Init(context.Background(), 50*time.Millisecond)
	ctx.Cancel()
	// First observe the retained context after its original deadline. An
	// implementation that reconstructs just the deadline loses cancellation.
	deadline, _ := ctx.Deadline()
	<-time.After(time.Until(deadline) + time.Millisecond)
	if ctx.Err() != context.Canceled || context.Cause(&ctx) != context.Canceled {
		t.Fatalf("error/cause = %v/%v", ctx.Err(), context.Cause(&ctx))
	}
	assertClosed(t, ctx.Done())
	ctx.Cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("repeated cancel changed result")
	}
}

func TestContextWithoutCancelParentDoesNotLeakCause(t *testing.T) {
	parent, stopParent := context.WithCancelCause(context.Background())
	stopParent(errors.New("discarded parent cause"))
	var ctx lazycontext.Context
	ctx.Init(context.WithoutCancel(parent), time.Minute)
	if ctx.Err() != nil || context.Cause(&ctx) != nil {
		t.Fatal("WithoutCancel inherited parent cancellation")
	}
	ctx.Cancel()
	if context.Cause(&ctx) != context.Canceled {
		t.Fatalf("cause = %v, want canceled", context.Cause(&ctx))
	}
}

// This valid noncancelable wrapper still forwards all values, including private
// keys. The lazy context must install its own cancellation metadata before any
// unknown lookup rather than accidentally returning the canceled parent's cause.
type noncancelableParent struct{ context.Context }

func (noncancelableParent) Done() <-chan struct{}       { return nil }
func (noncancelableParent) Deadline() (time.Time, bool) { return time.Time{}, false }
func (noncancelableParent) Err() error                  { return nil }

func TestContextOwnCancellationMetadata(t *testing.T) {
	parent, cancelParent := context.WithCancelCause(context.Background())
	cancelParent(errors.New("hidden parent cancellation"))
	var ctx lazycontext.Context
	ctx.Init(noncancelableParent{Context: parent}, time.Minute)
	defer ctx.Cancel()
	if got := context.Cause(&ctx); got != nil {
		t.Fatalf("new context cause = %v", got)
	}
	ctx.Cancel()
	if ctx.Err() != context.Canceled || context.Cause(&ctx) != context.Canceled {
		t.Fatalf("error/cause = %v/%v", ctx.Err(), context.Cause(&ctx))
	}
}

func TestContextChildrenAndAfterFunc(t *testing.T) {
	var ctx lazycontext.Context
	ctx.Init(context.Background(), time.Minute)
	defer ctx.Cancel()
	child, cancelChild := context.WithCancel(&ctx)
	defer cancelChild()
	called := make(chan struct{})
	stop := context.AfterFunc(&ctx, func() { close(called) })
	defer stop()
	ctx.Cancel()
	assertClosed(t, child.Done())
	if child.Err() != context.Canceled || context.Cause(child) != context.Canceled {
		t.Fatalf("child error/cause = %v/%v", child.Err(), context.Cause(child))
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("AfterFunc was not called")
	}
}

func TestContextConcurrentObservationAndCancellation(t *testing.T) {
	parent := context.WithValue(context.Background(), valueKey{}, "parent value")
	var ctx lazycontext.Context
	ctx.Init(parent, time.Minute)
	defer ctx.Cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 100 {
				if ctx.Value(valueKey{}) != "parent value" {
					t.Error("parent value changed")
					return
				}
				firstDone := ctx.Done()
				if firstDone != ctx.Done() {
					t.Error("Done channel changed")
					return
				}
				_ = ctx.Err()
				_ = context.Cause(&ctx)
				_, _ = ctx.Deadline()
				ctx.Cancel()
			}
		}()
	}
	close(start)
	wg.Wait()
	assertClosed(t, ctx.Done())
	if ctx.Err() != context.Canceled || context.Cause(&ctx) != context.Canceled {
		t.Fatalf("error/cause = %v/%v", ctx.Err(), context.Cause(&ctx))
	}
}

func assertClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatal("Done is not closed")
	}
}

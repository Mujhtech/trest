package invocationctx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type requestContext struct {
	context.Context
	slot        Slot
	reservation any
}

func (c *requestContext) Value(key any) any {
	if IsSlotKey(key) {
		if c.reservation != nil {
			return c.reservation
		}
		return &c.slot
	}
	if IsRequestStateKey(key) {
		return c
	}
	return c.Context.Value(key)
}

func TestSlotClaimPreservesIndependentLifetimesAndMetadata(t *testing.T) {
	for _, cancelable := range []bool{false, true} {
		name := "background"
		if cancelable {
			name = "cancelable"
		}
		t.Run(name, func(t *testing.T) {
			parent := context.Background()
			if cancelable {
				var cancel context.CancelFunc
				parent, cancel = context.WithCancel(parent)
				defer cancel()
			}
			request := &requestContext{Context: WithTrace(parent, Trace{TraceID: "trace", SpanID: "upstream"})}
			firstTrace := Trace{TraceID: "trace", SpanID: "first", ParentSpanID: "upstream"}
			first := Start(request, firstTrace, time.Minute)
			if cancelable {
				if _, ok := first.(*timerFrame); !ok || request.slot.claimed.Load() {
					t.Fatal("cancelable invocation consumed incompatible lazy storage")
				}
			} else if first != &request.slot.frame {
				t.Fatal("first invocation did not claim request-owned frame")
			}
			childTrace := Trace{TraceID: "trace", SpanID: "child", ParentSpanID: "first"}
			child := Start(first, childTrace, time.Minute)
			child.Cancel()
			if first.Err() != nil || context.Cause(first) != nil || child.Err() != context.Canceled || context.Cause(child) != context.Canceled {
				t.Fatal("child cancellation changed parent lifetime")
			}
			first.Cancel()
			laterTrace := Trace{TraceID: "trace", SpanID: "later", ParentSpanID: "upstream"}
			later := Start(request, laterTrace, time.Minute)
			defer later.Cancel()
			if first == later || child == later || later.Err() != nil || request.Err() != nil {
				t.Fatal("later invocation reused an existing frame or lifetime")
			}
			if TraceFrom(first) != firstTrace || TraceFrom(child) != childTrace || TraceFrom(later) != laterTrace || RequestState(first) != request {
				t.Fatal("later invocation changed retained metadata")
			}
			if first.Err() != context.Canceled || context.Cause(first) != context.Canceled {
				t.Fatal("retained frame lost cancellation")
			}
		})
	}
}

func TestSlotConcurrentClaimIsUnique(t *testing.T) {
	t.Parallel()
	const count = 64
	request := &requestContext{Context: context.Background()}
	frames := make(chan Context, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			frame := Start(request, Trace{SpanID: fmt.Sprint(i)}, time.Minute)
			frame.Cancel()
			frames <- frame
		}()
	}
	close(start)
	wg.Wait()
	close(frames)
	seenFrames := make(map[Context]bool, count)
	seenSpans := make(map[string]bool, count)
	for frame := range frames {
		span := TraceFrom(frame).SpanID
		if seenFrames[frame] || seenSpans[span] || frame.Err() != context.Canceled || context.Cause(frame) != context.Canceled {
			t.Fatal("concurrent invocations shared storage or lost cancellation")
		}
		seenFrames[frame], seenSpans[span] = true, true
	}
	if len(seenFrames) != count || !seenFrames[&request.slot.frame] {
		t.Fatal("request-owned frame was not claimed exactly once")
	}
}

func TestTimerSlotPreservesStandardCancellationAndRetainedTrace(t *testing.T) {
	for _, parentFirst := range []bool{true, false} {
		name := "invocation first"
		if parentFirst {
			name = "parent first"
		}
		t.Run(name, func(t *testing.T) {
			parent, stopParent := context.WithCancelCause(context.Background())
			defer stopParent(nil)
			var slot TimerSlot
			request := &requestContext{Context: parent, reservation: &slot}
			firstTrace := Trace{TraceID: "trace", SpanID: "first"}
			first := Start(request, firstTrace, time.Minute)
			if first != &slot.frame {
				t.Fatal("cancelable invocation did not claim compact timer storage")
			}
			child, stopChild := context.WithCancel(first)
			defer stopChild()
			later := Start(request, Trace{SpanID: "later"}, time.Minute)
			defer later.Cancel()
			if first == later || TraceFrom(first) != firstTrace || RequestState(first) != request {
				t.Fatal("additional invocation changed retained storage or metadata")
			}
			cause := errors.New("parent stopped")
			wantCause := error(context.Canceled)
			if parentFirst {
				stopParent(cause)
				first.Cancel()
				wantCause = cause
			} else {
				first.Cancel()
				if request.Err() != nil || later.Err() != nil {
					t.Fatal("invocation completion canceled independent contexts")
				}
				stopParent(cause)
			}
			if first.Err() != context.Canceled || context.Cause(first) != wantCause || context.Cause(child) != wantCause {
				t.Fatalf("first/child cancellation = %v/%v, want %v", context.Cause(first), context.Cause(child), wantCause)
			}
			select {
			case <-first.Done():
			default:
				t.Fatal("retained timer frame remained active")
			}
		})
	}
}

func TestTimerSlotFallsBackForNoncancelableParent(t *testing.T) {
	type valueKey struct{}
	var slot TimerSlot
	request := &requestContext{Context: context.WithValue(context.Background(), valueKey{}, "value"), reservation: &slot}
	first := Start(request, Trace{SpanID: "first"}, time.Minute)
	first.Cancel()
	if _, ok := first.(*Frame); !ok || slot.claimed.Load() {
		t.Fatal("noncancelable invocation consumed incompatible timer storage")
	}
	parent, stopParent := context.WithCancel(request)
	defer stopParent()
	later := Start(parent, Trace{SpanID: "later"}, time.Minute)
	defer later.Cancel()
	if later != &slot.frame || TraceFrom(first).SpanID != "first" || first.Err() != context.Canceled || later.Value(valueKey{}) != "value" {
		t.Fatal("changed parent lifetime lost independent storage or metadata")
	}
}

func TestLazySlotFallsBackWithoutLosingParentCauseOrLaterClaim(t *testing.T) {
	parent, stopParent := context.WithCancelCause(context.Background())
	defer stopParent(nil)
	request := &requestContext{Context: parent}
	firstTrace := Trace{TraceID: "trace", SpanID: "first"}
	first := Start(request, firstTrace, time.Minute)
	if first == &request.slot.frame || request.slot.claimed.Load() {
		t.Fatal("cancelable invocation consumed lazy storage")
	}
	cause := errors.New("parent stopped")
	stopParent(cause)
	first.Cancel()
	if first.Err() != context.Canceled || context.Cause(first) != cause {
		t.Fatal("fallback timer lost the parent's cancellation cause")
	}
	// Middleware may later detach cancellation while preserving request values.
	// The previously incompatible reservation must still be usable exactly once.
	later := Start(context.WithoutCancel(request), Trace{SpanID: "later"}, time.Minute)
	if later != &request.slot.frame || later.Err() != nil || context.Cause(later) != nil {
		t.Fatal("later noncancelable invocation could not claim independent storage")
	}
	later.Cancel()
	if context.Cause(later) != context.Canceled || context.Cause(first) != cause || TraceFrom(first) != firstTrace {
		t.Fatal("later claim changed the retained timer frame")
	}
}

func TestFrameUnknownValuesPreserveOwnCancellationCause(t *testing.T) {
	type valueKey struct{}
	parent, cancelParent := context.WithCancelCause(context.WithValue(context.Background(), valueKey{}, "value"))
	cause := errors.New("parent stopped")
	request := &requestContext{Context: parent}
	frame := Start(request, Trace{SpanID: "span"}, time.Minute)
	defer frame.Cancel()
	if frame.Value(valueKey{}) != "value" || context.Cause(frame) != nil {
		t.Fatal("frame lost parent value or became canceled")
	}
	cancelParent(cause)
	if frame.Err() != context.Canceled || context.Cause(frame) != cause {
		t.Fatalf("parent cancellation = %v/%v", frame.Err(), context.Cause(frame))
	}
	frame.Cancel()
	if context.Cause(frame) != cause || TraceFrom(frame).SpanID != "span" {
		t.Fatal("frame cancellation changed first cause or trace")
	}
}

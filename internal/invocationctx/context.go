// Package invocationctx keeps immutable invocation metadata and lifetime in a
// single, permanently owned frame. An HTTP request may reserve one frame in its
// own allocation; additional invocations allocate independent frames.
package invocationctx

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/mujhtech/trest/internal/correlation"
	"github.com/mujhtech/trest/internal/lazycontext"
	"github.com/mujhtech/trest/internal/spanreserve"
)

// Trace is the immutable trace metadata shared with operation's public type.
type Trace struct {
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
}

type traceKey struct{}
type principalKey struct{}
type requestStateKey struct{}
type slotKey struct{}

type traceContext struct {
	context.Context
	trace Trace
}

func (c *traceContext) Value(key any) any {
	if IsTraceKey(key) {
		return c
	}
	return c.Context.Value(key)
}

// WithTrace returns an immutable trace wrapper. The private pointer exposed to
// Value avoids a separate interface box and cannot mutate the public metadata.
func WithTrace(parent context.Context, trace Trace) context.Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	return &traceContext{Context: parent, trace: trace}
}

// TraceFrom returns a copy of the current trace, or its zero value.
func TraceFrom(ctx context.Context) Trace {
	value, _ := ctx.Value(traceKey{}).(*traceContext)
	if value == nil {
		return Trace{}
	}
	return value.trace
}

// IsTraceKey recognizes only this package's private trace metadata key.
func IsTraceKey(key any) bool { _, ok := key.(traceKey); return ok }

// IsPrincipalKey recognizes the private trusted-identity metadata key.
func IsPrincipalKey(key any) bool { _, ok := key.(principalKey); return ok }

// PrincipalValue retrieves operation's immutable principal wrapper.
func PrincipalValue(ctx context.Context) any { return ctx.Value(principalKey{}) }

// IsRequestStateKey recognizes the private HTTP response-state key.
func IsRequestStateKey(key any) bool { _, ok := key.(requestStateKey); return ok }

// RequestState retrieves the HTTP-owned response state, when present.
func RequestState(ctx context.Context) any { return ctx.Value(requestStateKey{}) }

// IsSlotKey recognizes the private request-owned frame reservation key.
func IsSlotKey(key any) bool { _, ok := key.(slotKey); return ok }

// Slot reserves storage for exactly one invocation. Its zero value is ready for
// use. Do not copy, reset, or pool it: handlers may retain the claimed context.
// Concurrent and later invocations receive fresh frames instead of sharing it.
// A cancelable parent uses a fresh timer frame without consuming this lazy slot.
type Slot struct {
	claimed atomic.Bool
	frame   Frame
}

// TimerSlot reserves only the metadata needed by a cancelable invocation. Its
// standard-library timer owns the lifetime, so no lazy deadline state is stored.
// It has the same one-use ownership rule as Slot. If middleware replaces the
// parent with a noncancelable context, Start allocates an independent lazy frame.
type TimerSlot struct {
	claimed atomic.Bool
	frame   timerFrame
}

// Context is an invocation-owned context with an explicit completion action.
type Context interface {
	context.Context
	Cancel()
}

type timerFrame struct {
	traceContext
	cancel context.CancelFunc
}

func (c *timerFrame) init(parent context.Context, trace Trace, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	c.traceContext = traceContext{Context: ctx, trace: trace}
	c.cancel = cancel
}

func (c *timerFrame) Cancel() { c.cancel() }

// Frame combines invocation metadata with its independent timeout. It is never
// reused, including after cancellation. Known framework values bypass lazy
// timer construction; every other key delegates to the lifetime so Cause and
// derived contexts always receive standard-library cancellation metadata.
type Frame struct {
	lifetime lazycontext.Lazy
	metadata traceContext
}

// Start claims request-owned storage when available, otherwise allocating a
// fresh frame. Call only after invocation admission; consuming trace IDs and
// checking cancellation or capacity do not claim the request's frame.
func Start(parent context.Context, trace Trace, timeout time.Duration) Context {
	// Probe the lifetime only after admission, where invocation construction
	// already needs it. HTTP storage selection must not call parent methods
	// before authentication, binding, or the request's panic recovery boundary.
	slot := parent.Value(slotKey{})
	_, deadline := parent.Deadline()
	if deadline || parent.Done() != nil {
		var frame *timerFrame
		if reserved, ok := slot.(*TimerSlot); ok && reserved != nil && reserved.claimed.CompareAndSwap(false, true) {
			frame = &reserved.frame
		} else {
			frame = new(timerFrame)
		}
		frame.init(parent, trace, timeout)
		return frame
	}
	var frame *Frame
	if reserved, ok := slot.(*Slot); ok && reserved != nil && reserved.claimed.CompareAndSwap(false, true) {
		frame = &reserved.frame
	} else {
		frame = new(Frame)
	}
	frame.init(parent, trace, timeout)
	return frame
}

func (c *Frame) init(parent context.Context, trace Trace, timeout time.Duration) {
	c.metadata = traceContext{Context: parent, trace: trace}
	c.lifetime.Init(parent, timeout)
}

func (c *Frame) Deadline() (time.Time, bool) { return c.lifetime.Deadline() }
func (c *Frame) Done() <-chan struct{}       { return c.lifetime.Done() }
func (c *Frame) Err() error                  { return c.lifetime.Err() }

func (c *Frame) Value(key any) any {
	if IsTraceKey(key) {
		return &c.metadata
	}
	if IsPrincipalKey(key) || IsSlotKey(key) || IsRequestStateKey(key) || correlation.IsKey(key) || spanreserve.IsKey(key) {
		return c.metadata.Context.Value(key)
	}
	return c.lifetime.Value(key)
}

// Cancel ends this invocation without canceling its parent or reusing storage.
func (c *Frame) Cancel() { c.lifetime.Cancel() }

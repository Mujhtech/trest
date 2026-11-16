// Package lazycontext defers timer construction for invocations whose parents
// cannot be canceled. Contexts with a cancelable parent use the standard library
// immediately so parent cancellation and its cause retain their original order.
package lazycontext

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const (
	active uint32 = iota
	canceled
	expired
)

type timerContext struct {
	context.Context
	cancel context.CancelFunc
}

// Context selects a cooperative lazy deadline or a standard-library timeout
// according to its parent's lifetime. Init must be called exactly once before
// use; afterward all methods are safe for concurrent use. Never copy or reuse it.
// Callers that already checked the parent can embed Lazy directly instead.
type Context struct {
	lazy   Lazy
	eager  context.Context
	cancel context.CancelFunc
}

// Init starts a timeout relative to now. It panics for a nil parent or reuse.
// A parent with a Done channel or deadline takes the standard-library path.
func (c *Context) Init(parent context.Context, timeout time.Duration) {
	c.lazy.Init(parent, timeout)
	_, hasDeadline := parent.Deadline()
	if parent.Done() != nil || hasDeadline {
		c.eager, c.cancel = context.WithDeadline(parent, c.lazy.deadline)
		c.lazy.deadline, _ = c.eager.Deadline()
	}
}

func (c *Context) Deadline() (time.Time, bool) { return c.lazy.Deadline() }

func (c *Context) Done() <-chan struct{} {
	if c.eager != nil {
		return c.eager.Done()
	}
	return c.lazy.Done()
}

func (c *Context) Err() error {
	if c.eager != nil {
		return c.eager.Err()
	}
	return c.lazy.Err()
}

func (c *Context) Value(key any) any {
	if c.eager != nil {
		return c.eager.Value(key)
	}
	return c.lazy.Value(key)
}

func (c *Context) Cancel() {
	if c.eager != nil {
		c.cancel()
		return
	}
	c.lazy.Cancel()
}

// Lazy is a cooperative invocation deadline for a parent already known to have
// neither a Done channel nor a deadline. Init does not repeat those probes.
// Initialize it exactly once before use; afterward all methods are safe for
// concurrent use. Never copy, recycle, or reuse it, even after cancellation.
//
// Done and Value install a standard-library deadline context on first use.
// Value deliberately delegates every key through that context: even private
// cancellation keys used by context.Cause and derived contexts must retain their
// standard-library meaning. An outer wrapper may handle its own known metadata
// keys before calling Value, but must delegate all other keys here.
type Lazy struct {
	parent   context.Context
	deadline time.Time

	mu    sync.Mutex
	state atomic.Uint32
	timer atomic.Pointer[timerContext]
}

// Init starts a timeout relative to now. It panics for a nil parent or reuse.
// The caller must have checked that the parent has no Done channel or deadline.
func (c *Lazy) Init(parent context.Context, timeout time.Duration) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if c.parent != nil {
		panic("cannot reuse an invocation context")
	}
	c.parent = parent
	c.deadline = time.Now().Add(timeout)
}

// Deadline returns the invocation deadline.
func (c *Lazy) Deadline() (time.Time, bool) {
	return c.deadline, true
}

// Done returns one stable channel that closes on cancellation or expiry.
func (c *Lazy) Done() <-chan struct{} {
	return c.materialize().Done()
}

// Err checks an unobserved deadline without creating a timer. Once Done or Value
// has exposed the standard-library context, its cancellation result is final.
func (c *Lazy) Err() error {
	if timer := c.timer.Load(); timer != nil {
		return timer.Err()
	}
	state := c.state.Load()
	if state == active && time.Until(c.deadline) <= 0 {
		c.mu.Lock()
		// A concurrent Done/Value call may have exposed a real timer. Its
		// result then owns the observable ordering with explicit cancellation.
		if timer := c.timer.Load(); timer != nil {
			c.mu.Unlock()
			return timer.Err()
		}
		if c.state.Load() == active {
			c.state.Store(expired)
		}
		state = c.state.Load()
		c.mu.Unlock()
	}
	switch state {
	case canceled:
		return context.Canceled
	case expired:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// Value preserves all parent values and standard-library cancellation metadata.
func (c *Lazy) Value(key any) any {
	return c.materialize().Value(key)
}

// Cancel releases resources and permanently cancels the invocation. If its
// unobserved deadline has already passed, expiry wins over this later cancel.
// A retained context remains valid after cancellation and is never recycled.
func (c *Lazy) Cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timer := c.timer.Load(); timer != nil {
		timer.cancel()
		return
	}
	if c.state.Load() == active {
		state := canceled
		if time.Until(c.deadline) <= 0 {
			state = expired
		}
		c.state.Store(state)
	}
}

func (c *Lazy) materialize() context.Context {
	if timer := c.timer.Load(); timer != nil {
		return timer.Context
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if timer := c.timer.Load(); timer != nil {
		return timer.Context
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if c.state.Load() == canceled {
		// Installing an already canceled child instead of an elapsed deadline
		// keeps a successful invocation's cancellation cause stable forever.
		ctx, cancel = context.WithCancel(c.parent)
		cancel()
	} else {
		ctx, cancel = context.WithDeadline(c.parent, c.deadline)
	}
	c.timer.Store(&timerContext{Context: ctx, cancel: cancel})
	return ctx
}

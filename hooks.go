package trest

import (
	"context"
	"errors"
)

// Resolve derives a typed request-local value from validated input. Resolution
// runs before resource authorization and the handler, including during JSON
// preparation and invocation. It must not perform privileged side effects.
// Authorization must still approve the resolved resource before it is exposed.
// The supplied key makes dependencies explicit and avoids untyped context maps.
// Calls return independent route copies and run in declaration order.
func Resolve[I, O, T any](route *Route[I, O], key Key[T], fn func(context.Context, I) (T, error)) *Route[I, O] {
	if route == nil {
		return nil
	}
	c := *route
	if key.identity == nil || fn == nil {
		c.configError = errors.New("trest: resolver requires a key from NewKey and a function")
		return &c
	}
	c.resolvers = append(append([]func(context.Context, I) (context.Context, error){}, route.resolvers...), func(ctx context.Context, in I) (context.Context, error) {
		value, err := fn(ctx, in)
		if err != nil {
			return nil, err
		}
		return WithValue(ctx, key, value), nil
	})
	return &c
}

// After transforms a successful typed result before output validation or HTTP
// delivery. Hooks run in declaration order and apply to every invocation path.
// A hook error does not undo handler effects; job/tool adapters retain their
// existing uncertain-outcome handling. Use AfterResponse to observe delivery.
func (r *Route[I, O]) After(fn func(context.Context, I, O) (O, error)) *Route[I, O] {
	c := *r
	if fn == nil {
		c.configError = errors.New("trest: nil result hook")
		return &c
	}
	c.after = append(append([]func(context.Context, I, O) (O, error){}, r.after...), fn)
	return &c
}

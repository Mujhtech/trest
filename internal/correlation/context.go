// Package correlation shares immutable transport metadata between the HTTP and
// operation layers without allocating a separate context wrapper at each layer.
package correlation

import "context"

type key struct{}

// Context carries an immutable correlation ID and forwards the parent's
// lifetime and application values. It may be embedded in request-owned state;
// it must not be changed or reused once that state has been published.
type Context struct {
	context.Context
	id string
}

// New creates metadata suitable for embedding without allocating a wrapper.
func New(parent context.Context, id string) Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	return Context{Context: parent, id: id}
}

// Value returns the private metadata wrapper, so callers cannot mutate its ID.
func (c *Context) Value(k any) any {
	if IsKey(k) {
		return c
	}
	return c.Context.Value(k)
}

// ID returns the nearest correlation ID, including an explicitly empty one.
func ID(ctx context.Context) string {
	value, _ := ctx.Value(key{}).(*Context)
	if value == nil {
		return ""
	}
	return value.id
}

// IsKey lets an invocation retain transport metadata when a resolver supplies
// unrelated application values. The key itself remains private to this package.
func IsKey(k any) bool {
	_, ok := k.(key)
	return ok
}

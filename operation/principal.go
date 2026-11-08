package operation

import "context"

// Principal is identity established by trusted application authentication.
// Business input must never be used to manufacture a principal.
type Principal struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id,omitempty"`
}

type principalKey struct{}

// Keep the immutable identity in its context wrapper rather than allocating a
// separate interface box. Value exposes only the private wrapper; the public
// accessor returns a copy that cannot change later authorization decisions.
type principalContext struct {
	context.Context
	principal Principal
}

func (c *principalContext) Value(key any) any {
	if _, ok := key.(principalKey); ok {
		return c
	}
	return c.Context.Value(key)
}

// WithPrincipal attaches trusted identity. It does not verify credentials.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	if ctx == nil {
		panic("cannot create context from nil parent")
	}
	return &principalContext{Context: ctx, principal: p}
}

// PrincipalFromContext returns trusted identity, if present and nonempty.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	value, _ := ctx.Value(principalKey{}).(*principalContext)
	if value == nil {
		return Principal{}, false
	}
	p := value.principal
	return p, p.ID != ""
}

// Access requires an explicit choice between public and permission-based access.
type Access struct {
	Mode       string `json:"mode"`
	Permission string `json:"permission,omitempty"`
}

// Public declares anonymous access to an operation.
func Public() Access { return Access{Mode: "public"} }

// Permission declares a permission the authorizer must enforce for every call.
func Permission(name string) Access { return Access{Mode: "permission", Permission: name} }

// Authorizer checks operation and resource access using trusted identity and typed input.
// A nil error grants access. Applications must implement tenant/resource checks here
// or in their domain service; a permission name alone grants nothing.
type Authorizer interface {
	Authorize(context.Context, Principal, string, any) error
}

// AuthorizerFunc adapts a function to Authorizer.
type AuthorizerFunc func(context.Context, Principal, string, any) error

func (f AuthorizerFunc) Authorize(ctx context.Context, p Principal, permission string, input any) error {
	return f(ctx, p, permission, input)
}

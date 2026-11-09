package trest

import "trest.local/trest/operation"

// Error is a classified error with a public message and optional private cause.
type Error = operation.Error

// ErrorCode is a stable error category used by HTTP, jobs, and tools.
type ErrorCode = operation.Code

// FieldError identifies an invalid input field by JSON Pointer.
type FieldError = operation.FieldError

// Authorizer checks permission and resource access using trusted identity.
type Authorizer = operation.Authorizer

// AuthorizerFunc adapts a function into a resource authorization policy.
type AuthorizerFunc = operation.AuthorizerFunc

const (
	InvalidInput     = operation.InvalidInput
	Unauthenticated  = operation.Unauthenticated
	Forbidden        = operation.Forbidden
	NotFound         = operation.NotFound
	Conflict         = operation.Conflict
	RateLimited      = operation.RateLimited
	Unavailable      = operation.Unavailable
	Internal         = operation.Internal
	DeadlineExceeded = operation.DeadlineExceeded
	Canceled         = operation.Canceled
)

// NewError classifies a failure. Only message may be sent to clients; cause stays private.
func NewError(code ErrorCode, message string, cause error) *Error {
	return operation.NewError(code, message, cause)
}

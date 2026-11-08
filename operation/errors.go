package operation

import (
	"context"
	"errors"
	"fmt"
)

// Code is a stable, transport-independent error category.
type Code string

const (
	InvalidInput     Code = "invalid_input"
	Unauthenticated  Code = "unauthenticated"
	Forbidden        Code = "forbidden"
	NotFound         Code = "not_found"
	Conflict         Code = "conflict"
	RateLimited      Code = "rate_limited"
	Unavailable      Code = "unavailable"
	Internal         Code = "internal"
	DeadlineExceeded Code = "deadline_exceeded"
	Canceled         Code = "canceled"
)

// FieldError describes an input problem at a JSON Pointer path.
type FieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error holds an approved public message and an optional private cause.
// Only put information safe to disclose to callers in Message and Fields.
type Error struct {
	Code    Code         `json:"code"`
	Message string       `json:"message"`
	Fields  []FieldError `json:"fields,omitempty"`
	Cause   error        `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap preserves the original cause for errors.Is and errors.As.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is compares error categories. Use errors.Is(err, &Error{Code: Forbidden}).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && e != nil && t != nil && e.Code == t.Code
}

// NewError marks message as safe for public disclosure; cause remains private.
func NewError(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

// CodeOf classifies wrapped operation and context errors. Unknown errors are internal.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) && e != nil {
		switch e.Code {
		case InvalidInput, Unauthenticated, Forbidden, NotFound, Conflict, RateLimited, Unavailable, Internal, DeadlineExceeded, Canceled:
			return e.Code
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return Canceled
	}
	return Internal
}

// PublicError returns a detached, redacted error suitable for a transport.
// Internal errors never disclose their original message or field details.
func PublicError(err error) *Error {
	if err == nil {
		return nil
	}
	code := CodeOf(err)
	messages := map[Code]string{
		InvalidInput: "invalid input", Unauthenticated: "authentication required", Forbidden: "access denied",
		NotFound: "resource not found", Conflict: "resource conflict", RateLimited: "too many requests",
		Unavailable: "service unavailable", Internal: "internal error", DeadlineExceeded: "deadline exceeded", Canceled: "request canceled",
	}
	result := &Error{Code: code, Message: messages[code]}
	var e *Error
	if code != Internal && errors.As(err, &e) && e != nil && e.Code == code {
		if e.Message != "" {
			result.Message = e.Message
		}
		result.Fields = append([]FieldError(nil), e.Fields...)
	}
	return result
}

// Diagnostic is a startup error identifying a concrete registration problem.
type Diagnostic struct {
	Code        string `json:"code"`
	OperationID string `json:"operation_id,omitempty"`
	Path        string `json:"path,omitempty"`
	Message     string `json:"message"`
}

func (d *Diagnostic) Error() string {
	return fmt.Sprintf("operation: %s %s %s: %s", d.Code, d.OperationID, d.Path, d.Message)
}

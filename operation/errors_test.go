package operation_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"trest.local/trest/operation"
)

func TestErrorClassificationAndRedaction(t *testing.T) {
	for _, tt := range []struct {
		name    string
		err     error
		code    operation.Code
		message string
	}{
		{"unknown", errors.New("password=secret"), operation.Internal, "internal error"},
		{"internal", operation.NewError(operation.Internal, "secret", nil), operation.Internal, "internal error"},
		{"invalid code", operation.NewError("unknown", "secret", nil), operation.Internal, "internal error"},
		{"approved message", operation.NewError(operation.Conflict, "already exists", errors.New("secret")), operation.Conflict, "already exists"},
		{"context timeout", context.DeadlineExceeded, operation.DeadlineExceeded, "deadline exceeded"},
		{"context canceled", context.Canceled, operation.Canceled, "request canceled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := fmt.Errorf("private wrapper: %w", tt.err)
			if operation.CodeOf(wrapped) != tt.code {
				t.Fatalf("classification %s", operation.CodeOf(wrapped))
			}
			p := operation.PublicError(wrapped)
			if p.Code != tt.code || p.Message != tt.message || p.Cause != nil {
				t.Fatalf("public error %+v", p)
			}
		})
	}
}

func TestErrorPreservesCauseAndCopiesFields(t *testing.T) {
	cause := errors.New("db failure")
	e := operation.NewError(operation.InvalidInput, "bad input", cause)
	e.Fields = []operation.FieldError{{Path: "/name", Code: "required", Message: "name required"}}
	wrapped := fmt.Errorf("boundary: %w", e)
	if !errors.Is(wrapped, cause) || !errors.Is(wrapped, &operation.Error{Code: operation.InvalidInput}) {
		t.Fatal("errors.Is did not find cause/category")
	}
	var typed *operation.Error
	if !errors.As(wrapped, &typed) || typed != e {
		t.Fatal("errors.As failed")
	}
	p := operation.PublicError(wrapped)
	p.Fields[0].Path = "changed"
	if e.Fields[0].Path != "/name" {
		t.Fatal("field details share storage")
	}
}

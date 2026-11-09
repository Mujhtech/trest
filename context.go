package trest

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"trest.local/trest/operation"
)

// RequestID returns the server-generated correlation ID shared with invocations.
func RequestID(ctx context.Context) string { return operation.CorrelationID(ctx) }

// SetHeader stages an HTTP response header while a typed handler is running.
// It fails outside an HTTP request or after the response has started. Request
// context values should be used on the request goroutine, not after it returns.
func SetHeader(ctx context.Context, name, value string) error {
	state, _ := ctx.Value(requestStateKey{}).(*requestState)
	if state == nil || state.writer.ResponseWriter == nil {
		return errors.New("trest: HTTP response unavailable")
	}
	if state.writer.status != 0 {
		return errors.New("trest: response already started")
	}
	if !validHeaderName(name) || strings.ContainsAny(value, "\r\n") {
		return errors.New("trest: invalid response header")
	}
	state.writer.Header().Set(name, value)
	return nil
}

// SetCookie stages a cookie on the HTTP response without changing a typed JSON
// return value. Supply explicit Secure/HttpOnly/SameSite policy, or use the signed
// cookie plugin. It fails outside HTTP and never writes to a tool/job result.
func SetCookie(ctx context.Context, cookie *http.Cookie) error {
	if cookie == nil {
		return errors.New("trest: nil cookie")
	}
	if err := cookie.Valid(); err != nil {
		return err
	}
	state, _ := ctx.Value(requestStateKey{}).(*requestState)
	if state == nil || state.writer.ResponseWriter == nil {
		return errors.New("trest: HTTP response unavailable")
	}
	if state.writer.status != 0 {
		return errors.New("trest: response already started")
	}
	http.SetCookie(&state.writer, cookie)
	return nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	return true
}

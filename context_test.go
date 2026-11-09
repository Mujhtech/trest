package trest_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"trest.local/trest"
	"trest.local/trest/operation"
)

type namedText string

func (namedText) String() string { return "different private representation" }

func TestNamedTextUsesValidatedValue(t *testing.T) {
	t.Parallel()
	app, err := trest.New(trest.Get("/", trest.Const(namedText("public value"))))
	if err != nil {
		t.Fatal(err)
	}
	if w := request(t, app, "GET", "/", "", ""); w.Body.String() != "public value" {
		t.Fatalf("stringer changed wire value: %s", w.Body)
	}
}

func TestTypedHandlerResponseHeadersAndCookies(t *testing.T) {
	t.Parallel()
	type result struct {
		ID string `json:"request_id"`
	}
	app, err := trest.New(trest.Get("/", func(ctx context.Context, _ trest.Empty) (result, error) {
		if err := trest.SetHeader(ctx, "X-Example", "yes"); err != nil {
			return result{}, err
		}
		if err := trest.SetCookie(ctx, &http.Cookie{Name: "session", Value: "example", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}); err != nil {
			return result{}, err
		}
		return result{ID: trest.RequestID(ctx)}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, app, "GET", "/", "", "")
	if w.Code != 200 || w.Header().Get("X-Example") != "yes" || w.Header().Get("X-Request-ID") == "" {
		t.Fatalf("response: %d %v", w.Code, w.Header())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatal(cookies)
	}
	if err := trest.SetHeader(context.Background(), "X-Test", "value"); err == nil {
		t.Fatal("HTTP header accepted outside request")
	}
	if err := trest.SetCookie(context.Background(), &http.Cookie{Name: "a", Value: "b"}); err == nil {
		t.Fatal("cookie accepted outside request")
	}
}

func TestRequestContextPreservesCancellationAndMetadata(t *testing.T) {
	t.Parallel()
	type applicationKey struct{}
	parent, cancel := context.WithCancelCause(context.WithValue(context.Background(), applicationKey{}, "retained"))
	defer cancel(nil)
	entered := make(chan context.Context, 1)
	app, err := trest.New(trest.Get("/", func(ctx context.Context, _ trest.Empty) (string, error) {
		entered <- ctx
		<-ctx.Done()
		return "", ctx.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil).WithContext(parent)
	done := make(chan struct{})
	go func() { app.ServeHTTP(w, r); close(done) }()
	var handlerContext context.Context
	select {
	case handlerContext = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	if handlerContext.Value(applicationKey{}) != "retained" || trest.RequestID(handlerContext) == "" || operation.TraceFromContext(handlerContext).SpanID == "" {
		t.Error("request context lost metadata")
	}
	if _, ok := handlerContext.Deadline(); !ok {
		t.Error("request context lost invocation deadline")
	}
	cause := errors.New("caller canceled")
	cancel(cause)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parent cancellation did not reach handler")
	}
	if w.Code != http.StatusRequestTimeout || !errors.Is(context.Cause(handlerContext), cause) {
		t.Fatalf("status=%d cause=%v", w.Code, context.Cause(handlerContext))
	}
	if trest.RequestID(handlerContext) != w.Header().Get("X-Request-ID") {
		t.Fatal("request and handler correlation IDs differ")
	}
}

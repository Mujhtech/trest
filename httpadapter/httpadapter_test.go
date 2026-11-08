package httpadapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"trest.local/trest/httpadapter"
	"trest.local/trest/operation"
)

type greetingInput struct {
	Name string `json:"name"`
}

type greetingOutput struct {
	Greeting string `json:"greeting"`
	Tenant   string `json:"tenant"`
}

func setup[I, O any](t *testing.T, definition operation.Definition[I, O], handler func(context.Context, I) (O, error), authorizer operation.Authorizer, opts httpadapter.Options, route httpadapter.Route) (*http.ServeMux, *httpadapter.Adapter, *operation.Handle[I, O]) {
	t.Helper()
	r := operation.NewRegistry(authorizer)
	h, err := operation.Register(r, definition, handler)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a, err := httpadapter.New(mux, r, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := httpadapter.Bind(a, h, route); err != nil {
		t.Fatal(err)
	}
	return mux, a, h
}

func publicGreeting() operation.Definition[greetingInput, greetingOutput] {
	return operation.Definition[greetingInput, greetingOutput]{ID: "greeting.create.v1", Summary: "Create greeting", Access: operation.Public()}
}

func decodeError(t *testing.T, response *httptest.ResponseRecorder) httpadapter.ErrorResponse {
	t.Helper()
	var result httpadapter.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid error response %q: %v", response.Body.String(), err)
	}
	if result.RequestID == "" || result.RequestID != response.Header().Get("X-Request-ID") {
		t.Fatalf("missing or mismatched request id: %#v", result)
	}
	if response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("unexpected content type: %s", response.Header().Get("Content-Type"))
	}
	return result
}

func jsonRequest(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestNew(t *testing.T) {
	t.Parallel()
	frozen := operation.NewRegistry(nil)
	if err := frozen.Freeze(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mux  *http.ServeMux
		reg  *operation.Registry
		opts httpadapter.Options
	}{
		{name: "nil mux", reg: frozen},
		{name: "nil registry", mux: http.NewServeMux()},
		{name: "unfrozen registry", mux: http.NewServeMux(), reg: operation.NewRegistry(nil)},
		{name: "negative body bound", mux: http.NewServeMux(), reg: frozen, opts: httpadapter.Options{MaxBodyBytes: -1}},
		{name: "too small output bound", mux: http.NewServeMux(), reg: frozen, opts: httpadapter.Options{MaxOutputBytes: 255}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := httpadapter.New(tc.mux, tc.reg, tc.opts); err == nil {
				t.Fatal("expected construction error")
			}
		})
	}
}

func TestBindEnforcesAuthorizationAndTrustedTenant(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	var authCalls atomic.Int64
	definition := publicGreeting()
	definition.Access = operation.Permission("greetings:write")
	authorizer := operation.AuthorizerFunc(func(ctx context.Context, principal operation.Principal, permission string, in any) error {
		authCalls.Add(1)
		if permission != "greetings:write" || in.(greetingInput).Name != "Ada" {
			t.Errorf("unexpected authorization input: %q %#v", permission, in)
		}
		if principal.TenantID != "tenant-a" {
			return errors.New("private policy service secret")
		}
		return nil
	})
	authenticate := func(r *http.Request) (operation.Principal, error) {
		switch r.Header.Get("Authorization") {
		case "Bearer valid-a":
			return operation.Principal{ID: "user-a", TenantID: "tenant-a"}, nil
		case "Bearer valid-b":
			return operation.Principal{ID: "user-b", TenantID: "tenant-b"}, nil
		case "":
			return operation.Principal{}, nil
		default:
			return operation.Principal{}, errors.New("provider secret: invalid token")
		}
	}
	mux, _, _ := setup(t, definition, func(ctx context.Context, in greetingInput) (greetingOutput, error) {
		calls.Add(1)
		principal, ok := operation.PrincipalFromContext(ctx)
		if !ok {
			t.Error("handler received no principal")
		}
		return greetingOutput{Greeting: "Hello " + in.Name, Tenant: principal.TenantID}, nil
	}, authorizer, httpadapter.Options{Authenticate: authenticate}, httpadapter.Route{Method: "POST", Path: "/greet", SuccessStatus: 201})

	for _, tc := range []struct {
		name, token string
		status      int
		code        operation.Code
	}{
		{name: "anonymous denied", status: 401, code: operation.Unauthenticated},
		{name: "invalid credentials", token: "Bearer wrong", status: 401, code: operation.Unauthenticated},
		{name: "other tenant denied", token: "Bearer valid-b", status: 403, code: operation.Forbidden},
		{name: "authorized", token: "Bearer valid-a", status: 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			r := jsonRequest("POST", "/greet", `{"name":"Ada"}`)
			r.Header.Set("Authorization", tc.token)
			r.Header.Set("X-Tenant-ID", "attacker")
			r.Header.Set("X-Request-ID", "attacker-controlled-id")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d; want %d; %s", w.Code, tc.status, w.Body.String())
			}
			if w.Header().Get("X-Request-ID") == "attacker-controlled-id" {
				t.Fatal("untrusted correlation id was accepted")
			}
			if tc.code != "" {
				result := decodeError(t, w)
				if result.Code != tc.code || strings.Contains(w.Body.String(), "secret") {
					t.Fatalf("unexpected public error: %s", w.Body.String())
				}
				if calls.Load() != before {
					t.Fatal("denied request invoked handler")
				}
			} else {
				var result greetingOutput
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Tenant != "tenant-a" || result.Greeting != "Hello Ada" || calls.Load() != before+1 {
					t.Fatalf("unexpected success: %#v", result)
				}
			}
		})
	}
	if authCalls.Load() != 2 {
		t.Fatalf("authorization ran %d times, want 2", authCalls.Load())
	}
}

func TestBindRejectsInvalidInputBeforeHandler(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	mux, _, _ := setup(t, publicGreeting(), func(context.Context, greetingInput) (greetingOutput, error) {
		calls.Add(1)
		return greetingOutput{}, nil
	}, nil, httpadapter.Options{MaxBodyBytes: 64}, httpadapter.Route{Method: "POST", Path: "/greet"})
	for _, tc := range []struct {
		name, body, media string
		status            int
	}{
		{name: "malformed", body: `{"name":`, media: "application/json", status: 400},
		{name: "empty", media: "application/json", status: 400},
		{name: "multiple documents", body: `{"name":"Ada"} {}`, media: "application/json", status: 400},
		{name: "missing required", body: `{}`, media: "application/json", status: 400},
		{name: "null", body: `null`, media: "application/json", status: 400},
		{name: "null field", body: `{"name":null}`, media: "application/json", status: 400},
		{name: "wrong scalar", body: `{"name":1}`, media: "application/json", status: 400},
		{name: "unknown field", body: `{"name":"Ada","tenant_id":"victim"}`, media: "application/json", status: 400},
		{name: "wrong field case", body: `{"Name":"Ada"}`, media: "application/json", status: 400},
		{name: "duplicate field", body: `{"name":"Ada","name":"Eve"}`, media: "application/json", status: 400},
		{name: "oversized", body: `{"name":"` + strings.Repeat("a", 64) + `"}`, media: "application/json", status: 413},
		{name: "missing media type", body: `{"name":"Ada"}`, status: 415},
		{name: "unsupported media", body: `{"name":"Ada"}`, media: "text/plain", status: 415},
		{name: "unsupported charset", body: `{"name":"Ada"}`, media: "application/json;charset=utf-16", status: 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := jsonRequest("POST", "/greet", tc.body)
			r.Header.Set("Content-Type", tc.media)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if e := decodeError(t, w); e.Code != operation.InvalidInput {
				t.Fatalf("unexpected error: %#v", e)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request invoked handler")
	}
}

func TestBindPreservesValidationFields(t *testing.T) {
	t.Parallel()
	definition := publicGreeting()
	definition.Validate = func(context.Context, greetingInput) error {
		return &operation.Error{Code: operation.InvalidInput, Message: "name is reserved", Fields: []operation.FieldError{{Path: "/name", Code: "reserved", Message: "choose another name"}}, Cause: errors.New("private validation rule")}
	}
	mux, _, _ := setup(t, definition, func(context.Context, greetingInput) (greetingOutput, error) {
		t.Fatal("invalid input invoked handler")
		return greetingOutput{}, nil
	}, nil, httpadapter.Options{}, httpadapter.Route{Method: "POST", Path: "/greet"})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, jsonRequest("POST", "/greet", `{"name":"Ada"}`))
	e := decodeError(t, w)
	if w.Code != 400 || len(e.Fields) != 1 || e.Fields[0].Path != "/name" || e.Fields[0].Code != "reserved" {
		t.Fatalf("unexpected validation error: %#v, status %d", e, w.Code)
	}
	if strings.Contains(w.Body.String(), "private") {
		t.Fatal("private cause leaked")
	}
}

func TestBindErrorMappingAndBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		code    operation.Code
		message string
	}{
		{name: "wrapped conflict", err: fmt.Errorf("private: %w", operation.NewError(operation.Conflict, "already exists", errors.New("secret"))), status: 409, code: operation.Conflict, message: "already exists"},
		{name: "not found", err: operation.NewError(operation.NotFound, "", nil), status: 404, code: operation.NotFound},
		{name: "rate limited", err: operation.NewError(operation.RateLimited, "", nil), status: 429, code: operation.RateLimited},
		{name: "unavailable", err: operation.NewError(operation.Unavailable, "", nil), status: 503, code: operation.Unavailable},
		{name: "unknown", err: errors.New("secret database credentials"), status: 500, code: operation.Internal, message: "internal error"},
		{name: "internal always redacted", err: operation.NewError(operation.Internal, "secret", nil), status: 500, code: operation.Internal, message: "internal error"},
		{name: "huge public error", err: operation.NewError(operation.Conflict, strings.Repeat("x", 300), nil), status: 500, code: operation.Internal, message: "internal error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux, _, _ := setup(t, publicGreeting(), func(context.Context, greetingInput) (greetingOutput, error) { return greetingOutput{}, tc.err }, nil, httpadapter.Options{MaxOutputBytes: 256}, httpadapter.Route{Method: "POST", Path: "/greet"})
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, jsonRequest("POST", "/greet", `{"name":"Ada"}`))
			e := decodeError(t, w)
			if w.Code != tc.status || e.Code != tc.code || (tc.message != "" && e.Message != tc.message) {
				t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
			}
			if w.Body.Len() > 256 || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("response leaked or exceeded bound: %s", w.Body.String())
			}
		})
	}
}

func TestBindOutputLimitAndPanic(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		handler func(context.Context, greetingInput) (greetingOutput, error)
	}{
		{name: "output too large", handler: func(context.Context, greetingInput) (greetingOutput, error) {
			return greetingOutput{Greeting: strings.Repeat("x", 300)}, nil
		}},
		{name: "handler panic", handler: func(context.Context, greetingInput) (greetingOutput, error) { panic("private panic payload") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux, _, _ := setup(t, publicGreeting(), tc.handler, nil, httpadapter.Options{MaxOutputBytes: 256}, httpadapter.Route{Method: "POST", Path: "/greet", SuccessStatus: 201})
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, jsonRequest("POST", "/greet", `{"name":"Ada"}`))
			e := decodeError(t, w)
			if w.Code != 500 || e.Code != operation.Internal || w.Body.Len() > 256 || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestBindCancellationAndDeadline(t *testing.T) {
	t.Parallel()
	t.Run("canceled before invocation", func(t *testing.T) {
		mux, _, _ := setup(t, publicGreeting(), func(context.Context, greetingInput) (greetingOutput, error) {
			t.Fatal("canceled request invoked handler")
			return greetingOutput{}, nil
		}, nil, httpadapter.Options{}, httpadapter.Route{Method: "POST", Path: "/greet"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, jsonRequest("POST", "/greet", `{"name":"Ada"}`).WithContext(ctx))
		if e := decodeError(t, w); w.Code != 408 || e.Code != operation.Canceled {
			t.Fatalf("unexpected response %d %#v", w.Code, e)
		}
	})
	t.Run("cooperative timeout", func(t *testing.T) {
		definition := publicGreeting()
		definition.Timeout = time.Millisecond
		mux, _, _ := setup(t, definition, func(ctx context.Context, in greetingInput) (greetingOutput, error) {
			<-ctx.Done()
			return greetingOutput{}, ctx.Err()
		}, nil, httpadapter.Options{}, httpadapter.Route{Method: "POST", Path: "/greet"})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, jsonRequest("POST", "/greet", `{"name":"Ada"}`))
		if e := decodeError(t, w); w.Code != 504 || e.Code != operation.DeadlineExceeded {
			t.Fatalf("unexpected response %d %#v", w.Code, e)
		}
	})
}

func TestBindExistingMuxAndReadOnlyRoutes(t *testing.T) {
	t.Parallel()
	mux, a, h := setup(t, operation.Definition[struct{}, greetingOutput]{ID: "greeting.read.v1", Summary: "Read greeting", Access: operation.Public()}, func(context.Context, struct{}) (greetingOutput, error) {
		return greetingOutput{Greeting: "hello"}, nil
	}, nil, httpadapter.Options{}, httpadapter.Route{Method: "GET", Path: "/"})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "healthy") })
	for _, tc := range []struct {
		name, method, target, body string
		status                     int
		contains                   string
	}{
		{name: "existing handler", method: "GET", target: "/healthz", status: 200, contains: "healthy"},
		{name: "operation", method: "GET", target: "/", status: 200, contains: "hello"},
		{name: "head", method: "HEAD", target: "/", status: 200},
		{name: "root exact match", method: "GET", target: "/unknown", status: 404},
		{name: "method mismatch", method: "POST", target: "/", status: 405},
		{name: "read body rejected", method: "GET", target: "/", body: `{}`, status: 400, contains: "request body is not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body)))
			if w.Code != tc.status || (tc.contains != "" && !strings.Contains(w.Body.String(), tc.contains)) {
				t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
			}
			if tc.method == "HEAD" && w.Body.Len() != 0 {
				t.Fatalf("head returned body: %s", w.Body.String())
			}
		})
	}
	if err := httpadapter.Bind(a, h, httpadapter.Route{Method: "GET", Path: "/later"}); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("post-serve bind should fail: %v", err)
	}
}

func TestBindStartupDiagnostics(t *testing.T) {
	t.Parallel()
	r := operation.NewRegistry(nil)
	h, err := operation.Register(r, publicGreeting(), func(context.Context, greetingInput) (greetingOutput, error) { return greetingOutput{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		route httpadapter.Route
	}{
		{name: "no method", route: httpadapter.Route{Path: "/greet"}},
		{name: "head explicitly unsupported", route: httpadapter.Route{Method: "HEAD", Path: "/greet"}},
		{name: "wildcard", route: httpadapter.Route{Method: "POST", Path: "/greet/{name}"}},
		{name: "query", route: httpadapter.Route{Method: "POST", Path: "/greet?name=x"}},
		{name: "trailing slash", route: httpadapter.Route{Method: "POST", Path: "/greet/"}},
		{name: "dot segment", route: httpadapter.Route{Method: "POST", Path: "/./greet"}},
		{name: "subtree", route: httpadapter.Route{Method: "POST", Path: "/greet//x"}},
		{name: "no slash", route: httpadapter.Route{Method: "POST", Path: "greet"}},
		{name: "percent escape", route: httpadapter.Route{Method: "POST", Path: "/%67reet"}},
		{name: "nonempty read input", route: httpadapter.Route{Method: "GET", Path: "/greet"}},
		{name: "no content incompatible with json", route: httpadapter.Route{Method: "POST", Path: "/greet", SuccessStatus: 204}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := httpadapter.New(http.NewServeMux(), r, httpadapter.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := httpadapter.Bind(a, h, tc.route); err == nil {
				t.Fatal("expected invalid binding error")
			}
			if len(a.Bindings()) != 0 {
				t.Fatal("failed binding changed route metadata")
			}
		})
	}
	t.Run("mux conflict", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /greet", func(http.ResponseWriter, *http.Request) {})
		a, err := httpadapter.New(mux, r, httpadapter.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := httpadapter.Bind(a, h, httpadapter.Route{Method: "POST", Path: "/greet"}); err == nil || !strings.Contains(err.Error(), "cannot bind") {
			t.Fatalf("expected conflict error, got %v", err)
		}
		if len(a.Bindings()) != 0 {
			t.Fatal("conflict changed route metadata")
		}
	})
	t.Run("foreign registry", func(t *testing.T) {
		other := operation.NewRegistry(nil)
		if err := other.Freeze(); err != nil {
			t.Fatal(err)
		}
		a, err := httpadapter.New(http.NewServeMux(), other, httpadapter.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := httpadapter.Bind(a, h, httpadapter.Route{Method: "POST", Path: "/greet"}); err == nil {
			t.Fatal("foreign handle accepted")
		}
	})
}

func TestBindingsAreSortedAndDetached(t *testing.T) {
	t.Parallel()
	r := operation.NewRegistry(nil)
	var handles []*operation.Handle[greetingInput, greetingOutput]
	for _, id := range []string{"greeting.b.v1", "greeting.a.v1"} {
		definition := publicGreeting()
		definition.ID = id
		h, err := operation.Register(r, definition, func(context.Context, greetingInput) (greetingOutput, error) { return greetingOutput{}, nil })
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, h)
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
	a, err := httpadapter.New(http.NewServeMux(), r, httpadapter.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{"/b", "/a"} {
		if err := httpadapter.Bind(a, handles[i], httpadapter.Route{Method: "POST", Path: path}); err != nil {
			t.Fatal(err)
		}
	}
	bindings := a.Bindings()
	if len(bindings) != 2 || bindings[0].Path != "/a" || bindings[1].Path != "/b" || bindings[0].SuccessStatus != 200 {
		t.Fatalf("unexpected bindings: %#v", bindings)
	}
	bindings[0].Path = "/tampered"
	if a.Bindings()[0].Path != "/a" {
		t.Fatal("bindings snapshot mutated adapter")
	}
	if err := httpadapter.Bind(a, handles[0], httpadapter.Route{Method: "POST", Path: "/alias"}); err == nil {
		t.Fatal("multiple routes for one operation accepted")
	}
}

func TestBindConcurrentRequests(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	mux, _, _ := setup(t, publicGreeting(), func(context.Context, greetingInput) (greetingOutput, error) {
		calls.Add(1)
		return greetingOutput{Greeting: "ok"}, nil
	}, nil, httpadapter.Options{}, httpadapter.Route{Method: "POST", Path: "/greet"})
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, jsonRequest("POST", "/greet", `{"name":"Ada"}`))
			if w.Code != 200 {
				t.Errorf("unexpected status %d: %s", w.Code, w.Body.String())
			}
			ids <- w.Header().Get("X-Request-ID")
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if id == "" || seen[id] {
			t.Errorf("missing or reused request id %q", id)
		}
		seen[id] = true
	}
	if calls.Load() != 20 {
		t.Fatalf("handler called %d times", calls.Load())
	}
}

func TestBindTrustedContextIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts httpadapter.Options
		want int
	}{
		{name: "middleware identity retained", want: http.StatusOK},
		{name: "authenticator owns identity", opts: httpadapter.Options{Authenticate: func(*http.Request) (operation.Principal, error) {
			return operation.Principal{}, nil
		}}, want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := publicGreeting()
			definition.Access = operation.Permission("greetings:write")
			var calls int
			mux, _, _ := setup(t, definition, func(ctx context.Context, in greetingInput) (greetingOutput, error) {
				calls++
				principal, ok := operation.PrincipalFromContext(ctx)
				if !ok || principal.TenantID != "trusted-tenant" {
					t.Errorf("unexpected principal: %#v", principal)
				}
				return greetingOutput{Greeting: "hello", Tenant: principal.TenantID}, nil
			}, operation.AuthorizerFunc(func(context.Context, operation.Principal, string, any) error { return nil }), tc.opts, httpadapter.Route{Method: "POST", Path: "/greet"})
			ctx := operation.WithPrincipal(context.Background(), operation.Principal{ID: "verified-user", TenantID: "trusted-tenant"})
			r := jsonRequest("POST", "/greet", `{"name":"Ada"}`).WithContext(ctx)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusUnauthorized && calls != 0 {
				t.Fatal("anonymous authentication result retained a prior identity")
			}
		})
	}
}

package trest_test

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/operation"
)

func TestTypedContextRoutePreservesRetainedRequests(t *testing.T) {
	t.Parallel()
	type callerKey struct{}
	type input struct {
		ID    int `path:"id" min:"1"`
		Limit int `query:"limit" min:"1"`
		Body  struct {
			Name string `json:"name" minLength:"2"`
		} `body:"json"`
	}
	type output struct {
		Name string `json:"name"`
	}
	principal := trest.Principal{ID: "verified", TenantID: "acme"}
	var middlewareRequest, authRequest *http.Request
	var middlewareContext, authContext context.Context
	var after trest.Event
	app, err := trest.New(
		trest.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				middlewareRequest, middlewareContext = r, r.Context()
				before := r.Clone(r.Context())
				multipartBefore := r.MultipartForm
				next.ServeHTTP(w, r)
				if r.Body != before.Body || r.Context() != before.Context() || r.MultipartForm != multipartBefore ||
					!reflect.DeepEqual(r.Form, before.Form) || !reflect.DeepEqual(r.PostForm, before.PostForm) || !reflect.DeepEqual(r.MultipartForm, before.MultipartForm) {
					t.Error("typed binding mutated the request retained by middleware")
				}
			})
		}),
		trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
			authRequest, authContext = r, r.Context()
			return principal, nil
		}),
		trest.RequireAuth(),
		trest.AfterResponse(func(event trest.Event) { after = event }),
		trest.Post("/items/{id}", func(ctx context.Context, in input) (output, error) {
			identity, ok := trest.Identity(ctx)
			if !ok || identity != principal || ctx.Value(callerKey{}) != "caller" || trest.RequestID(ctx) == "" || in.ID != 2 || in.Limit != 3 || in.Body.Name != "Ada" {
				return output{}, errors.New("typed context or input changed")
			}
			if err := trest.SetHeader(ctx, "X-Handler", "present"); err != nil {
				return output{}, err
			}
			if err := trest.SetCookie(ctx, &http.Cookie{Name: "session", Value: "example", HttpOnly: true}); err != nil {
				return output{}, err
			}
			return output{Name: in.Body.Name}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	parent := context.WithValue(context.Background(), callerKey{}, "caller")
	reader := strings.NewReader(`{"name":"Ada"}`)
	r := httptest.NewRequest(http.MethodPost, "/items/2?limit=3", nil).WithContext(parent)
	r.Body = io.NopCloser(reader)
	r.Header.Set("Content-Type", "application/json")
	r.Form = url.Values{"original": {"form"}}
	r.PostForm = url.Values{"original": {"post"}}
	r.MultipartForm = &multipart.Form{Value: map[string][]string{"original": {"multipart"}}}
	body := r.Body
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != `{"name":"Ada"}` || w.Header().Get("X-Handler") != "present" || len(w.Result().Cookies()) != 1 {
		t.Fatalf("typed response failed: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if middlewareRequest == nil || authRequest == nil || middlewareRequest == r || authRequest != middlewareRequest || middlewareRequest.Context() != middlewareContext || authRequest.Context() != authContext {
		t.Fatal("retained request/context identity changed")
	}
	if _, ok := trest.Identity(authContext); ok {
		t.Fatal("post-auth principal became visible through a retained pre-auth context")
	}
	if trest.RequestID(authContext) != w.Header().Get("X-Request-ID") || authRequest.Body != body || reader.Len() != 0 || r.Context() != parent || r.Body != body || trest.RequestID(parent) != "" {
		t.Fatal("request metadata, body ownership, or consumption changed")
	}
	if after.Status != http.StatusOK || after.RequestID != w.Header().Get("X-Request-ID") || after.Method != http.MethodPost || after.Path != "/items/{id}" {
		t.Fatalf("completion metadata changed: %+v", after)
	}
}

func TestTypedContextRouteHonorsMiddlewareContextReplacement(t *testing.T) {
	t.Parallel()
	type callerKey struct{}
	principal := trest.Principal{ID: "verified", TenantID: "acme"}
	var replacedRequest *http.Request
	var replacedContext context.Context
	app, err := trest.New(
		trest.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				replacedContext = context.WithValue(context.Background(), callerKey{}, "replacement")
				replacedRequest = r.WithContext(replacedContext)
				next.ServeHTTP(w, replacedRequest)
			})
		}),
		trest.Authenticate(func(*http.Request) (trest.Principal, error) { return principal, nil }),
		trest.Get("/", func(ctx context.Context, _ trest.Empty) (string, error) {
			identity, ok := trest.Identity(ctx)
			if !ok || identity != principal || ctx.Value(callerKey{}) != "replacement" || trest.RequestID(ctx) != "" || ctx.Err() != nil {
				return "", errors.New("middleware context replacement was not preserved")
			}
			if trest.SetHeader(ctx, "X-Leaked", "bad") == nil || trest.SetCookie(ctx, &http.Cookie{Name: "leaked", Value: "bad"}) == nil {
				return "", errors.New("removed request state was restored")
			}
			return "ok", nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent))
	if w.Code != http.StatusOK || w.Body.String() != "ok" || w.Header().Get("X-Request-ID") == "" || w.Header().Get("X-Leaked") != "" || len(w.Result().Cookies()) != 0 {
		t.Fatalf("middleware replacement failed: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if replacedRequest.Context() != replacedContext {
		t.Fatal("authentication changed the middleware's retained context")
	}
	if _, ok := trest.Identity(replacedContext); ok {
		t.Fatal("authentication mutated the replacement context")
	}
}

func TestTypedContextRouteHonorsAuthenticatorContextUpdate(t *testing.T) {
	t.Parallel()
	type authenticationKey struct{}
	app, err := trest.New(
		trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
			*r = *r.WithContext(context.WithValue(r.Context(), authenticationKey{}, "auth metadata"))
			return trest.Principal{ID: "verified"}, nil
		}),
		trest.Get("/", func(ctx context.Context, _ trest.Empty) (string, error) {
			principal, ok := trest.Identity(ctx)
			if !ok || principal.ID != "verified" || ctx.Value(authenticationKey{}) != "auth metadata" {
				return "", errors.New("authenticator context update was lost")
			}
			return "ok", nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("authenticator context update failed: %d %s", w.Code, w.Body)
	}
}

func TestTypedContextRoutePanicHookUsesUpdatedOuterRequestContext(t *testing.T) {
	t.Parallel()
	type callbackKey struct{}
	for _, test := range []struct {
		name, callback string
		keepState      bool
	}{
		{"auth preserves state", "auth", true},
		{"auth removes state", "auth", false},
		{"middleware preserves state", "middleware", true},
		{"middleware removes state", "middleware", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutateAndPanic := func(r *http.Request) {
				parent := context.Background()
				if test.keepState {
					parent = r.Context()
				}
				*r = *r.WithContext(context.WithValue(parent, callbackKey{}, "updated before panic"))
				panic("private callback panic")
			}
			var callback trest.Component
			if test.callback == "auth" {
				callback = trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
					mutateAndPanic(r)
					return trest.Principal{}, nil
				})
			} else {
				callback = trest.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mutateAndPanic(r)
						next.ServeHTTP(w, r)
					})
				})
			}
			hooks, calls := 0, 0
			var hookID string
			app, err := trest.New(callback,
				trest.OnError(func(ctx context.Context, err error) error {
					hooks++
					if ctx.Value(callbackKey{}) != "updated before panic" {
						t.Error("panic hook lost the callback's in-place context update")
					}
					hookID = trest.RequestID(ctx)
					headerErr := trest.SetHeader(ctx, "X-Panic-Hook", "called")
					if (headerErr == nil) != test.keepState {
						t.Errorf("request-state availability changed: %v", headerErr)
					}
					return err
				}),
				trest.Get("/", func(context.Context, trest.Empty) (string, error) {
					calls++
					return "must not run", nil
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			originalContext := r.Context()
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != http.StatusInternalServerError || hooks != 1 || calls != 0 || strings.Contains(w.Body.String(), "private") || r.Context() != originalContext || r.Context().Value(callbackKey{}) != nil {
				t.Fatalf("panic path changed: status=%d hooks=%d calls=%d body=%s", w.Code, hooks, calls, w.Body)
			}
			if test.keepState {
				if hookID == "" || hookID != w.Header().Get("X-Request-ID") || w.Header().Get("X-Panic-Hook") != "called" {
					t.Fatal("panic hook lost preserved request metadata")
				}
			} else if hookID != "" || w.Header().Get("X-Panic-Hook") != "" {
				t.Fatal("panic recovery restored request state removed by the callback")
			}
		})
	}
}

func TestTypedContextRouteErrorHookGetsAuthenticatedContext(t *testing.T) {
	t.Parallel()
	type input struct {
		Name string `json:"name" minLength:"2"`
	}
	principal := trest.Principal{ID: "verified", TenantID: "tenant"}
	for _, test := range []struct {
		name, body string
		status     int
		calls      int
	}{
		{"binding", `{"name":"x"}`, http.StatusBadRequest, 0},
		{"invocation", `{"name":"Ada"}`, http.StatusConflict, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls, hooks := 0, 0
			var hookID string
			app, err := trest.New(
				trest.Authenticate(func(*http.Request) (trest.Principal, error) { return principal, nil }),
				trest.OnError(func(ctx context.Context, err error) error {
					hooks++
					identity, ok := trest.Identity(ctx)
					if !ok || identity != principal {
						t.Error("error hook lost the authenticated principal")
					}
					hookID = trest.RequestID(ctx)
					if headerErr := trest.SetHeader(ctx, "X-Error-Hook", "called"); headerErr != nil {
						t.Error(headerErr)
					}
					return err
				}),
				trest.Post("/", func(context.Context, input) (string, error) {
					calls++
					return "", trest.NewError(trest.Conflict, "not ready", nil)
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			w := request(t, app, http.MethodPost, "/", test.body, "")
			if w.Code != test.status || calls != test.calls || hooks != 1 || hookID == "" || hookID != w.Header().Get("X-Request-ID") || w.Header().Get("X-Error-Hook") != "called" {
				t.Fatalf("error path changed: status=%d calls=%d hooks=%d id=%q headers=%v body=%s", w.Code, calls, hooks, hookID, w.Header(), w.Body)
			}
		})
	}
}

func TestTypedContextRouteRetainedContextsRemainIndependent(t *testing.T) {
	t.Parallel()
	type callerKey struct{}
	type output struct {
		ID string `json:"id"`
	}
	var contexts []context.Context
	app, err := trest.New(trest.Get("/", func(ctx context.Context, _ trest.Empty) (output, error) {
		contexts = append(contexts, ctx)
		if err := trest.SetHeader(ctx, "X-Handler", "yes"); err != nil {
			return output{}, err
		}
		if err := trest.SetCookie(ctx, &http.Cookie{Name: "session", Value: "value", HttpOnly: true}); err != nil {
			return output{}, err
		}
		return output{ID: trest.RequestID(ctx)}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var ids, spans []string
	for _, name := range []string{"first", "second"} {
		parent := context.WithValue(context.Background(), callerKey{}, name)
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent)
		body := r.Body
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Header().Get("X-Handler") != "yes" || len(w.Result().Cookies()) != 1 || r.Context() != parent || r.Body != body || trest.RequestID(parent) != "" {
			t.Fatalf("direct typed route changed request/response behavior: %d %v %s", w.Code, w.Header(), w.Body)
		}
		ids = append(ids, w.Header().Get("X-Request-ID"))
		spans = append(spans, operation.TraceFromContext(contexts[len(contexts)-1]).SpanID)
	}
	if len(contexts) != 2 || contexts[0] == contexts[1] || ids[0] == "" || ids[0] == ids[1] || spans[0] == "" || spans[0] == spans[1] {
		t.Fatal("request or invocation identity was reused")
	}
	for i, name := range []string{"first", "second"} {
		ctx := contexts[i]
		if ctx.Value(callerKey{}) != name || trest.RequestID(ctx) != ids[i] || operation.TraceFromContext(ctx).SpanID != spans[i] || ctx.Err() != context.Canceled || context.Cause(ctx) != context.Canceled {
			t.Fatal("retained context changed after a later request")
		}
		if trest.SetHeader(ctx, "X-Late", "bad") == nil || trest.SetCookie(ctx, &http.Cookie{Name: "late", Value: "bad"}) == nil {
			t.Fatal("response mutation succeeded after response start")
		}
	}
}

func TestTypedContextRouteAuthenticatedBodyPreservesParentCancellation(t *testing.T) {
	t.Parallel()
	type input struct {
		Name string `json:"name"`
	}
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	defer close(release)
	app, err := trest.New(
		trest.Authenticate(func(*http.Request) (trest.Principal, error) { return trest.Principal{ID: "verified"}, nil }),
		trest.Post("/", func(ctx context.Context, _ input) (string, error) {
			entered <- ctx
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-release:
				return "", errors.New("test released blocked handler")
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"Ada"}`)).WithContext(parent)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { app.ServeHTTP(w, r); close(done) }()
	var handlerContext context.Context
	select {
	case handlerContext = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("authenticated handler did not start")
	}
	cause := errors.New("caller canceled authenticated request")
	cancel(cause)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parent cancellation did not reach the typed handler")
	}
	if w.Code != http.StatusRequestTimeout || !errors.Is(context.Cause(handlerContext), cause) || r.Context() != parent || trest.RequestID(handlerContext) != w.Header().Get("X-Request-ID") {
		t.Fatalf("cancellation semantics changed: status=%d cause=%v", w.Code, context.Cause(handlerContext))
	}
}

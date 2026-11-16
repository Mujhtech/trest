package trest_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mujhtech/trest"
)

func TestEmptyInputRetainsRequestChecks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		query   string
		body    string
		length  int64
		nilBody bool
		status  int
	}{
		{name: "empty", status: 200},
		{name: "nil body", nilBody: true, status: 200},
		{name: "unknown length empty", length: -1, status: 200},
		{name: "empty query separators", query: "&", status: 200},
		{name: "unknown query", query: "extra=1", status: 400},
		{name: "malformed query", query: "extra=%zz", status: 400},
		{name: "malformed query separator", query: "extra=1;other=2", status: 400},
		{name: "oversized NoBody declared length", length: 1025, status: 413},
		{name: "oversized nil body declared length", nilBody: true, length: 1025, status: 413},
		{name: "body with zero length", body: "x", status: 400},
		{name: "body with unknown length", body: "x", length: -1, status: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			app, err := trest.New(trest.Limits(1024, 1024), trest.Get("/", func(ctx context.Context, _ trest.Empty) (string, error) {
				calls++
				if _, ok := ctx.Deadline(); !ok || trest.RequestID(ctx) == "" {
					t.Error("request IDs or invocation deadline were lost")
				}
				return "ok", nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.URL.RawQuery = test.query
			r.ContentLength = test.length
			if test.body != "" {
				r.Body = io.NopCloser(strings.NewReader(test.body))
			} else if test.nilBody {
				r.Body = nil
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			wantCalls := 0
			if test.status == http.StatusOK {
				wantCalls = 1
			}
			if w.Code != test.status || calls != wantCalls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body)
			}
		})
	}
}

type emptyInputReader struct {
	err   error
	reads int
}

func (r *emptyInputReader) Read([]byte) (int, error) {
	r.reads++
	return 0, r.err
}

func (*emptyInputReader) Close() error { return nil }

func TestEmptyInputProbesArbitraryReaders(t *testing.T) {
	t.Parallel()
	readErr := errors.New("private body read failure")
	for _, test := range []struct {
		name   string
		length int64
		err    error
		status int
	}{
		{name: "zero length EOF", err: io.EOF, status: http.StatusOK},
		{name: "unknown length EOF", length: -1, err: io.EOF, status: http.StatusOK},
		{name: "zero length read error", err: readErr, status: http.StatusBadRequest},
		{name: "unknown length read error", length: -1, err: readErr, status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			app, err := trest.New(trest.Get("/", func(context.Context, trest.Empty) (string, error) {
				calls++
				return "ok", nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			body := &emptyInputReader{err: test.err}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Body = body
			r.ContentLength = test.length
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			wantCalls := 0
			if test.status == http.StatusOK {
				wantCalls = 1
			}
			if body.reads == 0 || w.Code != test.status || calls != wantCalls {
				t.Fatalf("reads=%d status=%d calls=%d body=%s", body.reads, w.Code, calls, w.Body)
			}
			if strings.Contains(w.Body.String(), readErr.Error()) {
				t.Fatal("private body read error escaped into response")
			}
		})
	}
}

func TestEmptyInputLeavesIgnoredFieldsZero(t *testing.T) {
	t.Parallel()
	type input struct {
		Ignored string `json:"-"`
		hidden  *string
	}
	for _, test := range []struct {
		name, query string
	}{
		{name: "empty request"},
		{name: "empty parsed query", query: "&"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			app, err := trest.New(trest.Get("/", func(_ context.Context, in input) (string, error) {
				calls++
				if in.Ignored != "" || in.hidden != nil {
					t.Errorf("ignored fields were populated: %+v", in)
				}
				return "ok", nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.URL.RawQuery = test.query
			r.Header.Set("Ignored", "unbound header")
			r.AddCookie(&http.Cookie{Name: "Ignored", Value: "unbound-cookie"})
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Body.String() != "ok" || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body)
			}
		})
	}
}

func TestEmptyInputChecksAuthenticatedRequestMutations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "added body", mutate: func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader("x"))
			r.ContentLength = 0
		}},
		{name: "added unknown query", mutate: func(r *http.Request) {
			r.URL.RawQuery = "extra=1"
		}},
		{name: "added malformed query", mutate: func(r *http.Request) {
			r.URL.RawQuery = "extra=%zz"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			authCalls, handlerCalls := 0, 0
			app, err := trest.New(
				trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
					authCalls++
					test.mutate(r)
					return trest.Principal{ID: "verified"}, nil
				}),
				trest.RequireAuth(),
				trest.Get("/", func(context.Context, trest.Empty) (string, error) {
					handlerCalls++
					return "unexpected", nil
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			if w.Code != http.StatusBadRequest || authCalls != 1 || handlerCalls != 0 {
				t.Fatalf("status=%d authCalls=%d handlerCalls=%d body=%s", w.Code, authCalls, handlerCalls, w.Body)
			}
		})
	}
}

func TestEmptyInputRejectsAnonymousBeforeHandler(t *testing.T) {
	t.Parallel()
	calls := 0
	app, err := trest.New(trest.RequireAuth(), trest.Get("/", func(context.Context, trest.Empty) (string, error) {
		calls++
		return "unexpected", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body)
	}
}

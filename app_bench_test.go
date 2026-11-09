package trest_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"trest.local/trest"
	"trest.local/trest/operation"
)

type benchmarkPostInput struct {
	TenantID string `json:"tenant_id" minLength:"1" maxLength:"32"`
	SKU      string `json:"sku" minLength:"1" maxLength:"32"`
	Quantity int    `json:"quantity" min:"1" max:"100"`
}

type benchmarkPostOutput struct {
	TenantID string `json:"tenant_id" minLength:"1"`
	SKU      string `json:"sku" minLength:"1"`
	Total    int    `json:"total" min:"1"`
}

const benchmarkPostBody = `{"tenant_id":"acme","sku":"book","quantity":3}`
const benchmarkHTTPBytes = 1024

// BenchmarkTypedPOST includes in-process request/recorder creation. Both variants
// use the same business function, authentication, authorization, schema checks,
// concurrency limit, deadline, tracing, body/output limits, and panic recovery.
// DirectHTTPSharedInvoker is hand-written HTTP glue around operation.Handle; it
// is deliberately not a framework-free net/http baseline. Neither uses a network,
// a database, observers, or application I/O. Setup and equivalence checks are not timed.
func BenchmarkTypedPOST(b *testing.B) {
	for _, variant := range []string{"Trest", "DirectHTTPSharedInvoker"} {
		b.Run(variant, func(b *testing.B) {
			handler := newBenchmarkPostHandler(b, variant, nil)
			b.ReportAllocs()
			b.SetBytes(int64(len(benchmarkPostBody)))
			b.ResetTimer()
			// The module supports Go 1.23, which predates testing.B.Loop.
			for i := 0; i < b.N; i++ {
				r := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(benchmarkPostBody))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer acme")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != http.StatusCreated {
					b.Fatalf("POST failed: %d %s", w.Code, w.Body)
				}
			}
		})
	}
}

func newBenchmarkPostHandler(tb testing.TB, variant string, calls *int) http.Handler {
	tb.Helper()
	authenticate := func(r *http.Request) (operation.Principal, error) {
		if r.Header.Get("Authorization") == "Bearer acme" {
			return operation.Principal{ID: "actor", TenantID: "acme"}, nil
		}
		return operation.Principal{}, nil
	}
	validate := func(_ context.Context, in benchmarkPostInput) error {
		if in.SKU == "reserved" {
			return operation.NewError(operation.InvalidInput, "SKU is reserved", nil)
		}
		return nil
	}
	authorize := func(_ context.Context, principal operation.Principal, in benchmarkPostInput) error {
		if principal.TenantID != in.TenantID {
			return operation.NewError(operation.Forbidden, "wrong tenant", nil)
		}
		return nil
	}
	service := func(ctx context.Context, in benchmarkPostInput) (benchmarkPostOutput, error) {
		if calls != nil {
			*calls++
		}
		if _, ok := ctx.Deadline(); !ok || operation.CorrelationID(ctx) == "" || operation.TraceFromContext(ctx).SpanID == "" {
			return benchmarkPostOutput{}, errors.New("missing invocation metadata")
		}
		if in.SKU == "panic" {
			panic("private handler detail")
		}
		out := benchmarkPostOutput{TenantID: in.TenantID, SKU: in.SKU, Total: in.Quantity * 125}
		if in.SKU == "bad-output" {
			out.Total = 0
		}
		return out, nil
	}
	if variant == "Trest" {
		app, err := trest.New(
			trest.Authenticate(authenticate), trest.Concurrency(128), trest.Limits(benchmarkHTTPBytes, benchmarkHTTPBytes),
			trest.Post("/orders", service).Name("createOrder").Status(http.StatusCreated).Timeout(30*time.Second).Validate(validate).Authorize(authorize),
		)
		if err != nil {
			tb.Fatal(err)
		}
		return app
	}
	registry := operation.NewRegistry(nil, operation.WithMaxConcurrent(128))
	handle, err := operation.Register(registry, operation.Definition[benchmarkPostInput, benchmarkPostOutput]{
		ID: "orders.create.v1", Summary: "Create order", Access: operation.Permission("authenticated"),
		Timeout: 30 * time.Second, Validate: validate, Authorize: authorize,
	}, service)
	if err != nil {
		tb.Fatal(err)
	}
	if err = registry.Freeze(); err != nil {
		tb.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		id := hex.EncodeToString(random[:])
		w.Header().Set("X-Request-ID", id)
		fail := func(status int, err error) {
			public := operation.PublicError(err)
			if status == 0 {
				status = benchmarkErrorStatus(public.Code)
			}
			data, _ := json.Marshal(struct {
				Code      operation.Code `json:"code"`
				Message   string         `json:"message"`
				RequestID string         `json:"request_id"`
			}{public.Code, public.Message, id})
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_, _ = w.Write(data)
		}
		defer func() {
			if recover() != nil {
				fail(500, operation.NewError(operation.Internal, "internal error", nil))
			}
		}()
		principal, err := authenticate(r)
		if err != nil {
			fail(0, err)
			return
		}
		ctx := operation.WithPrincipal(operation.WithCorrelationID(r.Context(), id), principal)
		query, queryErr := url.ParseQuery(r.URL.RawQuery)
		if queryErr != nil || len(query) != 0 {
			fail(400, operation.NewError(operation.InvalidInput, "unexpected query", queryErr))
			return
		}
		if r.ContentLength > benchmarkHTTPBytes {
			fail(413, operation.NewError(operation.InvalidInput, "body exceeds limit", nil))
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			fail(415, operation.NewError(operation.InvalidInput, "expected JSON", err))
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, benchmarkHTTPBytes))
		if err != nil {
			fail(413, operation.NewError(operation.InvalidInput, "body exceeds limit", err))
			return
		}
		if err = handle.ValidateJSON(data); err != nil {
			fail(0, err)
			return
		}
		var in benchmarkPostInput
		if err = json.Unmarshal(data, &in); err != nil {
			fail(400, operation.NewError(operation.InvalidInput, "invalid JSON", err))
			return
		}
		out, err := handle.Invoke(ctx, in)
		if err != nil {
			fail(0, err)
			return
		}
		data, err = json.Marshal(out)
		if err != nil || len(data) > benchmarkHTTPBytes {
			fail(500, operation.NewError(operation.Internal, "invalid output", err))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(data)
	})
	return mux
}

func benchmarkErrorStatus(code operation.Code) int {
	switch code {
	case operation.InvalidInput:
		return 400
	case operation.Unauthenticated:
		return 401
	case operation.Forbidden:
		return 403
	case operation.RateLimited:
		return 429
	case operation.DeadlineExceeded:
		return 504
	case operation.Canceled:
		return 408
	default:
		return 500
	}
}

func TestTypedPOSTBenchmarkPolicyEquivalence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body, token, contentType, query string
		status, calls                         int
		code                                  operation.Code
	}{
		{name: "success", body: benchmarkPostBody, token: "Bearer acme", contentType: "application/json", status: 201, calls: 1},
		{name: "anonymous", body: benchmarkPostBody, contentType: "application/json", status: 401, code: operation.Unauthenticated},
		{name: "wrong tenant", body: strings.Replace(benchmarkPostBody, "acme", "other", 1), token: "Bearer acme", contentType: "application/json", status: 403, code: operation.Forbidden},
		{name: "domain validation", body: strings.Replace(benchmarkPostBody, "book", "reserved", 1), token: "Bearer acme", contentType: "application/json", status: 400, code: operation.InvalidInput},
		{name: "structural validation", body: strings.Replace(benchmarkPostBody, `:3`, `:0`, 1), token: "Bearer acme", contentType: "application/json", status: 400, code: operation.InvalidInput},
		{name: "unknown field", body: strings.TrimSuffix(benchmarkPostBody, "}") + `,"extra":1}`, token: "Bearer acme", contentType: "application/json", status: 400, code: operation.InvalidInput},
		{name: "duplicate field", body: strings.TrimSuffix(benchmarkPostBody, "}") + `,"quantity":4}`, token: "Bearer acme", contentType: "application/json", status: 400, code: operation.InvalidInput},
		{name: "trailing JSON", body: benchmarkPostBody + `{}`, token: "Bearer acme", contentType: "application/json", status: 400, code: operation.InvalidInput},
		{name: "wrong media type", body: benchmarkPostBody, token: "Bearer acme", contentType: "text/plain", status: 415, code: operation.InvalidInput},
		{name: "body limit", body: strings.Repeat(" ", benchmarkHTTPBytes) + benchmarkPostBody, token: "Bearer acme", contentType: "application/json", status: 413, code: operation.InvalidInput},
		{name: "unknown query", body: benchmarkPostBody, token: "Bearer acme", contentType: "application/json", query: "?extra=1", status: 400, code: operation.InvalidInput},
		{name: "output validation", body: strings.Replace(benchmarkPostBody, "book", "bad-output", 1), token: "Bearer acme", contentType: "application/json", status: 500, calls: 1, code: operation.Internal},
		{name: "panic recovery", body: strings.Replace(benchmarkPostBody, "book", "panic", 1), token: "Bearer acme", contentType: "application/json", status: 500, calls: 1, code: operation.Internal},
	}
	for _, variant := range []string{"Trest", "DirectHTTPSharedInvoker"} {
		t.Run(variant, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					calls := 0
					handler := newBenchmarkPostHandler(t, variant, &calls)
					r := httptest.NewRequest(http.MethodPost, "/orders"+tc.query, strings.NewReader(tc.body))
					r.Header.Set("Content-Type", tc.contentType)
					r.Header.Set("Authorization", tc.token)
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					if w.Code != tc.status || calls != tc.calls {
						t.Fatalf("status=%d calls=%d body=%s; want status=%d calls=%d", w.Code, calls, w.Body, tc.status, tc.calls)
					}
					if w.Header().Get("X-Request-ID") == "" || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
						t.Fatalf("missing response metadata: %#v", w.Header())
					}
					if tc.status == 201 {
						if got, want := w.Body.String(), `{"tenant_id":"acme","sku":"book","total":375}`; got != want {
							t.Fatalf("response=%s, want %s", got, want)
						}
						return
					}
					var failure struct{ Code operation.Code }
					if err := json.Unmarshal(w.Body.Bytes(), &failure); err != nil || failure.Code != tc.code || strings.Contains(w.Body.String(), "private") {
						t.Fatalf("error response=%s, want code=%s", w.Body, tc.code)
					}
				})
			}
		})
	}
}

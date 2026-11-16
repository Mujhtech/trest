package adoption_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mujhtech/trest/experiments/adoption"
)

func cancelableHelloRequest(handler http.Handler) *httptest.ResponseRecorder {
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/hello", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	cancel()
	return w
}

func cancelableInvoiceRequest(handler http.Handler) *httptest.ResponseRecorder {
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/invoices", strings.NewReader(adoption.Body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer acme-test")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	cancel()
	return w
}

// BenchmarkCancelableHelloJSON supplements the background-context benchmark
// with a fresh cancelable parent, as incoming net/http server requests have.
// Context creation and cancellation are timed equally for every framework.
func BenchmarkCancelableHelloJSON(b *testing.B) {
	for _, factory := range adoption.HelloFactories() {
		b.Run("framework="+factory.Name, func(b *testing.B) {
			fixture, err := factory.New()
			if err != nil {
				b.Fatal(err)
			}
			checkHello(b, cancelableHelloRequest(fixture.Handler))
			b.ReportAllocs()
			for b.Loop() {
				w := cancelableHelloRequest(fixture.Handler)
				if w.Code != http.StatusOK {
					b.Fatalf("hello: %d %s", w.Code, w.Body)
				}
			}
		})
	}
}

// BenchmarkCancelableDuplicateInvoice retains the same warmed duplicate-only
// workload, but every timed request receives and cancels a fresh parent context.
func BenchmarkCancelableDuplicateInvoice(b *testing.B) {
	for _, factory := range adoption.Factories() {
		b.Run("framework="+factory.Name, func(b *testing.B) {
			fixture, err := factory.New()
			if err != nil {
				b.Fatal(err)
			}
			warm := cancelableInvoiceRequest(fixture.Handler)
			if warm.Code != http.StatusCreated || fixture.Store.Count() != 1 {
				b.Fatalf("warmup: %d %s", warm.Code, warm.Body)
			}
			b.ReportAllocs()
			for b.Loop() {
				w := cancelableInvoiceRequest(fixture.Handler)
				if w.Code != http.StatusCreated {
					b.Fatalf("request: %d %s", w.Code, w.Body)
				}
			}
			if fixture.Store.Count() != 1 {
				b.Fatal("duplicate request caused another write")
			}
		})
	}
}

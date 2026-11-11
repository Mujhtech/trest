package adoption_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"trest.local/trest/experiments/adoption"
)

func request(handler http.Handler, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// TestMaintenanceTasks is the same behavioral evidence for every implementation:
// adding a scoped endpoint, diagnosing bad input, and handling duplicate effects.
func TestMaintenanceTasks(t *testing.T) {
	for _, factory := range adoption.Factories() {
		t.Run(factory.Name, func(t *testing.T) {
			fixture, err := factory.New()
			if err != nil {
				t.Fatal(err)
			}
			t.Run("scoped endpoint accepts owner", func(t *testing.T) {
				w := request(fixture.Handler, "/invoices/preview", "acme-test", adoption.Body)
				if w.Code != 200 || fixture.Store.Count() != 0 {
					t.Fatalf("preview: %d %s writes=%d", w.Code, w.Body.String(), fixture.Store.Count())
				}
			})
			t.Run("scoped endpoint denies other tenant", func(t *testing.T) {
				w := request(fixture.Handler, "/invoices/preview", "globex-test", adoption.Body)
				if w.Code != 403 || fixture.Store.Count() != 0 {
					t.Fatalf("denial: %d %s", w.Code, w.Body.String())
				}
			})
			t.Run("failed request has no side effect", func(t *testing.T) {
				w := request(fixture.Handler, "/invoices", "acme-test", strings.Replace(adoption.Body, "12500", "0", 1))
				if w.Code != 400 || fixture.Store.Count() != 0 {
					t.Fatalf("invalid: %d %s", w.Code, w.Body.String())
				}
			})
			t.Run("missing credentials denied", func(t *testing.T) {
				w := request(fixture.Handler, "/invoices", "", adoption.Body)
				if w.Code != 401 || fixture.Store.Count() != 0 {
					t.Fatalf("auth: %d %s", w.Code, w.Body.String())
				}
			})
			t.Run("concurrent duplicate has one effect", func(t *testing.T) {
				var wg sync.WaitGroup
				for i := 0; i < 16; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						w := request(fixture.Handler, "/invoices", "acme-test", adoption.Body)
						if w.Code != 201 {
							t.Errorf("duplicate: %d %s", w.Code, w.Body.String())
							return
						}
						var invoice adoption.Invoice
						if err := json.Unmarshal(w.Body.Bytes(), &invoice); err != nil {
							t.Error(err)
						}
						if invoice.ID != "inv_1" || invoice.TenantID != "acme" {
							t.Errorf("wrong result: %+v", invoice)
						}
					}()
				}
				wg.Wait()
				if fixture.Store.Count() != 1 {
					t.Fatalf("duplicate writes: %d", fixture.Store.Count())
				}
			})
			t.Run("key reuse with changed input conflicts", func(t *testing.T) {
				w := request(fixture.Handler, "/invoices", "acme-test", strings.Replace(adoption.Body, "12500", "12501", 1))
				if w.Code != 409 || fixture.Store.Count() != 1 {
					t.Fatalf("conflict: %d %s", w.Code, w.Body.String())
				}
			})
		})
	}
}

func BenchmarkDuplicateInvoice(b *testing.B) {
	for _, factory := range adoption.Factories() {
		b.Run(factory.Name, func(b *testing.B) {
			fixture, err := factory.New()
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w := request(fixture.Handler, "/invoices", "acme-test", adoption.Body)
				if w.Code != 201 {
					b.Fatal(w.Code)
				}
			}
		})
	}
}

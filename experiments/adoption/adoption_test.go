package adoption_test

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mujhtech/trest/experiments/adoption"
)

func request(handler http.Handler, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func newFixture(t *testing.T, factory adoption.Factory) *adoption.Fixture {
	t.Helper()
	fixture, err := factory.New()
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func assertInvoice(t *testing.T, response *httptest.ResponseRecorder, status int, want adoption.Invoice) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("response: %d %s", response.Code, response.Body.String())
	}
	mediaType, _, err := mime.ParseMediaType(response.Header().Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		t.Fatalf("content type: %q (%v)", response.Header().Get("Content-Type"), err)
	}
	var got adoption.Invoice
	decoder := json.NewDecoder(strings.NewReader(response.Body.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("invoice: got %+v, want %+v", got, want)
	}
}

// TestMaintenanceTasks is the same behavioral evidence for every implementation:
// adding a scoped endpoint, diagnosing bad input, and handling duplicate effects.
func TestMaintenanceTasks(t *testing.T) {
	for _, factory := range adoption.Factories() {
		t.Run(factory.Name, func(t *testing.T) {
			t.Run("scoped endpoint accepts owner", func(t *testing.T) {
				fixture := newFixture(t, factory)
				w := request(fixture.Handler, "/invoices/preview", "acme-test", adoption.Body)
				assertInvoice(t, w, http.StatusOK, adoption.Invoice{ID: "preview", TenantID: "acme", AmountMinor: 12500, Currency: "NGN"})
				if fixture.Store.Count() != 0 {
					t.Fatalf("preview writes: %d", fixture.Store.Count())
				}
			})
			t.Run("creation returns complete invoice", func(t *testing.T) {
				fixture := newFixture(t, factory)
				w := request(fixture.Handler, "/invoices", "acme-test", adoption.Body)
				assertInvoice(t, w, http.StatusCreated, adoption.Invoice{ID: "inv_1", TenantID: "acme", AmountMinor: 12500, Currency: "NGN"})
				if fixture.Store.Count() != 1 {
					t.Fatalf("creation writes: %d", fixture.Store.Count())
				}
			})
			t.Run("scoped endpoint denies other tenant", func(t *testing.T) {
				fixture := newFixture(t, factory)
				w := request(fixture.Handler, "/invoices/preview", "globex-test", adoption.Body)
				if w.Code != 403 || fixture.Store.Count() != 0 {
					t.Fatalf("denial: %d %s", w.Code, w.Body.String())
				}
			})
			t.Run("failed request has no side effect", func(t *testing.T) {
				fixture := newFixture(t, factory)
				w := request(fixture.Handler, "/invoices", "acme-test", strings.Replace(adoption.Body, "12500", "0", 1))
				if w.Code != 400 || fixture.Store.Count() != 0 {
					t.Fatalf("invalid: %d %s", w.Code, w.Body.String())
				}
			})
			t.Run("missing credentials denied", func(t *testing.T) {
				fixture := newFixture(t, factory)
				w := request(fixture.Handler, "/invoices", "", adoption.Body)
				if w.Code != 401 || fixture.Store.Count() != 0 {
					t.Fatalf("auth: %d %s", w.Code, w.Body.String())
				}
			})
			t.Run("concurrent duplicate has one effect", func(t *testing.T) {
				fixture := newFixture(t, factory)
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
							return
						}
						if invoice != (adoption.Invoice{ID: "inv_1", TenantID: "acme", AmountMinor: 12500, Currency: "NGN"}) {
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
				fixture := newFixture(t, factory)
				want := adoption.Invoice{ID: "inv_1", TenantID: "acme", AmountMinor: 12500, Currency: "NGN"}
				assertInvoice(t, request(fixture.Handler, "/invoices", "acme-test", adoption.Body), http.StatusCreated, want)
				w := request(fixture.Handler, "/invoices", "acme-test", strings.Replace(adoption.Body, "12500", "12501", 1))
				if w.Code != 409 || fixture.Store.Count() != 1 {
					t.Fatalf("conflict: %d %s", w.Code, w.Body.String())
				}
				assertInvoice(t, request(fixture.Handler, "/invoices", "acme-test", adoption.Body), http.StatusCreated, want)
				if fixture.Store.Count() != 1 {
					t.Fatalf("retry after conflict writes: %d", fixture.Store.Count())
				}
			})
		})
	}
}

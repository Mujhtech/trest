package adoption

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These are common application requirements, not an assertion that all JSON
// parsers, response headers, or error envelopes have identical semantics.
func TestFrameworkRejectsBeforeDomainExecution(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		body   string
		status int
	}{
		{name: "malformed JSON", token: "acme-test", body: `{"customer_id":`, status: http.StatusBadRequest},
		{name: "zero amount", token: "acme-test", body: strings.Replace(Body, "12500", "0", 1), status: http.StatusBadRequest},
		{name: "negative amount", token: "acme-test", body: strings.Replace(Body, "12500", "-1", 1), status: http.StatusBadRequest},
		{name: "amount exceeds limit", token: "acme-test", body: strings.Replace(Body, "12500", "100000001", 1), status: http.StatusBadRequest},
		{name: "unsupported currency", token: "acme-test", body: strings.Replace(Body, "NGN", "EUR", 1), status: http.StatusBadRequest},
		{name: "missing customer", token: "acme-test", body: strings.Replace(Body, "customer-acme", "", 1), status: http.StatusBadRequest},
		{name: "missing idempotency key", token: "acme-test", body: strings.Replace(Body, "same-request", "", 1), status: http.StatusBadRequest},
		{name: "missing authentication", body: Body, status: http.StatusUnauthorized},
		{name: "invalid authentication", token: "invalid-token", body: Body, status: http.StatusUnauthorized},
		{name: "other tenant", token: "globex-test", body: Body, status: http.StatusForbidden},
	}
	for _, factory := range Factories() {
		t.Run(factory.Name, func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					fixture, err := factory.New()
					if err != nil {
						t.Fatal(err)
					}
					calls := 0
					fixture.Store.onExecute = func() { calls++ }
					r := httptest.NewRequest(http.MethodPost, "/invoices", strings.NewReader(test.body))
					r.Header.Set("Content-Type", "application/json")
					if test.token != "" {
						r.Header.Set("Authorization", "Bearer "+test.token)
					}
					w := httptest.NewRecorder()
					fixture.Handler.ServeHTTP(w, r)
					// Huma may classify malformed input as unprocessable content;
					// both statuses reject it before the domain function runs.
					acceptedStatus := w.Code == test.status || (factory.Name == "huma" && test.name == "malformed JSON" && w.Code == http.StatusUnprocessableEntity)
					if !acceptedStatus {
						t.Errorf("status: got %d, want %d; body: %s", w.Code, test.status, w.Body.String())
					}
					if calls != 0 {
						t.Errorf("rejected request entered domain function %d times", calls)
					}
					if fixture.Store.Count() != 0 {
						t.Errorf("rejected request wrote %d invoices", fixture.Store.Count())
					}
				})
			}
		})
	}
}

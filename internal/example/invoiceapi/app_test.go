package invoiceapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trest.local/trest/internal/example/invoiceapi"
	"trest.local/trest/internal/example/invoices"
	"trest.local/trest/operation"
)

func TestInvoiceAPI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		token  string
		body   string
		status int
		code   string
	}{
		{"accepted", "acme-secret", `{"customer_id":"customer-acme","amount_minor":12500,"currency":"NGN"}`, http.StatusCreated, ""},
		{"missing credentials", "", `{"customer_id":"customer-acme","amount_minor":12500,"currency":"NGN"}`, http.StatusUnauthorized, "unauthenticated"},
		{"invalid credentials", "wrong-secret", `{"customer_id":"customer-acme","amount_minor":12500,"currency":"NGN"}`, http.StatusUnauthorized, "unauthenticated"},
		{"cross tenant customer", "acme-secret", `{"customer_id":"customer-globex","amount_minor":12500,"currency":"NGN"}`, http.StatusForbidden, "forbidden"},
		{"forged tenant field", "acme-secret", `{"customer_id":"customer-globex","amount_minor":12500,"currency":"NGN","tenant_id":"globex"}`, http.StatusBadRequest, "invalid_input"},
		{"invalid business value", "acme-secret", `{"customer_id":"customer-acme","amount_minor":0,"currency":"NGN"}`, http.StatusBadRequest, "invalid_input"},
		{"missing required field", "acme-secret", `{"customer_id":"customer-acme","currency":"NGN"}`, http.StatusBadRequest, "invalid_input"},
		{"malformed payload", "acme-secret", `{"customer_id":`, http.StatusBadRequest, "invalid_input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, err := invoiceapi.New(map[string]operation.Principal{
				"acme-secret": {ID: "user-acme", TenantID: "acme"},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/invoices", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			// Unverified identity headers must not change the authenticated tenant.
			req.Header.Set("X-Tenant-ID", "globex")
			response := httptest.NewRecorder()
			app.Handler.ServeHTTP(response, req)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
			if tc.code == "" {
				var invoice invoices.Invoice
				if err := json.Unmarshal(response.Body.Bytes(), &invoice); err != nil {
					t.Fatal(err)
				}
				if invoice.TenantID != "acme" || invoice.CustomerID != "customer-acme" || invoice.AmountMinor != 12500 || app.Repository.Count() != 1 {
					t.Fatalf("incorrect stored result: %+v, count %d", invoice, app.Repository.Count())
				}
				return
			}
			var result struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Code != tc.code {
				t.Fatalf("error code = %q, want %q: %s", result.Code, tc.code, response.Body.String())
			}
			if app.Repository.Count() != 0 {
				t.Fatal("rejected request changed stored invoices")
			}
		})
	}
}

func TestOpenAPIWithoutCredentials(t *testing.T) {
	t.Parallel()
	app, err := invoiceapi.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	other, err := invoiceapi.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := other.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("contract depends on application instance")
	}
	var document struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(first, &document); err != nil {
		t.Fatal(err)
	}
	if document.OpenAPI != "3.1.1" || document.Paths["/invoices"] == nil {
		t.Fatalf("missing invoice contract: %s", first)
	}
	if bytes.Contains(first, []byte("acme-secret")) || bytes.Contains(first, []byte("demo-user")) {
		t.Fatal("contract contains credentials or identity")
	}
}

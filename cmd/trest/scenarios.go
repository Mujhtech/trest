package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/mujhtech/trest/internal/example/invoiceapi"
	"github.com/mujhtech/trest/internal/example/invoices"
	"github.com/mujhtech/trest/operation"
	"github.com/mujhtech/trest/scenario"
)

// invoiceCases is explicit application configuration. Each run constructs fresh
// in-memory storage and synthetic identities. No environment credentials, live
// URLs, databases, or production resources are consulted.
func invoiceCases(observer operation.Observer) []scenario.Case {
	newApp := func() (*invoiceapi.App, error) {
		return invoiceapi.New(map[string]operation.Principal{"scenario-token": {ID: "scenario-user", TenantID: "acme"}}, operation.WithObserver(observer))
	}
	request := func(ctx context.Context, app *invoiceapi.App, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/invoices", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer scenario-token")
		response := httptest.NewRecorder()
		app.Handler.ServeHTTP(response, req)
		return response
	}
	return []scenario.Case{
		{Name: "invoice-create", Summary: "Create an invoice and verify the response, tenant, and stored effect in a fresh local fixture", Run: func(ctx context.Context) ([]scenario.Assertion, error) {
			app, err := newApp()
			if err != nil {
				return nil, err
			}
			response := request(ctx, app, `{"customer_id":"customer-acme","amount_minor":12500,"currency":"NGN"}`)
			var invoice invoices.Invoice
			if err := json.Unmarshal(response.Body.Bytes(), &invoice); err != nil {
				return nil, err
			}
			return []scenario.Assertion{
				{Name: "http status is 201", Passed: response.Code == http.StatusCreated},
				{Name: "trusted tenant is preserved", Passed: invoice.TenantID == "acme"},
				{Name: "invoice amount and currency are preserved", Passed: invoice.AmountMinor == 12500 && invoice.Currency == "NGN"},
				{Name: "exactly one invoice was stored", Passed: app.Repository.Count() == 1},
				{Name: "response includes a request id", Passed: response.Header().Get("X-Request-ID") != ""},
			}, nil
		}},
		{Name: "invoice-denied", Summary: "Verify that cross-tenant access is forbidden and produces no invoice", Run: func(ctx context.Context) ([]scenario.Assertion, error) {
			app, err := newApp()
			if err != nil {
				return nil, err
			}
			response := request(ctx, app, `{"customer_id":"customer-globex","amount_minor":12500,"currency":"NGN"}`)
			return []scenario.Assertion{{Name: "http status is 403", Passed: response.Code == http.StatusForbidden}, {Name: "no invoice was stored", Passed: app.Repository.Count() == 0}}, nil
		}},
		{Name: "invoice-validation", Summary: "Verify that invalid business input produces a field error without a write", Run: func(ctx context.Context) ([]scenario.Assertion, error) {
			app, err := newApp()
			if err != nil {
				return nil, err
			}
			response := request(ctx, app, `{"customer_id":"customer-acme","amount_minor":0,"currency":"NGN"}`)
			var failure struct {
				Fields []operation.FieldError `json:"fields"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
				return nil, errors.New("could not decode error fixture")
			}
			fieldFound := false
			for _, field := range failure.Fields {
				fieldFound = fieldFound || field.Path == "/amount_minor"
			}
			return []scenario.Assertion{{Name: "http status is 400", Passed: response.Code == http.StatusBadRequest}, {Name: "amount field error is reported", Passed: fieldFound}, {Name: "no invoice was stored", Passed: app.Repository.Count() == 0}}, nil
		}},
	}
}

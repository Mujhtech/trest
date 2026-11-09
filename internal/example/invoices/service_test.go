package invoices_test

import (
	"context"
	"errors"
	"testing"

	"trest.local/trest/internal/example/invoices"
)

func TestServiceCreate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		tenant string
		input  invoices.CreateInput
		want   error
	}{
		{"valid invoice", "acme", invoices.CreateInput{CustomerID: "customer-1", AmountMinor: 12500, Currency: "NGN"}, nil},
		{"wrong tenant", "other", invoices.CreateInput{CustomerID: "customer-1", AmountMinor: 12500, Currency: "NGN"}, invoices.ErrForbidden},
		{"missing tenant", "", invoices.CreateInput{CustomerID: "customer-1", AmountMinor: 12500, Currency: "NGN"}, invoices.ErrForbidden},
		{"zero amount", "acme", invoices.CreateInput{CustomerID: "customer-1", Currency: "NGN"}, invoices.ErrInvalidInput},
		{"unsupported currency", "acme", invoices.CreateInput{CustomerID: "customer-1", AmountMinor: 1, Currency: "XXX"}, invoices.ErrInvalidInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := invoices.NewMemoryRepository(map[string]string{"customer-1": "acme"})
			service := invoices.NewService(repo)
			invoice, err := service.Create(context.Background(), tc.tenant, tc.input)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Create error = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				if repo.Count() != 0 {
					t.Fatal("rejected call created an invoice")
				}
				return
			}
			if invoice.ID != "inv_1" || invoice.TenantID != "acme" || invoice.AmountMinor != 12500 || repo.Count() != 1 {
				t.Fatalf("unexpected invoice: %+v", invoice)
			}
		})
	}
}

func TestServiceCreateCanceled(t *testing.T) {
	t.Parallel()
	repo := invoices.NewMemoryRepository(map[string]string{"customer-1": "acme"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := invoices.NewService(repo).Create(ctx, "acme", invoices.CreateInput{
		CustomerID: "customer-1", AmountMinor: 1, Currency: "USD",
	})
	if !errors.Is(err, context.Canceled) || repo.Count() != 0 {
		t.Fatalf("canceled call: error %v, stored invoices %d", err, repo.Count())
	}
}

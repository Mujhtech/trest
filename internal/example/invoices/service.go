// Package invoices contains the example's business logic, independent of Trest.
package invoices

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// CreateInput describes business input. Identity is supplied separately by a
// trusted application caller, never accepted from the JSON payload.
type CreateInput struct {
	CustomerID  string `json:"customer_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// Invoice is the stored result of creating an invoice.
type Invoice struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	CustomerID  string `json:"customer_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

var (
	ErrInvalidInput = errors.New("invoices: invalid input")
	ErrForbidden    = errors.New("invoices: customer is outside tenant")
)

// Repository is the storage capability needed by Service. A real application
// can implement it with its own database and transaction policy.
type Repository interface {
	Create(context.Context, string, CreateInput) (Invoice, error)
}

// Service owns invoice business rules and receives its dependencies explicitly.
type Service struct {
	repository Repository
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

// Validate checks the example's business rules. It is shared with the transport
// registration while Create also checks it for ordinary trusted Go callers.
func Validate(in CreateInput) error {
	if in.CustomerID == "" {
		return fmt.Errorf("%w: customer_id is required", ErrInvalidInput)
	}
	if in.AmountMinor <= 0 || in.AmountMinor > 100_000_000 {
		return fmt.Errorf("%w: amount_minor must be between 1 and 100000000", ErrInvalidInput)
	}
	if in.Currency != "USD" && in.Currency != "NGN" {
		return fmt.Errorf("%w: currency must be USD or NGN", ErrInvalidInput)
	}
	return nil
}

// Create requires a trusted tenant selected by the application. Repository
// enforcement prevents writing an invoice for a customer owned by another tenant.
func (s *Service) Create(ctx context.Context, tenantID string, in CreateInput) (Invoice, error) {
	if err := ctx.Err(); err != nil {
		return Invoice{}, err
	}
	if err := Validate(in); err != nil {
		return Invoice{}, err
	}
	if tenantID == "" {
		return Invoice{}, ErrForbidden
	}
	return s.repository.Create(ctx, tenantID, in)
}

// MemoryRepository is ephemeral example storage. It is safe for concurrent use.
// The supplied customer -> tenant ownership map is copied during construction.
type MemoryRepository struct {
	mu        sync.Mutex
	customers map[string]string
	invoices  []Invoice
}

func NewMemoryRepository(customers map[string]string) *MemoryRepository {
	copied := make(map[string]string, len(customers))
	for customerID, tenantID := range customers {
		copied[customerID] = tenantID
	}
	return &MemoryRepository{customers: copied}
}

// OwnsCustomer reports the example's application-owned resource policy.
func (r *MemoryRepository) OwnsCustomer(tenantID, customerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner, ok := r.customers[customerID]
	return ok && tenantID != "" && owner == tenantID
}

func (r *MemoryRepository) Create(ctx context.Context, tenantID string, in CreateInput) (Invoice, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Invoice{}, err
	}
	owner, ok := r.customers[in.CustomerID]
	if !ok || owner != tenantID || tenantID == "" {
		return Invoice{}, ErrForbidden
	}
	invoice := Invoice{
		ID: fmt.Sprintf("inv_%d", len(r.invoices)+1), TenantID: tenantID,
		CustomerID: in.CustomerID, AmountMinor: in.AmountMinor, Currency: in.Currency,
	}
	r.invoices = append(r.invoices, invoice)
	return invoice, nil
}

// Count exposes stored effects for the example's acceptance tests.
func (r *MemoryRepository) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.invoices)
}

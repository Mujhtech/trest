// Package invoices is a complete in-memory example application. Service and its
// data types have no dependency on Trest and can be called from ordinary Go code.
package invoices

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrNotFound  = errors.New("invoice not found")
	ErrForbidden = errors.New("invoice belongs to another tenant")
	ErrInvalid   = errors.New("invalid invoice")
)

// Invoice stores amounts in currency minor units; no floating point money is used.
type Invoice struct {
	ID          string      `json:"id"`
	TenantID    string      `json:"tenant_id"`
	CustomerID  string      `json:"customer_id"`
	AmountMinor int64       `json:"amount_minor"`
	Currency    string      `json:"currency" enum:"NGN,USD"`
	Status      string      `json:"status" enum:"draft,paid"`
	Memo        *string     `json:"memo,omitempty"`
	Attachment  *Attachment `json:"attachment,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// Attachment keeps metadata only. The demo deliberately does not persist files.
type Attachment struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type CreateInput struct {
	CustomerID  string  `json:"customer_id" minLength:"1"`
	AmountMinor int64   `json:"amount_minor" min:"1" max:"100000000"`
	Currency    string  `json:"currency" enum:"NGN,USD"`
	Memo        *string `json:"memo,omitempty" maxLength:"2000"`
}
type UpdateInput struct {
	AmountMinor int64   `json:"amount_minor" min:"1" max:"100000000"`
	Status      string  `json:"status" enum:"draft,paid"`
	Memo        *string `json:"memo,omitempty" maxLength:"2000"`
}
type Filter struct {
	Currency, Status string
	Limit, Offset    int
}
type Page struct {
	Items  []Invoice `json:"items"`
	Total  int       `json:"total"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}
type Change struct {
	Sequence  int64    `json:"sequence"`
	Kind      string   `json:"kind" enum:"created,updated,deleted,attachment"`
	InvoiceID string   `json:"invoice_id"`
	Invoice   *Invoice `json:"invoice,omitempty"`
}
type subscription struct {
	tenant  string
	changes chan Change
}

// Service owns synthetic local data and bounded subscriptions. It starts no
// goroutines. Slow subscribers are disconnected and should obtain a new snapshot.
type Service struct {
	mu          sync.RWMutex
	invoices    map[string]Invoice
	customers   map[string]string
	nextID      int64
	sequence    map[string]int64
	subscribers map[*subscription]struct{}
}

func NewService() *Service {
	return &Service{invoices: map[string]Invoice{}, customers: map[string]string{"customer-acme": "acme", "customer-globex": "globex"}, subscribers: map[*subscription]struct{}{}, sequence: map[string]int64{}}
}
func (s *Service) OwnsCustomer(tenant, customer string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return tenant != "" && s.customers[customer] == tenant
}
func (s *Service) Create(ctx context.Context, tenant string, in CreateInput) (Invoice, error) {
	if err := ctx.Err(); err != nil {
		return Invoice{}, err
	}
	if (in.Memo != nil && len(*in.Memo) > 2000) || in.AmountMinor <= 0 || in.AmountMinor > 100000000 || (in.Currency != "NGN" && in.Currency != "USD") {
		return Invoice{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if tenant == "" || s.customers[in.CustomerID] != tenant {
		return Invoice{}, ErrForbidden
	}
	s.nextID++
	now := time.Now().UTC()
	out := Invoice{ID: fmt.Sprintf("inv_%06d", s.nextID), TenantID: tenant, CustomerID: in.CustomerID, AmountMinor: in.AmountMinor, Currency: in.Currency, Status: "draft", Memo: copyString(in.Memo), CreatedAt: now, UpdatedAt: now}
	s.invoices[out.ID] = out
	s.publish(tenant, "created", out.ID, &out)
	return clone(out), nil
}
func (s *Service) Get(ctx context.Context, tenant, id string) (Invoice, error) {
	if err := ctx.Err(); err != nil {
		return Invoice{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out, ok := s.invoices[id]
	if !ok {
		return Invoice{}, ErrNotFound
	}
	if tenant == "" || out.TenantID != tenant {
		return Invoice{}, ErrForbidden
	}
	return clone(out), nil
}
func (s *Service) List(ctx context.Context, tenant string, filter Filter) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if tenant == "" {
		return Page{}, ErrForbidden
	}
	if filter.Limit < 1 || filter.Limit > 100 || filter.Offset < 0 {
		return Page{}, ErrInvalid
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.list(tenant, filter), nil
}
func (s *Service) list(tenant string, filter Filter) Page {
	items := []Invoice{}
	for _, item := range s.invoices {
		if item.TenantID == tenant && (filter.Currency == "" || filter.Currency == item.Currency) && (filter.Status == "" || filter.Status == item.Status) {
			items = append(items, clone(item))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	total := len(items)
	start := min(filter.Offset, total)
	end := min(start+filter.Limit, total)
	return Page{Items: items[start:end], Total: total, Limit: filter.Limit, Offset: filter.Offset}
}
func (s *Service) Update(ctx context.Context, tenant, id string, in UpdateInput) (Invoice, error) {
	if err := ctx.Err(); err != nil {
		return Invoice{}, err
	}
	if (in.Memo != nil && len(*in.Memo) > 2000) || in.AmountMinor <= 0 || in.AmountMinor > 100000000 || (in.Status != "draft" && in.Status != "paid") {
		return Invoice{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, ok := s.invoices[id]
	if !ok {
		return Invoice{}, ErrNotFound
	}
	if tenant == "" || out.TenantID != tenant {
		return Invoice{}, ErrForbidden
	}
	out.AmountMinor = in.AmountMinor
	out.Status = in.Status
	out.Memo = copyString(in.Memo)
	out.UpdatedAt = time.Now().UTC()
	s.invoices[id] = out
	s.publish(tenant, "updated", id, &out)
	return clone(out), nil
}
func (s *Service) Delete(ctx context.Context, tenant, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, ok := s.invoices[id]
	if !ok {
		return ErrNotFound
	}
	if tenant == "" || out.TenantID != tenant {
		return ErrForbidden
	}
	delete(s.invoices, id)
	s.publish(tenant, "deleted", id, nil)
	return nil
}
func (s *Service) Attach(ctx context.Context, tenant, id string, file Attachment) (Invoice, error) {
	if err := ctx.Err(); err != nil {
		return Invoice{}, err
	}
	if file.Name == "" || file.Bytes < 0 || file.Bytes > 512<<10 || len(file.SHA256) != 64 {
		return Invoice{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, ok := s.invoices[id]
	if !ok {
		return Invoice{}, ErrNotFound
	}
	if tenant == "" || out.TenantID != tenant {
		return Invoice{}, ErrForbidden
	}
	out.Attachment = &file
	out.UpdatedAt = time.Now().UTC()
	s.invoices[id] = out
	s.publish(tenant, "attachment", id, &out)
	return clone(out), nil
}

// Subscribe obtains the initial snapshot and subscribes under one lock, so changes
// cannot disappear between the snapshot and live feed. Call cancel when finished.
func (s *Service) Subscribe(tenant string) (Page, <-chan Change, func()) {
	s.mu.Lock()
	sub := &subscription{tenant: tenant, changes: make(chan Change, 16)}
	s.subscribers[sub] = struct{}{}
	snapshot := s.list(tenant, Filter{Limit: 100})
	s.mu.Unlock()
	var once sync.Once
	return snapshot, sub.changes, func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, ok := s.subscribers[sub]; ok {
				delete(s.subscribers, sub)
				close(sub.changes)
			}
		})
	}
}
func (s *Service) publish(tenant, kind, id string, invoice *Invoice) {
	s.sequence[tenant]++
	event := Change{Sequence: s.sequence[tenant], Kind: kind, InvoiceID: id}
	if invoice != nil {
		copy := clone(*invoice)
		event.Invoice = &copy
	}
	for sub := range s.subscribers {
		if sub.tenant != tenant {
			continue
		}
		select {
		case sub.changes <- cloneChange(event):
		default:
			delete(s.subscribers, sub)
			close(sub.changes)
		}
	}
}
func copyString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func clone(in Invoice) Invoice {
	in.Memo = copyString(in.Memo)
	if in.Attachment != nil {
		copy := *in.Attachment
		in.Attachment = &copy
	}
	return in
}

func cloneChange(in Change) Change {
	if in.Invoice != nil {
		copied := clone(*in.Invoice)
		in.Invoice = &copied
	}
	return in
}

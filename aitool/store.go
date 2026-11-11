package aitool

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/mujhtech/trest/operation"
)

// State is persisted before and after an execution attempt. Uncertain is terminal
// for automatic execution: an operator must reconcile external effects manually.
type State string

const (
	Pending   State = "pending"
	Approved  State = "approved"
	Running   State = "running"
	Succeeded State = "succeeded"
	Rejected  State = "rejected"
	Uncertain State = "uncertain"
)

// Record is a versioned approval or model-call receipt. Arguments and results can
// contain application data; secure the store and apply a retention policy.
type Record struct {
	Version           int                 `json:"version"`
	ID                string              `json:"id"`
	Kind              string              `json:"kind"`
	RunID             string              `json:"run_id"`
	Tool              string              `json:"tool,omitempty"`
	OperationID       string              `json:"operation_id,omitempty"`
	Principal         operation.Principal `json:"principal"`
	Arguments         json.RawMessage     `json:"arguments,omitempty"`
	Digest            string              `json:"digest,omitempty"`
	ExpiresAt         time.Time           `json:"expires_at,omitempty"`
	State             State               `json:"state"`
	ApprovedBy        operation.Principal `json:"approved_by,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	UpdatedAt         time.Time           `json:"updated_at"`
	Result            json.RawMessage     `json:"result,omitempty"`
	Failure           *Failure            `json:"failure,omitempty"`
	EstimatedCost     int64               `json:"estimated_cost_micros,omitempty"`
	ActualCost        int64               `json:"actual_cost_micros,omitempty"`
	ProviderRequestID string              `json:"provider_request_id,omitempty"`
}

var (
	ErrRecordNotFound = errors.New("aitool: record not found")
	ErrRecordExists   = errors.New("aitool: record already exists")
)

// Store must atomically persist Create and Update before returning success.
// Update serializes competing claims. Implementations must return detached data
// and must not call fn after returning. Store access is trusted application access.
type Store interface {
	Create(Record) error
	Load(string) (Record, error)
	Update(string, func(*Record) error) error
}

// MemoryStore is for tests and ephemeral development only. Use FileStore or an
// application durable Store when approvals or model receipts must survive restart.
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]Record
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{records: make(map[string]Record)} }

func cloneRecord(r Record) Record {
	r.Arguments = append(json.RawMessage(nil), r.Arguments...)
	r.Result = append(json.RawMessage(nil), r.Result...)
	if r.Failure != nil {
		f := *r.Failure
		r.Failure = &f
	}
	return r
}

func (s *MemoryStore) Create(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[r.ID]; ok {
		return ErrRecordExists
	}
	s.records[r.ID] = cloneRecord(r)
	return nil
}
func (s *MemoryStore) Load(id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return Record{}, ErrRecordNotFound
	}
	return cloneRecord(r), nil
}
func (s *MemoryStore) Update(id string, fn func(*Record) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return ErrRecordNotFound
	}
	r = cloneRecord(r)
	if err := fn(&r); err != nil {
		return err
	}
	if r.ID != id {
		return errors.New("aitool: update cannot change record identity")
	}
	s.records[id] = cloneRecord(r)
	return nil
}

package aitool

import (
	"context"
	"errors"
	"math"

	"trest.local/trest/operation"
)

// Usage reports admission counts, estimates, and reconciled actual usage. Model
// receipts survive restart in the configured Store; these session totals are
// per-run and cannot be reset by a model tool call.
type Usage struct {
	ToolCalls     int   `json:"tool_calls"`
	ModelTurns    int   `json:"model_turns"`
	EstimatedCost int64 `json:"estimated_cost_micros"`
	ActualCost    int64 `json:"actual_cost_micros"`
	CommittedCost int64 `json:"committed_cost_micros"`
}

func (s *Session) Usage() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Usage{ToolCalls: s.calls, ModelTurns: s.modelTurns, EstimatedCost: s.estimatedCost, ActualCost: s.actualCost, CommittedCost: s.estimatedCost + s.overrunCost}
}

// ReserveModelCall must run before provider acceptance. The application passes a
// conservative estimate in its chosen currency's millionths. A running receipt
// is durably written first. No provider SDK is imported or called by this package.
// A failed/uncertain provider call still consumes its turn and estimated budget.
func (s *Session) ReserveModelCall(ctx context.Context, estimate int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.ctx.Err(); err != nil {
		return "", err
	}
	if estimate < 0 {
		return "", operation.NewError(operation.InvalidInput, "estimated cost must not be negative", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	committed := s.estimatedCost + s.overrunCost
	if s.modelTurns >= s.limits.MaxModelTurns || estimate > s.limits.MaxEstimatedCost-committed {
		return "", limitError("model turn or estimated cost budget exhausted")
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	now := s.catalog.now().UTC()
	r := Record{Version: 1, ID: id, Kind: "model_call", RunID: s.id, Principal: s.principal, State: Running, CreatedAt: now, UpdatedAt: now, EstimatedCost: estimate}
	if err := s.catalog.store.Create(r); err != nil {
		return "", err
	}
	s.modelTurns++
	s.estimatedCost += estimate
	return id, nil
}

// CompleteModelCall reconciles provider-reported usage and request identity. This
// is application-only; model-produced usage must never be trusted. Estimates do
// not constrain actual billing, and actual cost may exceed the admission budget.
func (s *Session) CompleteModelCall(id, providerRequestID string, actualCost int64) error {
	if actualCost < 0 {
		return errors.New("aitool: actual cost must not be negative")
	}
	if len(providerRequestID) > 256 {
		return errors.New("aitool: provider request id exceeds 256 bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if actualCost > math.MaxInt64-s.actualCost {
		return errors.New("aitool: usage total overflow")
	}
	var extra int64
	if err := s.catalog.store.Update(id, func(r *Record) error {
		if r.Kind != "model_call" || r.RunID != s.id || r.Principal != s.principal || (r.State != Running && r.State != Uncertain) {
			return operation.NewError(operation.Conflict, "model receipt is not reconcilable in this session", nil)
		}
		extra = max(actualCost-r.EstimatedCost, 0)
		if extra > math.MaxInt64-s.estimatedCost-s.overrunCost {
			return errors.New("aitool: committed usage total overflow")
		}
		r.State = Succeeded
		r.ProviderRequestID = providerRequestID
		r.ActualCost = actualCost
		r.UpdatedAt = s.catalog.now().UTC()
		r.Failure = nil
		return nil
	}); err != nil {
		return err
	}
	s.actualCost += actualCost
	s.overrunCost += extra
	return nil
}

// MarkModelUncertain records loss of the provider outcome after acceptance.
// It never retries, refunds a turn, or assumes no charge was incurred.
func (s *Session) MarkModelUncertain(id, providerRequestID string) error {
	if len(providerRequestID) > 256 {
		return errors.New("aitool: provider request id exceeds 256 bytes")
	}
	return s.catalog.store.Update(id, func(r *Record) error {
		if r.Kind != "model_call" || r.RunID != s.id || r.Principal != s.principal || r.State != Running {
			return operation.NewError(operation.Conflict, "model receipt is not running in this session", nil)
		}
		r.State = Uncertain
		r.ProviderRequestID = providerRequestID
		r.UpdatedAt = s.catalog.now().UTC()
		r.Failure = &Failure{Code: "unavailable", Message: "provider outcome is unknown; reconcile usage before explicit retry"}
		return nil
	})
}

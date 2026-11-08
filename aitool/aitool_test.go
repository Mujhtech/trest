package aitool

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"trest.local/trest/operation"
)

type input struct {
	Resource string `json:"resource"`
	Amount   int    `json:"amount"`
}
type output struct {
	Tenant string `json:"tenant"`
	Amount int    `json:"amount"`
}

func principalContext() context.Context {
	return operation.WithPrincipal(context.Background(), operation.Principal{ID: "alice", TenantID: "tenant-a"})
}
func fixture(t *testing.T, store Store, approval bool, handler func(context.Context, input) (output, error), allowed *atomic.Bool) (*Catalog, *Session) {
	t.Helper()
	if allowed == nil {
		allowed = &atomic.Bool{}
		allowed.Store(true)
	}
	r := operation.NewRegistry(operation.AuthorizerFunc(func(ctx context.Context, p operation.Principal, permission string, v any) error {
		if !allowed.Load() || p.ID != "alice" || p.TenantID != "tenant-a" || v.(input).Resource != "owned" {
			return errors.New("private policy details")
		}
		return nil
	}))
	_, err := operation.Register(r, operation.Definition[input, output]{ID: "test.write.v1", Summary: "Write resource", Access: operation.Permission("write"), SideEffect: "write", Validate: func(ctx context.Context, in input) error {
		if in.Amount <= 0 {
			return operation.NewError(operation.InvalidInput, "amount must be positive", nil)
		}
		return nil
	}}, handler)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
	c, err := New(r, []Tool{{Name: "write_resource", OperationID: "test.write.v1", RequireApproval: approval}}, store, func(ctx context.Context, p operation.Principal, r Record) error {
		if p.ID != "approver" {
			return errors.New("not approver")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.NewSession(principalContext(), []string{"write_resource"}, Limits{MaxCalls: 32, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return c, s
}
func approve(t *testing.T, c *Catalog, pending Result) {
	t.Helper()
	ctx := operation.WithPrincipal(context.Background(), operation.Principal{ID: "approver", TenantID: "tenant-a"})
	if err := c.Approve(ctx, pending.ApprovalID, pending.Digest); err != nil {
		t.Fatal(err)
	}
}

func TestCallAllowlistAndIdentity(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	c, s := fixture(t, NewMemoryStore(), false, func(ctx context.Context, in input) (output, error) {
		calls.Add(1)
		p, _ := operation.PrincipalFromContext(ctx)
		return output{p.TenantID, in.Amount}, nil
	}, nil)
	if r := s.Call(context.Background(), "hidden", json.RawMessage(`{}`)); r.Failure == nil || r.Failure.Code != "forbidden" {
		t.Fatalf("hidden tool accepted: %+v", r)
	}
	for _, args := range []string{`{"resource":"other","amount":1}`, `{"resource":"owned","amount":0}`, `{"resource":"owned","amount":1,"tenant_id":"victim"}`} {
		if r := s.Call(context.Background(), "write_resource", json.RawMessage(args)); r.Status == Succeeded {
			t.Fatalf("invalid input executed: %+v", r)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("denied tool invoked handler")
	}
	ctx := operation.WithPrincipal(context.Background(), operation.Principal{ID: "mallory", TenantID: "victim"})
	r := s.Call(ctx, "write_resource", json.RawMessage(`{"resource":"owned","amount":3}`))
	if r.Status != Succeeded || !strings.Contains(string(r.Data), "tenant-a") {
		t.Fatalf("session identity lost: %+v", r)
	}
	empty, err := c.NewSession(principalContext(), nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if len(empty.Tools()) != 0 {
		t.Fatal("empty allowlist exposed tools")
	}
	tools := s.Tools()
	tools[0].InputSchema[0] = '!'
	if !json.Valid(s.Tools()[0].InputSchema) {
		t.Fatal("tool metadata mutated catalog")
	}
}

func TestApprovalLifecycleAndRevocation(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	var calls atomic.Int64
	allowed := &atomic.Bool{}
	allowed.Store(true)
	c, s := fixture(t, store, true, func(ctx context.Context, in input) (output, error) {
		calls.Add(1)
		return output{"tenant-a", in.Amount}, nil
	}, allowed)
	pending := s.Call(context.Background(), "write_resource", json.RawMessage(`{ "amount":3, "resource":"owned" }`))
	if pending.Status != Pending || calls.Load() != 0 {
		t.Fatalf("approval did not pause: %+v", pending)
	}
	r, err := c.Approval(principalContext(), pending.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Arguments) != `{"resource":"owned","amount":3}` {
		t.Fatalf("arguments not canonical: %s", r.Arguments)
	}
	if err := c.Approve(principalContext(), r.ID, r.Digest); operation.CodeOf(err) != operation.Forbidden {
		t.Fatalf("actor self-approved: %v", err)
	}
	approve(t, c, pending)
	allowed.Store(false)
	if got := s.ExecuteApproval(context.Background(), r.ID, r.Digest); got.Status == Succeeded || calls.Load() != 0 {
		t.Fatalf("revocation was bypassed: %+v", got)
	}
	allowed.Store(true)
	if got := s.ExecuteApproval(context.Background(), r.ID, "tampered"); got.Status == Succeeded {
		t.Fatal("wrong digest accepted")
	}
	if got := s.ExecuteApproval(context.Background(), r.ID, r.Digest); got.Status != Succeeded || calls.Load() != 1 {
		t.Fatalf("approved execution failed: %+v", got)
	}
	if got := s.ExecuteApproval(context.Background(), r.ID, r.Digest); got.Status == Succeeded || calls.Load() != 1 {
		t.Fatal("approval replay executed")
	}
	final, err := store.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != Succeeded || len(final.Result) == 0 || final.ApprovedBy.ID != "approver" {
		t.Fatalf("missing receipt: %+v", final)
	}
}

func TestApprovalTamperingExpiryAndActor(t *testing.T) {
	t.Parallel()
	for _, tc := range []string{"arguments", "actor", "operation", "expiry", "expired", "different caller"} {
		t.Run(tc, func(t *testing.T) {
			store := NewMemoryStore()
			var calls atomic.Int64
			c, s := fixture(t, store, true, func(context.Context, input) (output, error) { calls.Add(1); return output{}, nil }, nil)
			pending := s.Call(context.Background(), "write_resource", json.RawMessage(`{"resource":"owned","amount":2}`))
			approve(t, c, pending)
			switch tc {
			case "different caller":
				ctx := operation.WithPrincipal(context.Background(), operation.Principal{ID: "alice", TenantID: "other"})
				other, err := c.NewSession(ctx, []string{"write_resource"}, Limits{})
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				s = other
			case "expired":
				c.now = func() time.Time { return time.Now().Add(time.Hour) }
			default:
				if err := store.Update(pending.ApprovalID, func(r *Record) error {
					switch tc {
					case "arguments":
						r.Arguments = json.RawMessage(`{"resource":"owned","amount":99}`)
					case "actor":
						r.Principal.ID = "mallory"
					case "operation":
						r.OperationID = "other"
					case "expiry":
						r.ExpiresAt = r.ExpiresAt.Add(time.Hour)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			got := s.ExecuteApproval(context.Background(), pending.ApprovalID, pending.Digest)
			if got.Status == Succeeded || calls.Load() != 0 {
				t.Fatalf("tampered/expired scope executed: %+v", got)
			}
		})
	}
}

func TestApprovalConcurrentClaim(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	var calls atomic.Int64
	c, s := fixture(t, store, true, func(context.Context, input) (output, error) { calls.Add(1); return output{"tenant-a", 1}, nil }, nil)
	pending := s.Call(context.Background(), "write_resource", json.RawMessage(`{"resource":"owned","amount":1}`))
	approve(t, c, pending)
	var wg sync.WaitGroup
	var successes atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, err := c.NewSession(principalContext(), []string{"write_resource"}, Limits{})
			if err != nil {
				t.Error(err)
				return
			}
			defer other.Close()
			if r := other.ExecuteApproval(context.Background(), pending.ApprovalID, pending.Digest); r.Status == Succeeded {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 || successes.Load() != 1 {
		t.Fatalf("claims executed %d times with %d successes", calls.Load(), successes.Load())
	}
}

func TestApprovalPanicIsUncertain(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	c, s := fixture(t, store, true, func(context.Context, input) (output, error) { panic("secret after effect") }, nil)
	pending := s.Call(context.Background(), "write_resource", json.RawMessage(`{"resource":"owned","amount":1}`))
	approve(t, c, pending)
	got := s.ExecuteApproval(context.Background(), pending.ApprovalID, pending.Digest)
	if got.Status != Uncertain || strings.Contains(got.Failure.Message, "secret") {
		t.Fatalf("panic not redacted: %+v", got)
	}
	r, err := store.Load(pending.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != Uncertain {
		t.Fatalf("uncertainty not persisted: %+v", r)
	}
}

func TestSessionBudgetsAndConcurrency(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	c, _ := fixture(t, NewMemoryStore(), false, func(ctx context.Context, in input) (output, error) {
		started <- struct{}{}
		select {
		case <-release:
			return output{}, nil
		case <-ctx.Done():
			return output{}, ctx.Err()
		}
	}, nil)
	s, err := c.NewSession(principalContext(), []string{"write_resource"}, Limits{MaxCalls: 3, MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan Result, 1)
	go func() {
		done <- s.Call(context.Background(), "write_resource", json.RawMessage(`{"resource":"owned","amount":1}`))
	}()
	<-started
	if r := s.Call(context.Background(), "write_resource", json.RawMessage(`{"resource":"owned","amount":1}`)); r.Failure == nil || r.Failure.Code != "rate_limited" {
		t.Fatalf("parallel write admitted: %+v", r)
	}
	close(release)
	<-done
	if r := s.Call(context.Background(), "write_resource", json.RawMessage(`{"resource":"owned","amount":1}`)); r.Status != Succeeded {
		t.Fatalf("third attempt failed: %+v", r)
	}
	if r := s.Call(context.Background(), "write_resource", json.RawMessage(`{"resource":"owned","amount":1}`)); r.Failure == nil || r.Failure.Code != "rate_limited" {
		t.Fatal("call budget exceeded")
	}
	s.Close()
	if r := s.Call(context.Background(), "write_resource", json.RawMessage(`{}`)); r.Failure == nil || r.Failure.Code != "canceled" {
		t.Fatal("closed session executed")
	}
}

func TestModelReceiptsAndCostBudget(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	c, _ := fixture(t, store, false, func(context.Context, input) (output, error) { return output{}, nil }, nil)
	s, err := c.NewSession(principalContext(), nil, Limits{MaxModelTurns: 2, MaxEstimatedCost: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.ReserveModelCall(context.Background(), 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveModelCall(context.Background(), 41); err == nil {
		t.Fatal("cost budget exceeded")
	}
	if err := s.MarkModelUncertain(id, "provider-123"); err != nil {
		t.Fatal(err)
	}
	r, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != Uncertain {
		t.Fatal("lost response not recorded")
	}
	if err := s.CompleteModelCall(id, "provider-123", 120); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveModelCall(context.Background(), 0); err == nil {
		t.Fatal("over-budget actual usage admitted more calls")
	}
	if err := s.CompleteModelCall(id, "provider-123", 120); err == nil {
		t.Fatal("usage reconciled twice")
	}
	usage := s.Usage()
	if usage.ModelTurns != 1 || usage.EstimatedCost != 60 || usage.ActualCost != 120 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestFileStoreRestartAndLock(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "receipts")
	store, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := OpenFileStore(dir); err == nil {
		_ = duplicate.Close()
		t.Fatal("concurrent store ownership permitted")
	}
	c, s := fixture(t, store, true, func(context.Context, input) (output, error) { return output{}, nil }, nil)
	pending := s.Call(context.Background(), "write_resource", json.RawMessage(`{"resource":"owned","amount":1}`))
	approve(t, c, pending)
	if err := store.Update(pending.ApprovalID, func(r *Record) error { r.State = Running; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	r, err := reopened.Load(pending.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != Uncertain || r.Digest != pending.Digest {
		t.Fatalf("restart did not preserve uncertainty: %+v", r)
	}
	if _, err := reopened.Load("../outside"); err == nil {
		t.Fatal("path traversal accepted")
	}
	info, err := os.Stat(filepath.Join(dir, pending.ApprovalID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("record permissions %v", info.Mode().Perm())
	}
}

func TestCallExecutionErrorsRemainUncertain(t *testing.T) {
	t.Parallel()
	for _, tc := range []string{"error after effect", "cancellation after effect"} {
		t.Run(tc, func(t *testing.T) {
			var calls atomic.Int64
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, s := fixture(t, NewMemoryStore(), false, func(context.Context, input) (output, error) {
				calls.Add(1)
				if tc == "cancellation after effect" {
					cancel()
					return output{}, context.Canceled
				}
				return output{}, errors.New("private failure after commit")
			}, nil)
			got := s.Call(ctx, "write_resource", json.RawMessage(`{"resource":"owned","amount":1}`))
			if got.Status != Uncertain || calls.Load() != 1 {
				t.Fatalf("effect uncertainty lost: %+v", got)
			}
			if strings.Contains(got.Failure.Message, "private") {
				t.Fatal("private error leaked")
			}
		})
	}
}

func TestSessionTimeAndModelTurnLimits(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t, NewMemoryStore(), false, func(ctx context.Context, in input) (output, error) { <-ctx.Done(); return output{}, ctx.Err() }, nil)
	s, err := c.NewSession(principalContext(), []string{"write_resource"}, Limits{Timeout: 20 * time.Millisecond, MaxModelTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ReserveModelCall(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveModelCall(context.Background(), 0); err == nil {
		t.Fatal("model turn limit not enforced")
	}
	got := s.Call(context.Background(), "write_resource", json.RawMessage(`{"resource":"owned","amount":1}`))
	if got.Failure == nil || got.Failure.Code != "deadline_exceeded" {
		t.Fatalf("wall deadline not propagated: %+v", got)
	}
}

func TestModelBudgetIncludesOutstandingAndOverruns(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t, NewMemoryStore(), false, func(context.Context, input) (output, error) { return output{}, nil }, nil)
	s, err := c.NewSession(principalContext(), nil, Limits{MaxEstimatedCost: 300})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.ReserveModelCall(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveModelCall(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteModelCall(first, "provider", 200); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveModelCall(context.Background(), 1); err == nil {
		t.Fatal("overrun plus outstanding reservation was undercounted")
	}
	if s.Usage().CommittedCost != 300 {
		t.Fatalf("wrong committed usage: %+v", s.Usage())
	}
}

func TestWholeResultOutputBound(t *testing.T) {
	t.Parallel()
	for _, tc := range []string{"escaped error", "success envelope"} {
		t.Run(tc, func(t *testing.T) {
			c, _ := fixture(t, NewMemoryStore(), false, func(context.Context, input) (output, error) {
				if tc == "escaped error" {
					return output{}, operation.NewError(operation.Conflict, strings.Repeat("\x01", 160), nil)
				}
				return output{Tenant: strings.Repeat("<", 100), Amount: 1}, nil
			}, nil)
			s, err := c.NewSession(principalContext(), []string{"write_resource"}, Limits{MaxOutputBytes: 256})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got := s.Call(context.Background(), "write_resource", json.RawMessage(`{"resource":"owned","amount":1}`))
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > 256 {
				t.Fatalf("whole result exceeds bound: %d", len(encoded))
			}
			if got.Status != Uncertain {
				t.Fatalf("discarded execution outcome lost uncertainty: %+v", got)
			}
		})
	}
}

func TestModelBudgetOverflow(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	c, _ := fixture(t, store, false, func(context.Context, input) (output, error) { return output{}, nil }, nil)
	s, err := c.NewSession(principalContext(), nil, Limits{MaxEstimatedCost: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.ReserveModelCall(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveModelCall(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteModelCall(first, "provider", math.MaxInt64); err == nil {
		t.Fatal("overflow was accepted")
	}
	receipt, err := store.Load(first)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != Running || s.Usage().ActualCost != 0 || s.Usage().CommittedCost != 2 {
		t.Fatal("failed reconciliation changed usage or receipt")
	}
}

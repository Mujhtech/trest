// Package aitool exposes an explicit allowlist of operations to model tools.
// Trusted application code creates sessions after authenticating the actor.
// Model arguments never establish identity, grant permissions, or approve work.
package aitool

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"trest.local/trest/operation"
)

// Tool is an explicit exposure declaration. Side effects are derived from the
// operation metadata: only SideEffect="read" permits parallel calls by default.
type Tool struct {
	Name            string
	OperationID     string
	Description     string
	RequireApproval bool
	ApprovalTTL     time.Duration
}

// Descriptor is safe-to-publish tool metadata, without credentials or identity.
type Descriptor struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	InputSchema     json.RawMessage `json:"input_schema"`
	RequireApproval bool            `json:"requires_approval"`
}

// Limits apply to one session. Costs are estimated millionths of the
// application's chosen billing currency, not a billing guarantee. Actual usage
// can exceed an estimate and prevents subsequent admission when over budget.
type Limits struct {
	MaxCalls            int
	MaxModelTurns       int
	MaxEstimatedCost    int64
	Timeout             time.Duration
	MaxConcurrent       int
	MaxInputBytes       int
	MaxOutputBytes      int
	AllowParallelWrites bool
}

// ApprovalPolicy is called only by the trusted application's approval endpoint.
// It must authorize the approver and confirm the exact digest shown to them.
type ApprovalPolicy func(context.Context, operation.Principal, Record) error

// Catalog is immutable after construction. No tool is exposed implicitly.
type Catalog struct {
	registry *operation.Registry
	store    Store
	tools    map[string]Tool
	policy   ApprovalPolicy
	now      func() time.Time
}

var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// New validates the allowlist. store is required for approval and model-call
// receipts. A nil policy disables approval grants, even when tools request one.
func New(registry *operation.Registry, tools []Tool, store Store, policy ApprovalPolicy) (*Catalog, error) {
	if registry == nil || !registry.IsFrozen() {
		return nil, errors.New("aitool: registry must be frozen")
	}
	if store == nil {
		return nil, errors.New("aitool: supply an explicit approval and model receipt store")
	}
	c := &Catalog{registry: registry, store: store, tools: make(map[string]Tool), policy: policy, now: time.Now}
	for _, t := range tools {
		if !toolName.MatchString(t.Name) {
			return nil, fmt.Errorf("aitool: invalid tool name %q", t.Name)
		}
		if _, ok := c.tools[t.Name]; ok {
			return nil, fmt.Errorf("aitool: duplicate tool %q", t.Name)
		}
		m, ok := registry.Lookup(t.OperationID)
		if !ok {
			return nil, fmt.Errorf("aitool: unknown operation %q", t.OperationID)
		}
		if m.JSONDisabled {
			return nil, fmt.Errorf("aitool: operation %q requires native invocation", t.OperationID)
		}
		if t.Description == "" {
			t.Description = m.Summary
		}
		if t.ApprovalTTL == 0 {
			t.ApprovalTTL = 5 * time.Minute
		}
		if t.ApprovalTTL < 0 || t.ApprovalTTL > 24*time.Hour {
			return nil, errors.New("aitool: approval ttl must be positive and at most 24 hours")
		}
		c.tools[t.Name] = t
	}
	return c, nil
}

// Session binds immutable trusted identity and a per-run subset of the catalog.
// Close it to release its deadline timer. Recreating sessions resets budgets;
// the application must prevent clients from minting unlimited sessions.
type Session struct {
	catalog       *Catalog
	principal     operation.Principal
	allowed       map[string]bool
	limits        Limits
	id            string
	ctx           context.Context
	cancel        context.CancelFunc
	slots         chan struct{}
	writes        chan struct{}
	mu            sync.Mutex
	calls         int
	modelTurns    int
	estimatedCost int64
	actualCost    int64
	overrunCost   int64
}

// NewSession captures identity from trusted context. Anonymous sessions are
// allowed for public tools but cannot request or execute approvals.
func (c *Catalog) NewSession(ctx context.Context, allowlist []string, limits Limits) (*Session, error) {
	if ctx == nil {
		return nil, errors.New("aitool: nil session context")
	}
	if limits.MaxCalls == 0 {
		limits.MaxCalls = 8
	}
	if limits.MaxModelTurns == 0 {
		limits.MaxModelTurns = 8
	}
	if limits.MaxEstimatedCost == 0 {
		limits.MaxEstimatedCost = 1_000_000
	}
	if limits.Timeout == 0 {
		limits.Timeout = 30 * time.Second
	}
	if limits.MaxConcurrent == 0 {
		limits.MaxConcurrent = 1
	}
	if limits.MaxInputBytes == 0 {
		limits.MaxInputBytes = 1 << 20
	}
	if limits.MaxOutputBytes == 0 {
		limits.MaxOutputBytes = 64 << 10
	}
	if limits.MaxCalls < 1 || limits.MaxModelTurns < 1 || limits.MaxEstimatedCost < 1 || limits.Timeout < 0 || limits.MaxConcurrent < 1 || limits.MaxInputBytes < 1 || limits.MaxOutputBytes < 256 {
		return nil, errors.New("aitool: invalid session limits")
	}
	allowed := make(map[string]bool)
	for _, name := range allowlist {
		if _, ok := c.tools[name]; !ok {
			return nil, fmt.Errorf("aitool: unknown allowed tool %q", name)
		}
		allowed[name] = true
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	principal, _ := operation.PrincipalFromContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, limits.Timeout)
	return &Session{catalog: c, principal: principal, allowed: allowed, limits: limits, id: id, ctx: ctx, cancel: cancel, slots: make(chan struct{}, limits.MaxConcurrent), writes: make(chan struct{}, 1)}, nil
}
func (s *Session) Close()     { s.cancel() }
func (s *Session) ID() string { return s.id }

// Context carries the session deadline and trusted identity into provider calls.
func (s *Session) Context() context.Context { return s.ctx }

// Tools lists only this session's explicit allowlist, in deterministic order.
func (s *Session) Tools() []Descriptor {
	result := make([]Descriptor, 0, len(s.allowed))
	for name := range s.allowed {
		t := s.catalog.tools[name]
		m, _ := s.catalog.registry.Lookup(t.OperationID)
		result = append(result, Descriptor{Name: name, Description: t.Description, InputSchema: m.InputSchema, RequireApproval: t.RequireApproval})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// Failure is a bounded public error. Internal causes and stack traces are absent.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result distinguishes completed work from approval pauses. Data is a committed
// result only when Status is succeeded. Approval records contain exact input.
type Result struct {
	Status     State           `json:"status"`
	Data       json.RawMessage `json:"data,omitempty"`
	ApprovalID string          `json:"approval_id,omitempty"`
	Digest     string          `json:"digest,omitempty"`
	Failure    *Failure        `json:"error,omitempty"`
}

func failure(err error) *Failure {
	p := operation.PublicError(err)
	message := p.Message
	if len(message) > 160 {
		message = string([]rune(message)[:min(160, len([]rune(message)))])
	}
	return &Failure{Code: string(p.Code), Message: message}
}
func fail(err error) Result           { return Result{Status: Rejected, Failure: failure(err)} }
func limitError(message string) error { return operation.NewError(operation.RateLimited, message, nil) }

func (s *Session) enter(ctx context.Context, name string) (context.Context, func(), Tool, error) {
	if ctx == nil {
		return nil, nil, Tool{}, operation.NewError(operation.Internal, "nil call context", nil)
	}
	if err := s.ctx.Err(); err != nil {
		return nil, nil, Tool{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, Tool{}, err
	}
	if !s.allowed[name] {
		return nil, nil, Tool{}, operation.NewError(operation.Forbidden, "tool is not allowed in this session", nil)
	}
	t := s.catalog.tools[name]
	s.mu.Lock()
	if s.calls >= s.limits.MaxCalls {
		s.mu.Unlock()
		return nil, nil, Tool{}, limitError("tool call budget exhausted")
	}
	s.calls++
	s.mu.Unlock()
	select {
	case s.slots <- struct{}{}:
	default:
		return nil, nil, Tool{}, limitError("tool concurrency limit reached")
	}
	m, _ := s.catalog.registry.Lookup(t.OperationID)
	write := m.SideEffect != "read" && !s.limits.AllowParallelWrites
	if write {
		select {
		case s.writes <- struct{}{}:
		default:
			<-s.slots
			return nil, nil, Tool{}, limitError("parallel side-effecting tools are disabled")
		}
	}
	deadline, _ := s.ctx.Deadline()
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	// Both contexts share the same deadline. Let this context's deadline timer
	// preserve DeadlineExceeded; racing a generic cancel against it changes the
	// reported outcome nondeterministically to Canceled.
	stop := context.AfterFunc(s.ctx, func() {
		if s.ctx.Err() != context.DeadlineExceeded {
			cancel()
		}
	})
	callCtx = operation.WithPrincipal(callCtx, s.principal)
	release := func() {
		stop()
		cancel()
		if write {
			<-s.writes
		}
		<-s.slots
	}
	return callCtx, release, t, nil
}

// Call validates and authorizes through the shared invoker. Approval tools pause
// without calling their handlers. Returned public errors are bounded.
func (s *Session) Call(ctx context.Context, name string, args json.RawMessage) (result Result) {
	defer func() { result = boundResult(result, s.limits.MaxOutputBytes) }()
	ctx, release, t, err := s.enter(ctx, name)
	if err != nil {
		return fail(err)
	}
	defer release()
	defer func() {
		if recover() != nil {
			result = Result{Status: Uncertain, Failure: &Failure{Code: "internal", Message: "execution panicked; effects may have occurred"}}
		}
	}()
	if len(args) > s.limits.MaxInputBytes {
		return fail(operation.NewError(operation.InvalidInput, "tool input exceeds size limit", nil))
	}
	if t.RequireApproval {
		if s.principal.ID == "" {
			return fail(operation.NewError(operation.Unauthenticated, "approval requires an authenticated actor", nil))
		}
		canonical, err := s.catalog.registry.PrepareJSON(ctx, t.OperationID, args)
		if err != nil {
			return fail(err)
		}
		id, err := randomID()
		if err != nil {
			return fail(err)
		}
		now := s.catalog.now().UTC()
		r := Record{Version: 1, ID: id, Kind: "approval", RunID: s.id, Tool: name, OperationID: t.OperationID, Principal: s.principal, Arguments: canonical, ExpiresAt: now.Add(t.ApprovalTTL), State: Pending, CreatedAt: now, UpdatedAt: now}
		r.Digest = approvalDigest(r)
		if err := s.catalog.store.Create(r); err != nil {
			return fail(err)
		}
		return Result{Status: Pending, ApprovalID: id, Digest: r.Digest}
	}
	canonical, err := s.catalog.registry.PrepareJSON(ctx, t.OperationID, args)
	if err != nil {
		return fail(err)
	}
	data, err := s.catalog.registry.InvokeJSON(ctx, t.OperationID, canonical)
	if err != nil {
		// A handler can commit an effect before returning an error. Even a
		// deadline does not roll it back; never report safe automatic retry.
		return Result{Status: Uncertain, Failure: failure(err)}
	}
	if len(data) > s.limits.MaxOutputBytes {
		return Result{Status: Uncertain, Failure: &Failure{Code: "internal", Message: "tool output exceeds size limit; effects may have occurred"}}
	}
	return Result{Status: Succeeded, Data: data}
}

// Approval returns a record only to the same authenticated actor and tenant.
func (c *Catalog) Approval(ctx context.Context, id string) (Record, error) {
	r, err := c.store.Load(id)
	if err != nil {
		return Record{}, err
	}
	p, ok := operation.PrincipalFromContext(ctx)
	if !ok || p != r.Principal {
		return Record{}, operation.NewError(operation.Forbidden, "approval belongs to another actor", nil)
	}
	if r.Kind != "approval" || r.Digest != approvalDigest(r) {
		return Record{}, operation.NewError(operation.Conflict, "approval record failed integrity verification", nil)
	}
	return r, nil
}

// Approve is application-only. The exact digest must be shown to an authorized
// approver and passed back unchanged. Tools and model text cannot call this API.
func (c *Catalog) Approve(ctx context.Context, id, digest string) error {
	if c.policy == nil {
		return operation.NewError(operation.Forbidden, "approval policy is not configured", nil)
	}
	p, ok := operation.PrincipalFromContext(ctx)
	if !ok {
		return operation.NewError(operation.Unauthenticated, "approval requires authentication", nil)
	}
	r, err := c.store.Load(id)
	if err != nil {
		return err
	}
	if r.Kind != "approval" || digest != r.Digest || r.Digest != approvalDigest(r) {
		return operation.NewError(operation.Conflict, "approval digest does not match", nil)
	}
	if r.State != Pending || !c.now().Before(r.ExpiresAt) {
		return operation.NewError(operation.Conflict, "approval is not pending or has expired", nil)
	}
	if err := c.policy(ctx, p, cloneRecord(r)); err != nil {
		return operation.NewError(operation.Forbidden, "approval denied", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.store.Update(id, func(current *Record) error {
		if current.State != Pending || current.Digest != digest || approvalDigest(*current) != digest || !c.now().Before(current.ExpiresAt) {
			return operation.NewError(operation.Conflict, "approval changed or expired", nil)
		}
		current.State = Approved
		current.ApprovedBy = p
		current.UpdatedAt = c.now().UTC()
		return nil
	})
}

// ExecuteApproval atomically claims the exact approved record, revalidates and
// reauthorizes it, then invokes once. Every failure after the claim is uncertain:
// callers must reconcile effects rather than automatically replaying it.
func (s *Session) ExecuteApproval(ctx context.Context, id, digest string) (result Result) {
	defer func() { result = boundResult(result, s.limits.MaxOutputBytes) }()
	r, err := s.catalog.store.Load(id)
	if err != nil {
		return fail(err)
	}
	ctx, release, t, err := s.enter(ctx, r.Tool)
	if err != nil {
		return fail(err)
	}
	defer release()
	if !t.RequireApproval || r.Kind != "approval" || r.OperationID != t.OperationID || r.Principal != s.principal || s.principal.ID == "" || digest != r.Digest || approvalDigest(r) != digest {
		return fail(operation.NewError(operation.Forbidden, "approval scope or digest does not match", nil))
	}
	if r.State != Approved || !s.catalog.now().Before(r.ExpiresAt) {
		return fail(operation.NewError(operation.Conflict, "approval is not executable or has expired", nil))
	}
	canonical, err := s.catalog.registry.PrepareJSON(ctx, t.OperationID, r.Arguments)
	if err != nil {
		return fail(err)
	}
	if string(canonical) != string(r.Arguments) {
		return fail(operation.NewError(operation.Conflict, "validated arguments changed; request a new approval", nil))
	}
	err = s.catalog.store.Update(id, func(current *Record) error {
		if current.State != Approved || current.Digest != digest || approvalDigest(*current) != digest || current.Principal != s.principal || !s.catalog.now().Before(current.ExpiresAt) {
			return operation.NewError(operation.Conflict, "approval was claimed, changed, or expired", nil)
		}
		current.State = Running
		current.UpdatedAt = s.catalog.now().UTC()
		return nil
	})
	if err != nil {
		return fail(err)
	}
	result = Result{Status: Uncertain, Failure: &Failure{Code: "internal", Message: "execution interrupted; effects may have occurred"}}
	defer func() {
		if recover() != nil {
			result = Result{Status: Uncertain, Failure: &Failure{Code: "internal", Message: "execution panicked; effects may have occurred"}}
		}
		result = boundResult(result, s.limits.MaxOutputBytes)
		if err := s.catalog.store.Update(id, func(current *Record) error {
			if current.State != Running {
				return errors.New("aitool: execution claim changed")
			}
			current.State = result.Status
			current.Result = append(json.RawMessage(nil), result.Data...)
			current.Failure = result.Failure
			current.UpdatedAt = s.catalog.now().UTC()
			return nil
		}); err != nil {
			result = Result{Status: Uncertain, Failure: &Failure{Code: "internal", Message: "execution receipt was not persisted; reconcile before retrying"}}
		}
	}()
	data, err := s.catalog.registry.InvokeJSON(ctx, t.OperationID, r.Arguments)
	if err != nil {
		result = Result{Status: Uncertain, Failure: failure(err)}
		return result
	}
	if len(data) > s.limits.MaxOutputBytes {
		return result
	}
	result = Result{Status: Succeeded, Data: data}
	return result
}

func approvalDigest(r Record) string {
	payload := struct {
		Version             int
		ID, Tool, Operation string
		Principal           operation.Principal
		Arguments           json.RawMessage
		Expiry              time.Time
	}{r.Version, r.ID, r.Tool, r.OperationID, r.Principal, r.Arguments, r.ExpiresAt.UTC()}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Bound the entire JSON envelope, including escaping and pending/error fields.
func boundResult(result Result, maxBytes int) Result {
	encoded, err := json.Marshal(result)
	if err == nil && len(encoded) <= maxBytes {
		return result
	}
	state := result.Status
	if state == Succeeded {
		state = Uncertain
	}
	return Result{Status: state, Failure: &Failure{Code: "internal", Message: "tool result exceeds size limit; inspect trusted execution record"}}
}

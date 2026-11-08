package operation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// SourceLocation identifies the registration call site. File is a basename by
// default, so inspection does not reveal machine-specific absolute paths.
type SourceLocation struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
}

// TraceContext links nested invocations. Applications may carry a trusted trace
// context across asynchronous boundaries; it is never an authorization identity.
type TraceContext struct {
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
}
type traceKey struct{}
type correlationKey struct{}

// Store immutable metadata in the context itself. context.WithValue would also
// allocate an interface box for the trace or string. Returning the private
// wrapper from Value lets accessors copy the value without exposing a mutable
// *TraceContext to application context wrappers.
type traceContext struct {
	context.Context
	trace TraceContext
}

func (c *traceContext) Value(key any) any {
	if _, ok := key.(traceKey); ok {
		return c
	}
	return c.Context.Value(key)
}

type correlationContext struct {
	context.Context
	id string
}

func (c *correlationContext) Value(key any) any {
	if _, ok := key.(correlationKey); ok {
		return c
	}
	return c.Context.Value(key)
}

// WithTrace attaches an application-supplied trace parent. The next invocation
// creates a fresh child span. Do not use trace IDs as security credentials.
func WithTrace(ctx context.Context, trace TraceContext) context.Context {
	if ctx == nil {
		panic("cannot create context from nil parent")
	}
	return &traceContext{Context: ctx, trace: trace}
}

// TraceFromContext returns trace metadata for the current invocation.
func TraceFromContext(ctx context.Context) TraceContext {
	trace, _ := ctx.Value(traceKey{}).(*traceContext)
	if trace == nil {
		return TraceContext{}
	}
	return trace.trace
}

// WithCorrelationID attaches a transport correlation ID to emitted events.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		panic("cannot create context from nil parent")
	}
	return &correlationContext{Context: ctx, id: id}
}

// CorrelationID retrieves the transport correlation ID, if any.
func CorrelationID(ctx context.Context) string {
	value, _ := ctx.Value(correlationKey{}).(*correlationContext)
	if value == nil {
		return ""
	}
	return value.id
}

// Event records the completion of an invocation phase. It deliberately contains
// no payloads, error strings, credentials, or response data. IDs may still be
// sensitive: the application owns access, redaction, and retention policy.
type Event struct {
	TraceID       string        `json:"trace_id"`
	SpanID        string        `json:"span_id"`
	ParentSpanID  string        `json:"parent_span_id,omitempty"`
	OperationID   string        `json:"operation_id"`
	Phase         string        `json:"phase"`
	StartedAt     time.Time     `json:"started_at"`
	Duration      time.Duration `json:"duration_ns"`
	Code          Code          `json:"code,omitempty"`
	PrincipalID   string        `json:"principal_id,omitempty"`
	TenantID      string        `json:"tenant_id,omitempty"`
	CorrelationID string        `json:"correlation_id,omitempty"`
}

// Observer receives metadata-only events synchronously. It must be fast,
// concurrency-safe, and honor cancellation. Panics are isolated so telemetry
// cannot change application authorization or execution results.
type Observer interface{ Observe(context.Context, Event) }

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(context.Context, Event)

func (f ObserverFunc) Observe(ctx context.Context, event Event) { f(ctx, event) }

// WithObserver enables opt-in invocation observation. Nil disables observation.
func WithObserver(observer Observer) Option {
	return func(c *registryConfig) {
		c.observer = nil
		if !isNil(observer) {
			c.observer = observer
		}
	}
}

func newInvocationTrace(ctx context.Context) (context.Context, error) {
	parent := TraceFromContext(ctx)
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return ctx, err
	}
	id := hex.EncodeToString(bytes[:])
	traceID := parent.TraceID
	if traceID == "" {
		traceID = id
	}
	return WithTrace(ctx, TraceContext{TraceID: traceID, SpanID: id, ParentSpanID: parent.SpanID}), nil
}

// Phase clocks have no observable use when no observer is configured. Trace
// identity remains available to handlers independently of observer presence.
func (r *Registry) startPhase() time.Time {
	if r.observer != nil {
		return time.Now()
	}
	return time.Time{}
}

func (r *Registry) observe(ctx context.Context, id, phase string, start time.Time, code Code) {
	if r.observer == nil {
		return
	}
	trace := TraceFromContext(ctx)
	principal, _ := PrincipalFromContext(ctx)
	event := Event{TraceID: trace.TraceID, SpanID: trace.SpanID, ParentSpanID: trace.ParentSpanID,
		OperationID: id, Phase: phase, StartedAt: start.UTC(), Duration: time.Since(start), Code: code,
		PrincipalID: principal.ID, TenantID: principal.TenantID, CorrelationID: CorrelationID(ctx)}
	defer func() { _ = recover() }()
	r.observer.Observe(ctx, event)
}

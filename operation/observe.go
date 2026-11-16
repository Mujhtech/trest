package operation

import (
	"context"
	"time"

	"github.com/mujhtech/trest/internal/correlation"
	"github.com/mujhtech/trest/internal/invocationctx"
	"github.com/mujhtech/trest/internal/spanreserve"
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

// WithTrace attaches an application-supplied trace parent. The next invocation
// creates a fresh child span. Do not use trace IDs as security credentials.
func WithTrace(ctx context.Context, trace TraceContext) context.Context {
	return invocationctx.WithTrace(ctx, invocationctx.Trace(trace))
}

// TraceFromContext returns trace metadata for the current invocation.
func TraceFromContext(ctx context.Context) TraceContext {
	return TraceContext(invocationctx.TraceFrom(ctx))
}

// WithCorrelationID attaches a transport correlation ID to emitted events.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	value := correlation.New(ctx, id)
	return &value
}

// CorrelationID retrieves the transport correlation ID, if any.
func CorrelationID(ctx context.Context) string {
	return correlation.ID(ctx)
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

func (r *Registry) newInvocationTrace(ctx context.Context) (context.Context, error) {
	trace, err := r.nextTrace(ctx)
	if err != nil {
		return ctx, err
	}
	return WithTrace(ctx, trace), nil
}

func (r *Registry) nextTrace(ctx context.Context) (TraceContext, error) {
	parent := TraceFromContext(ctx)
	id := spanreserve.Take(ctx)
	if id == "" {
		var err error
		id, err = r.ids.ID()
		if err != nil {
			return TraceContext{}, err
		}
	}
	traceID := parent.TraceID
	if traceID == "" {
		traceID = id
	}
	return TraceContext{TraceID: traceID, SpanID: id, ParentSpanID: parent.SpanID}, nil
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

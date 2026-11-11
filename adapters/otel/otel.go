// Package trestotel connects Trest HTTP requests and completed operation phases
// to an explicitly supplied OpenTelemetry provider. It exports metadata only.
package trestotel

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/mujhtech/trest/operation"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Options supplies application-owned telemetry dependencies. Neither dependency
// is read from or installed into OpenTelemetry global state.
type Options struct {
	Provider trace.TracerProvider
	// Propagator is required. Use propagation.TraceContext{} for W3C trace
	// context, or an empty composite propagator to disable remote extraction.
	Propagator propagation.TextMapPropagator
}

// Instrumentation is safe for concurrent requests. The application owns the
// provider, exporters, sampling policy, and their explicit startup and shutdown.
type Instrumentation struct {
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

// New validates explicit dependencies and obtains a tracer without creating a
// provider, exporter, background worker, or network connection.
func New(options Options) (*Instrumentation, error) {
	if isNil(options.Provider) || isNil(options.Propagator) {
		return nil, errors.New("trestotel: provider and propagator are required")
	}
	tracer := options.Provider.Tracer("github.com/mujhtech/trest/adapters/otel")
	if isNil(tracer) {
		return nil, errors.New("trestotel: provider returned a nil tracer")
	}
	return &Instrumentation{tracer: tracer, propagator: options.Propagator}, nil
}

// Middleware records an HTTP server span. Install with trest.Use at app scope,
// alongside trest.Observe(i), to connect operation phase spans to each request.
// Only the matched route template, normalized method, and response status are
// recorded. Unmatched URLs never become span names or attributes.
func (i *Instrumentation) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := httpMethod(r.Method)
		route := routeTemplate(r.Pattern)
		attributes := []attribute.KeyValue{attribute.String("http.request.method", method)}
		if route != "" {
			attributes = append(attributes, attribute.String("http.route", route))
		}
		parent := i.propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := i.tracer.Start(parent, requestName(method, route),
			trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attributes...))
		request := r.WithContext(ctx)
		recorded := &responseWriter{ResponseWriter: w}
		defer func() {
			recovered := recover()
			// Wrapping an entire mux can discover its template only after dispatch.
			if matched := routeTemplate(request.Pattern); matched != "" {
				span.SetName(requestName(method, matched))
				span.SetAttributes(attribute.String("http.route", matched))
			}
			status := recorded.status
			if status == 0 && recovered == nil {
				status = http.StatusOK
			}
			// Recovery outside this middleware may map a panic to a response that
			// is not observable yet. Do not invent its eventual HTTP status.
			if status != 0 {
				span.SetAttributes(attribute.Int("http.response.status_code", status))
			}
			switch {
			case recovered != nil:
				markError(span, "panic")
			case recorded.failure != "":
				markError(span, recorded.failure)
			case ctx.Err() != nil:
				markError(span, string(operation.CodeOf(ctx.Err())))
			case status >= 500:
				markError(span, strconv.Itoa(status))
			}
			span.End()
			if recovered != nil {
				// Preserve normal application/server panic handling without exporting
				// the recovered value, which can contain credentials or payloads.
				panic(recovered)
			}
		}()
		next.ServeHTTP(recorded, request)
	})
}

// Observe exports a completed operation phase as a child of the active OTel
// context, with the recorded start and duration. Phase spans are siblings; the
// completion callback cannot make them active parents during earlier work.
// Identity, correlation IDs, internal trace IDs, payloads, and error text are
// omitted. Invalid event names or timing are ignored.
func (i *Instrumentation) Observe(ctx context.Context, event operation.Event) {
	if ctx == nil || event.StartedAt.IsZero() || event.Duration < 0 || !validOperation(event.OperationID) || !validPhase(event.Phase) {
		return
	}
	_, span := i.tracer.Start(ctx, "trest."+event.Phase+" "+event.OperationID,
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(event.StartedAt),
		trace.WithAttributes(attribute.String("trest.operation.id", event.OperationID), attribute.String("trest.operation.phase", event.Phase)))
	if event.Code != "" {
		code := errorCode(event.Code)
		span.SetAttributes(attribute.String("trest.error.code", code))
		markError(span, code)
	}
	span.End(trace.WithTimestamp(event.StartedAt.Add(event.Duration)))
}

func markError(span trace.Span, kind string) {
	span.SetStatus(codes.Error, "")
	span.SetAttributes(attribute.String("error.type", kind))
}

func errorCode(code operation.Code) string {
	switch code {
	case operation.InvalidInput, operation.Unauthenticated, operation.Forbidden,
		operation.NotFound, operation.Conflict, operation.RateLimited,
		operation.Unavailable, operation.Internal, operation.DeadlineExceeded, operation.Canceled:
		return string(code)
	default:
		return string(operation.Internal)
	}
}

func validPhase(phase string) bool {
	switch phase {
	case "invoke", "prepare", "validate", "resolve", "authorize", "handler", "json_prepare", "json_invoke":
		return true
	default:
		return false
	}
}

func validOperation(id string) bool {
	if len(id) == 0 || len(id) > 256 || id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for _, char := range id {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func httpMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH":
		return method
	default:
		return "_OTHER"
	}
}

func routeTemplate(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		pattern = path
	}
	if !strings.HasPrefix(pattern, "/") || len(pattern) > 2048 || strings.ContainsAny(pattern, "?# \t\r\n") {
		return ""
	}
	return strings.TrimSuffix(pattern, "{$}")
}

func requestName(method, route string) string {
	if route == "" {
		return method
	}
	return method + " " + route
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

type responseWriter struct {
	http.ResponseWriter
	status  int
	failure string
}

func (w *responseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.ResponseWriter.WriteHeader(status)
		w.status = status
	}
}

func (w *responseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	if err != nil {
		w.failure = "write_error"
	}
	return n, err
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) Flush() { _ = w.FlushError() }

func (w *responseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil {
		w.failure = "flush_error"
	}
	return err
}

func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	connection, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		w.failure = "hijack_error"
	} else if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return connection, buffer, err
}

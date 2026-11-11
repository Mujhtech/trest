package trestotel_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"trest.local/trest"
	trestotel "trest.local/trest/adapters/otel"
	"trest.local/trest/operation"
)

func instrumentation(t *testing.T) (*trestotel.Instrumentation, *tracetest.InMemoryExporter, *sdktrace.TracerProvider) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	i, err := trestotel.New(trestotel.Options{Provider: provider, Propagator: propagation.TraceContext{}})
	if err != nil {
		t.Fatal(err)
	}
	return i, exporter, provider
}

func attributeValue(span tracetest.SpanStub, key string) attribute.Value {
	for _, item := range span.Attributes {
		if string(item.Key) == key {
			return item.Value
		}
	}
	return attribute.Value{}
}

func serverSpan(t *testing.T, spans tracetest.SpanStubs) tracetest.SpanStub {
	t.Helper()
	var servers []tracetest.SpanStub
	for _, span := range spans {
		if span.SpanKind == trace.SpanKindServer {
			servers = append(servers, span)
		}
	}
	if len(servers) != 1 {
		t.Fatalf("server spans=%d, all=%+v", len(servers), spans)
	}
	return servers[0]
}

func assertNoSecrets(t *testing.T, spans tracetest.SpanStubs, secrets ...string) {
	t.Helper()
	for _, span := range spans {
		// Provider resource configuration is application-owned; inspect everything
		// emitted by this adapter: names, attributes, events, and status text.
		data, err := json.Marshal(struct {
			Name       string
			Attributes []attribute.KeyValue
			Events     []sdktrace.Event
			Status     sdktrace.Status
		}{span.Name, span.Attributes, span.Events, span.Status})
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(data), secret) {
				t.Fatalf("private value %q appeared in span %s", secret, span.Name)
			}
		}
	}
}

func TestNewRequiresExplicitDependencies(t *testing.T) {
	t.Parallel()
	var nilProvider *sdktrace.TracerProvider
	for _, tc := range []struct {
		name    string
		options trestotel.Options
	}{
		{"missing dependencies", trestotel.Options{}},
		{"missing propagator", trestotel.Options{Provider: noop.NewTracerProvider()}},
		{"missing provider", trestotel.Options{Propagator: propagation.TraceContext{}}},
		{"typed nil provider", trestotel.Options{Provider: nilProvider, Propagator: propagation.TraceContext{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := trestotel.New(tc.options); err == nil {
				t.Fatal("missing dependency accepted")
			}
		})
	}
	if _, err := trestotel.New(trestotel.Options{Provider: noop.NewTracerProvider(), Propagator: propagation.NewCompositeTextMapPropagator()}); err != nil {
		t.Fatal(err)
	}
}

func TestRequestAndOperationSpansPropagateWithoutPayloads(t *testing.T) {
	t.Parallel()
	i, exporter, _ := instrumentation(t)
	type body struct {
		Secret string `json:"secret"`
	}
	type input struct {
		ID    string `path:"id" json:"id"`
		Query string `query:"q" json:"q"`
		Body  body   `body:"json" json:"body"`
	}
	var active trace.SpanContext
	app, err := trest.New(trest.Use(i.Middleware), trest.Observe(i),
		trest.Authenticate(func(*http.Request) (trest.Principal, error) {
			return trest.Principal{ID: "private-actor", TenantID: "private-tenant"}, nil
		}), trest.RequireAuth(),
		trest.Post("/invoices/{id}", func(ctx context.Context, in input) (body, error) {
			active = trace.SpanContextFromContext(ctx)
			return body{Secret: "private-response"}, nil
		}).Name("invoice"),
	)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://private-host.example/invoices/private-id?q=private-query", strings.NewReader(`{"secret":"private-body"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer private-credential")
	r.Header.Set("Cookie", "session=private-cookie")
	r.Header.Set("Baggage", "secret=private-baggage")
	r.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	spans := exporter.GetSpans()
	server := serverSpan(t, spans)
	if server.Name != "POST /invoices/{id}" || attributeValue(server, "http.route").AsString() != "/invoices/{id}" {
		t.Fatalf("request route=%s attrs=%+v", server.Name, server.Attributes)
	}
	if server.Parent.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || server.Parent.SpanID().String() != "00f067aa0ba902b7" || !server.Parent.IsRemote() {
		t.Fatalf("remote parent=%+v", server.Parent)
	}
	if active.SpanID() != server.SpanContext.SpanID() || active.TraceID() != server.Parent.TraceID() {
		t.Fatal("handler did not inherit the request span")
	}
	if server.Status.Code != codes.Unset || attributeValue(server, "http.response.status_code").AsInt64() != 200 {
		t.Fatalf("request status=%+v attrs=%+v", server.Status, server.Attributes)
	}
	opID := app.Routes()[0].OperationID
	phases := map[string]bool{}
	for _, span := range spans {
		if span.SpanKind == trace.SpanKindServer {
			continue
		}
		phase := attributeValue(span, "trest.operation.phase").AsString()
		phases[phase] = true
		if span.Name != "trest."+phase+" "+opID || attributeValue(span, "trest.operation.id").AsString() != opID {
			t.Fatalf("operation name or metadata: %+v", span)
		}
		if span.Parent.SpanID() != server.SpanContext.SpanID() || span.SpanContext.TraceID() != server.SpanContext.TraceID() {
			t.Fatal("phase span is not a child of the request span")
		}
		if span.StartTime.Before(server.StartTime) || span.EndTime.After(server.EndTime) || span.EndTime.Before(span.StartTime) {
			t.Fatalf("phase timing outside request: %s", phase)
		}
		if span.Status.Code != codes.Unset {
			t.Fatalf("successful phase status=%+v", span.Status)
		}
	}
	for _, phase := range []string{"validate", "authorize", "handler", "invoke"} {
		if !phases[phase] {
			t.Errorf("missing %s phase", phase)
		}
	}
	assertNoSecrets(t, spans, "private-host", "private-id", "private-query", "private-body", "private-response", "private-credential", "private-cookie", "private-baggage", "private-actor", "private-tenant")
}

func TestObserveUsesRecordedTimesAndClassifications(t *testing.T) {
	t.Parallel()
	i, exporter, provider := instrumentation(t)
	started := time.Date(2026, 11, 10, 1, 2, 3, 4, time.UTC)
	ctx, parent := provider.Tracer("test").Start(context.Background(), "parent", trace.WithTimestamp(started.Add(-time.Second)))
	event := operation.Event{
		OperationID: "invoice.create", Phase: "resolve", StartedAt: started, Duration: 37 * time.Millisecond,
		Code: operation.Forbidden, PrincipalID: "private-actor", TenantID: "private-tenant", CorrelationID: "private-correlation",
		TraceID: "private-internal-trace", SpanID: "private-internal-span", ParentSpanID: "private-internal-parent",
	}
	i.Observe(ctx, event)
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	span := spans[0]
	if span.Name != "trest.resolve invoice.create" || span.SpanKind != trace.SpanKindInternal || span.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("phase span=%+v", span)
	}
	if !span.StartTime.Equal(started) || !span.EndTime.Equal(started.Add(event.Duration)) {
		t.Fatalf("start=%s end=%s", span.StartTime, span.EndTime)
	}
	if span.Status.Code != codes.Error || span.Status.Description != "" || attributeValue(span, "error.type").AsString() != "forbidden" || attributeValue(span, "trest.error.code").AsString() != "forbidden" {
		t.Fatalf("error metadata=%+v %+v", span.Status, span.Attributes)
	}
	assertNoSecrets(t, spans, "private-actor", "private-tenant", "private-correlation", "private-internal")
	event.Code = "private-error-text"
	i.Observe(ctx, event)
	if got := attributeValue(exporter.GetSpans()[1], "error.type").AsString(); got != "internal" {
		t.Fatalf("unknown error=%q", got)
	}
	parent.End(trace.WithTimestamp(started.Add(time.Second)))
}

func TestObserveIgnoresInvalidEventMetadata(t *testing.T) {
	t.Parallel()
	i, exporter, _ := instrumentation(t)
	for _, tc := range []struct {
		name  string
		event operation.Event
	}{
		{"missing time", operation.Event{OperationID: "invoice.get", Phase: "handler"}},
		{"negative duration", operation.Event{OperationID: "invoice.get", Phase: "handler", StartedAt: time.Now(), Duration: -time.Second}},
		{"unknown phase", operation.Event{OperationID: "invoice.get", Phase: "private-payload", StartedAt: time.Now()}},
		{"invalid operation", operation.Event{OperationID: "invoice /private-id", Phase: "handler", StartedAt: time.Now()}},
	} {
		t.Run(tc.name, func(t *testing.T) { i.Observe(context.Background(), tc.event) })
	}
	if spans := exporter.GetSpans(); len(spans) != 0 {
		t.Fatalf("invalid spans=%+v", spans)
	}
}

func TestHTTPFailureAndPanicClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		failure    error
		panics     bool
		status     int
		spanStatus codes.Code
		errorType  string
	}{
		{"public rejection", trest.NewError(trest.Forbidden, "private-public-message", nil), false, 403, codes.Unset, ""},
		{"private failure", errors.New("private-internal-error"), false, 500, codes.Error, "500"},
		{"panic", nil, true, 500, codes.Error, "panic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, exporter, _ := instrumentation(t)
			app, err := trest.New(trest.Use(i.Middleware), trest.Observe(i), trest.Get("/failure", func(context.Context, trest.Empty) (string, error) {
				if tc.panics {
					panic("private-panic-value")
				}
				return "", tc.failure
			}).Name("failure"))
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest("GET", "http://api.example/failure", nil))
			if w.Code != tc.status {
				t.Fatalf("HTTP status=%d body=%s", w.Code, w.Body)
			}
			spans := exporter.GetSpans()
			server := serverSpan(t, spans)
			wantRecordedStatus := int64(tc.status)
			if tc.panics {
				wantRecordedStatus = 0 // Trest's outer recovery has not written yet.
			}
			if server.Status.Code != tc.spanStatus || attributeValue(server, "error.type").AsString() != tc.errorType || attributeValue(server, "http.response.status_code").AsInt64() != wantRecordedStatus {
				t.Fatalf("server failure=%+v attrs=%+v", server.Status, server.Attributes)
			}
			var errorPhase bool
			for _, span := range spans {
				if span.SpanKind == trace.SpanKindInternal && span.Status.Code == codes.Error {
					errorPhase = true
				}
			}
			if !errorPhase {
				t.Fatal("operation failure was not observed")
			}
			assertNoSecrets(t, spans, "private-public-message", "private-internal-error", "private-panic-value")
		})
	}
}

func TestAuthenticationStillRejectsBeforeHandler(t *testing.T) {
	t.Parallel()
	i, exporter, _ := instrumentation(t)
	var calls atomic.Int64
	app, err := trest.New(trest.Use(i.Middleware), trest.Observe(i), trest.RequireAuth(),
		trest.Get("/private", func(context.Context, trest.Empty) (string, error) { calls.Add(1); return "secret", nil }))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "http://api.example/private", nil))
	if w.Code != 401 || calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
	}
	server := serverSpan(t, exporter.GetSpans())
	if server.Status.Code != codes.Unset || attributeValue(server, "http.response.status_code").AsInt64() != 401 {
		t.Fatalf("authentication response span=%+v", server)
	}
}

func TestPanicRecoveryStatusObservedOutsideApp(t *testing.T) {
	t.Parallel()
	for _, installation := range []string{"inside app", "outside app"} {
		t.Run(installation, func(t *testing.T) {
			i, exporter, _ := instrumentation(t)
			components := []trest.Component{
				trest.Observe(i),
				trest.OnError(func(context.Context, error) error {
					return trest.NewError(trest.Unavailable, "private-recovery-message", nil)
				}),
				trest.Get("/panic", func(context.Context, trest.Empty) (string, error) {
					panic("private-panic-value")
				}),
			}
			if installation == "inside app" {
				components = append(components, trest.Use(i.Middleware))
			}
			app, err := trest.New(components...)
			if err != nil {
				t.Fatal(err)
			}
			var handler http.Handler = app
			if installation == "outside app" {
				handler = i.Middleware(app)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", "http://api.example/panic", nil))
			if w.Code != 503 {
				t.Fatalf("recovered response=%d %s", w.Code, w.Body)
			}
			span := serverSpan(t, exporter.GetSpans())
			if span.Status.Code != codes.Error {
				t.Fatal("panic response was not an error")
			}
			status := attributeValue(span, "http.response.status_code")
			if installation == "inside app" && status.Type() != attribute.EMPTY {
				t.Fatal("middleware guessed a response status before recovery")
			}
			if installation == "outside app" && status.AsInt64() != 503 {
				t.Fatalf("recovered status=%v", status)
			}
			assertNoSecrets(t, exporter.GetSpans(), "private-recovery-message", "private-panic-value")
		})
	}
}

func TestOuterMiddlewareUsesMatchedPatternAndNeverRawURL(t *testing.T) {
	t.Parallel()
	i, exporter, _ := instrumentation(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	handler := i.Middleware(mux)
	for _, path := range []string{"/things/private-id", "/unmatched-private-path"} {
		exporter.Reset()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "http://api.example"+path+"?secret=private-query", nil))
		span := serverSpan(t, exporter.GetSpans())
		if path == "/things/private-id" && span.Name != "GET /things/{id}" {
			t.Fatalf("matched name=%q", span.Name)
		}
		if path != "/things/private-id" && (span.Name != "GET" || attributeValue(span, "http.route").AsString() != "") {
			t.Fatalf("unmatched name=%q", span.Name)
		}
		assertNoSecrets(t, exporter.GetSpans(), "private-id", "unmatched-private-path", "private-query")
	}
}

func TestCancellationAndWriteErrorsDoNotExposeMessages(t *testing.T) {
	t.Parallel()
	i, exporter, _ := instrumentation(t)
	ctx, cancel := context.WithCancel(context.Background())
	handler := i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { cancel(); w.WriteHeader(408) }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://api.example/secret", nil).WithContext(ctx))
	span := serverSpan(t, exporter.GetSpans())
	if span.Status.Code != codes.Error || attributeValue(span, "error.type").AsString() != "canceled" {
		t.Fatalf("canceled span=%+v", span)
	}
	exporter.Reset()
	failure := &failedWriter{header: make(http.Header)}
	handler = i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("private-response")) }))
	handler.ServeHTTP(failure, httptest.NewRequest("GET", "http://api.example/secret", nil))
	span = serverSpan(t, exporter.GetSpans())
	if span.Status.Code != codes.Error || attributeValue(span, "error.type").AsString() != "write_error" {
		t.Fatalf("write failure span=%+v", span)
	}
	assertNoSecrets(t, exporter.GetSpans(), "private-write-error", "private-response")
}

type failedWriter struct{ header http.Header }

func (w *failedWriter) Header() http.Header     { return w.header }
func (*failedWriter) WriteHeader(int)           {}
func (*failedWriter) Write([]byte) (int, error) { return 0, errors.New("private-write-error") }

func TestResponseControllerCapabilitiesRemainAvailable(t *testing.T) {
	t.Parallel()
	i, exporter, _ := instrumentation(t)
	underlying := &capableWriter{ResponseRecorder: httptest.NewRecorder()}
	handler := i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(103)
		w.WriteHeader(201)
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			t.Error(err)
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
	}))
	handler.ServeHTTP(underlying, httptest.NewRequest("GET", "http://api.example/", nil))
	span := serverSpan(t, exporter.GetSpans())
	if attributeValue(span, "http.response.status_code").AsInt64() != 201 || !underlying.deadline || !underlying.Flushed {
		t.Fatalf("capabilities lost: attrs=%+v deadline=%t flush=%t", span.Attributes, underlying.deadline, underlying.Flushed)
	}
	exporter.Reset()
	left, right := net.Pipe()
	defer func() {
		if err := left.Close(); err != nil {
			t.Error(err)
		}
		if err := right.Close(); err != nil {
			t.Error(err)
		}
	}()
	underlying.connection = left
	handler = i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, _, err := http.NewResponseController(w).Hijack()
		if err != nil || connection != left {
			t.Errorf("hijack=%v %v", connection, err)
		}
	}))
	handler.ServeHTTP(underlying, httptest.NewRequest("GET", "http://api.example/", nil))
	span = serverSpan(t, exporter.GetSpans())
	if attributeValue(span, "http.response.status_code").AsInt64() != 101 {
		t.Fatal("hijack status not recorded")
	}
}

type capableWriter struct {
	*httptest.ResponseRecorder
	deadline   bool
	connection net.Conn
}

func (w *capableWriter) SetWriteDeadline(time.Time) error { w.deadline = true; return nil }
func (w *capableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.connection, bufio.NewReadWriter(bufio.NewReader(w.connection), bufio.NewWriter(w.connection)), nil
}

func TestConcurrentRequestsKeepSeparateParents(t *testing.T) {
	t.Parallel()
	i, exporter, _ := instrumentation(t)
	app, err := trest.New(trest.Use(i.Middleware), trest.Observe(i), trest.Get("/items/{id}", func(context.Context, struct {
		ID string `path:"id" json:"id"`
	}) (string, error) {
		return "ok", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for index := range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("http://api.example/items/%d", index), nil))
			if w.Code != 200 {
				t.Errorf("status=%d", w.Code)
			}
		}()
	}
	group.Wait()
	spans := exporter.GetSpans()
	parents := map[trace.SpanID]trace.TraceID{}
	for _, span := range spans {
		if span.SpanKind == trace.SpanKindServer {
			parents[span.SpanContext.SpanID()] = span.SpanContext.TraceID()
		}
	}
	if len(parents) != 20 {
		t.Fatalf("parents=%d", len(parents))
	}
	for _, span := range spans {
		if span.SpanKind != trace.SpanKindInternal {
			continue
		}
		if traceID, ok := parents[span.Parent.SpanID()]; !ok || traceID != span.SpanContext.TraceID() {
			t.Fatal("phase attached to another request")
		}
	}
}

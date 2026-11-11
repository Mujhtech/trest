package operation_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mujhtech/trest/operation"
	"github.com/mujhtech/trest/telemetry"
)

func TestTracePhasesIdentityAndRedaction(t *testing.T) {
	recorder, err := telemetry.NewRecorder(32)
	if err != nil {
		t.Fatal(err)
	}
	r := operation.NewRegistry(nil, operation.WithObserver(recorder))
	h := register(t, r, public("echo.v1"), echo)
	freeze(t, r)
	ctx := operation.WithTrace(context.Background(), operation.TraceContext{TraceID: "trace-parent", SpanID: "span-parent"})
	ctx = operation.WithCorrelationID(ctx, "request-1")
	ctx = operation.WithPrincipal(ctx, operation.Principal{ID: "actor", TenantID: "tenant"})
	_, err = h.Invoke(ctx, input{Name: "secret-payload"})
	if err != nil {
		t.Fatal(err)
	}
	trace, ok := recorder.Trace("trace-parent")
	if !ok || len(trace.Events) != 4 {
		t.Fatalf("events %+v", trace)
	}
	expected := []string{"validate", "authorize", "handler", "invoke"}
	for i, e := range trace.Events {
		if e.Phase != expected[i] || e.OperationID != "echo.v1" || e.ParentSpanID != "span-parent" || e.SpanID == "span-parent" || e.PrincipalID != "actor" || e.TenantID != "tenant" || e.CorrelationID != "request-1" {
			t.Fatalf("bad event %+v", e)
		}
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-payload") {
		t.Fatal("trace contains payload")
	}
	source := h.Metadata().Source
	if source.Line <= 0 || source.Function == "" || source.File != "registry_test.go" {
		t.Fatalf("source %+v", source)
	}
}

func TestObserverCannotChangeResults(t *testing.T) {
	r := operation.NewRegistry(nil, operation.WithObserver(operation.ObserverFunc(func(context.Context, operation.Event) { panic("observer bug") })))
	h := register(t, r, public("echo.v1"), echo)
	freeze(t, r)
	out, err := h.Invoke(context.Background(), input{Name: "Ada"})
	if err != nil || out.Name != "Ada" {
		t.Fatalf("result %+v %v", out, err)
	}
}

func TestPanicRecordedWithoutBeingSwallowed(t *testing.T) {
	recorder, err := telemetry.NewRecorder(32)
	if err != nil {
		t.Fatal(err)
	}
	r := operation.NewRegistry(nil, operation.WithObserver(recorder))
	h := register(t, r, public("panic.v1"), func(context.Context, input) (output, error) { panic("private panic detail") })
	freeze(t, r)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		_, _ = h.Invoke(context.Background(), input{})
	}()
	traces := recorder.Traces()
	if len(traces) != 1 {
		t.Fatalf("traces %+v", traces)
	}
	events := traces[0].Events
	if events[len(events)-1].Code != operation.Internal {
		t.Fatalf("panic recorded as success: %+v", events)
	}
	encoded, _ := json.Marshal(traces)
	if strings.Contains(string(encoded), "private panic") {
		t.Fatal("panic detail leaked")
	}
}

func TestNestedInvocationTrace(t *testing.T) {
	recorder, _ := telemetry.NewRecorder(32)
	r := operation.NewRegistry(nil, operation.WithObserver(recorder))
	child := register(t, r, public("child.v1"), echo)
	parent := register(t, r, public("parent.v1"), func(ctx context.Context, in input) (output, error) { return child.Invoke(ctx, in) })
	freeze(t, r)
	if _, err := parent.Invoke(context.Background(), input{}); err != nil {
		t.Fatal(err)
	}
	traces := recorder.Traces()
	if len(traces) != 1 {
		t.Fatalf("not a single trace: %+v", traces)
	}
	var parentSpan string
	for _, e := range traces[0].Events {
		if e.OperationID == "parent.v1" {
			parentSpan = e.SpanID
		}
	}
	for _, e := range traces[0].Events {
		if e.OperationID == "child.v1" && e.ParentSpanID != parentSpan {
			t.Fatal("child parent link lost")
		}
	}
}

func TestJSONTraceIncludesTransportOutcome(t *testing.T) {
	recorder, _ := telemetry.NewRecorder(32)
	r := operation.NewRegistry(nil, operation.WithObserver(recorder), operation.WithJSONLimits(256, 4))
	register(t, r, public("echo.v1"), echo)
	freeze(t, r)
	ctx := operation.WithTrace(context.Background(), operation.TraceContext{TraceID: "json-outcome"})
	_, err := r.InvokeJSON(ctx, "echo.v1", json.RawMessage(`{"name":"Ada"}`))
	if operation.CodeOf(err) != operation.Internal {
		t.Fatalf("got %v", err)
	}
	trace, _ := recorder.Trace("json-outcome")
	last := trace.Events[len(trace.Events)-1]
	if last.Phase != "json_invoke" || last.Code != operation.Internal {
		t.Fatalf("missing failed delivery outcome: %+v", trace)
	}
	handlerSucceeded := false
	for _, e := range trace.Events {
		if e.Phase == "handler" && e.Code == "" {
			handlerSucceeded = true
		}
	}
	if !handlerSucceeded {
		t.Fatal("lost successful handler evidence")
	}
	ctx = operation.WithTrace(context.Background(), operation.TraceContext{TraceID: "invalid-json"})
	_, err = r.PrepareJSON(ctx, "echo.v1", json.RawMessage(`{"unknown":1}`))
	trace, _ = recorder.Trace("invalid-json")
	if operation.CodeOf(err) != operation.InvalidInput || len(trace.Events) != 1 || trace.Events[0].Code != operation.InvalidInput {
		t.Fatalf("invalid input trace %+v %v", trace, err)
	}
}

func TestMetadataContextsPreserveLifetimeAndValues(t *testing.T) {
	t.Parallel()
	type appKey struct{}
	deadline := time.Now().Add(time.Minute)
	parent, stop := context.WithDeadline(context.WithValue(context.Background(), appKey{}, "app-value"), deadline)
	defer stop()
	parent, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	original := operation.TraceContext{TraceID: "trace", SpanID: "span", ParentSpanID: "parent"}
	ctx := operation.WithCorrelationID(operation.WithTrace(parent, original), "request")
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) || ctx.Done() != parent.Done() || ctx.Err() != nil {
		t.Fatalf("lifetime not forwarded: deadline=%v ok=%v err=%v", got, ok, ctx.Err())
	}
	if ctx.Value(appKey{}) != "app-value" {
		t.Fatal("application value lost")
	}
	copied := operation.TraceFromContext(ctx)
	copied.TraceID = "changed"
	original.SpanID = "changed"
	if got := operation.TraceFromContext(ctx); got.TraceID != "trace" || got.SpanID != "span" {
		t.Fatalf("trace was mutable: %+v", got)
	}
	blank := operation.WithCorrelationID(operation.WithTrace(ctx, operation.TraceContext{}), "")
	if operation.TraceFromContext(blank) != (operation.TraceContext{}) || operation.CorrelationID(blank) != "" {
		t.Fatal("empty metadata failed to shadow parent")
	}
	if got := operation.TraceFromContext(ctx); got.TraceID != "trace" || operation.CorrelationID(ctx) != "request" {
		t.Fatal("child changed parent metadata")
	}
	cause := errors.New("test cancellation")
	cancel(cause)
	if ctx.Err() != context.Canceled || !errors.Is(context.Cause(ctx), cause) || !errors.Is(context.Cause(blank), cause) {
		t.Fatalf("cancellation/cause lost: %v %v", ctx.Err(), context.Cause(ctx))
	}
}

func TestMetadataContextsSupportConcurrentReaders(t *testing.T) {
	t.Parallel()
	want := operation.TraceContext{TraceID: "trace", SpanID: "span"}
	ctx := operation.WithCorrelationID(operation.WithTrace(context.Background(), want), "request")
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				got := operation.TraceFromContext(ctx)
				if got != want || operation.CorrelationID(ctx) != "request" {
					t.Error("metadata changed during concurrent read")
					return
				}
				got.SpanID = "local-copy"
			}
		}()
	}
	wg.Wait()
}

func TestMetadataContextsRejectNilParent(t *testing.T) {
	for _, makeContext := range []func(){
		func() { operation.WithTrace(nil, operation.TraceContext{}) }, //nolint:staticcheck // Verify nil-parent rejection.
		func() { operation.WithCorrelationID(nil, "") },               //nolint:staticcheck // Verify nil-parent rejection.
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("nil parent accepted")
				}
			}()
			makeContext()
		}()
	}
}

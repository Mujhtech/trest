package operation_test

import (
	"context"
	"sync"
	"testing"

	"github.com/mujhtech/trest/internal/spanreserve"
	"github.com/mujhtech/trest/operation"
)

type reservedSpanContext struct {
	context.Context
	reservation spanreserve.Reservation
}

func (c *reservedSpanContext) Value(key any) any {
	if spanreserve.IsKey(key) {
		return &c.reservation
	}
	return c.Context.Value(key)
}

func TestReservedSpanNestedInvocationsRetainIndependentTraces(t *testing.T) {
	for _, mode := range invocationContextModes {
		t.Run(mode.name, func(t *testing.T) {
			base := context.Background()
			if mode.cancelable {
				var cancel context.CancelFunc
				base, cancel = context.WithCancel(base)
				defer cancel()
			}
			originalTrace := operation.TraceContext{TraceID: "upstream trace", SpanID: "upstream span"}
			base = operation.WithTrace(base, originalTrace)
			ctx := &reservedSpanContext{Context: base, reservation: spanreserve.New("reserved span")}
			var options []operation.Option
			if mode.observed {
				options = append(options, operation.WithObserver(operation.ObserverFunc(func(context.Context, operation.Event) {})))
			}
			registry := operation.NewRegistry(nil, options...)
			var contexts []context.Context
			var traces []operation.TraceContext
			capture := func(ctx context.Context) {
				contexts = append(contexts, ctx)
				traces = append(traces, operation.TraceFromContext(ctx))
			}
			child := register(t, registry, public("reserved.child.v1"), func(ctx context.Context, in input) (output, error) {
				capture(ctx)
				return echo(ctx, in)
			})
			parent := register(t, registry, public("reserved.parent.v1"), func(ctx context.Context, in input) (output, error) {
				capture(ctx)
				return child.Invoke(ctx, in)
			})
			freeze(t, registry)
			for range 2 {
				if _, err := parent.Invoke(ctx, input{Name: "Ada"}); err != nil {
					t.Fatal(err)
				}
			}
			if len(traces) != 4 || traces[0].SpanID != "reserved span" {
				t.Fatalf("first invocation did not consume reservation: %#v", traces)
			}
			seen := make(map[string]bool)
			for index, trace := range traces {
				if trace.SpanID == "" || seen[trace.SpanID] || trace.TraceID != originalTrace.TraceID {
					t.Fatalf("invocation reused or lost trace identity: %#v", trace)
				}
				seen[trace.SpanID] = true
				wantParent := originalTrace.SpanID
				if index%2 == 1 {
					wantParent = traces[index-1].SpanID
				}
				if trace.ParentSpanID != wantParent || operation.TraceFromContext(contexts[index]) != trace {
					t.Fatalf("retained trace or nested link changed: %#v", trace)
				}
			}
			if operation.TraceFromContext(ctx) != originalTrace || ctx.Err() != nil || spanreserve.Take(ctx) != "" {
				t.Fatal("invocation changed parent metadata/lifetime or reused its reservation")
			}
		})
	}
}

func TestReservedSpanConcurrentInvocationsConsumeOnce(t *testing.T) {
	t.Parallel()
	const workers = 64
	registry := operation.NewRegistry(nil, operation.WithMaxConcurrent(workers))
	handle := register(t, registry, public("reserved.concurrent.v1"), func(ctx context.Context, _ input) (output, error) {
		return output{Name: operation.TraceFromContext(ctx).SpanID}, nil
	})
	freeze(t, registry)
	ctx := &reservedSpanContext{Context: context.Background(), reservation: spanreserve.New("reserved span")}
	results := make(chan string, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := handle.Invoke(ctx, input{})
			if err != nil {
				t.Error(err)
				return
			}
			results <- out.Name
		}()
	}
	wg.Wait()
	close(results)
	seen := make(map[string]bool, workers)
	for span := range results {
		if span == "" || seen[span] {
			t.Fatalf("concurrent invocation reused span %q", span)
		}
		seen[span] = true
	}
	if len(seen) != workers || !seen["reserved span"] {
		t.Fatalf("got %d unique spans, reserved span consumed = %v", len(seen), seen["reserved span"])
	}
}

func TestResolverCannotConsumeAnotherRequestSpanReservation(t *testing.T) {
	t.Parallel()
	registry := operation.NewRegistry(nil)
	foreign := &reservedSpanContext{Context: context.Background(), reservation: spanreserve.New("foreign span")}
	original := &reservedSpanContext{Context: context.Background(), reservation: spanreserve.New("original span")}
	var parentTrace, childTrace operation.TraceContext
	child := register(t, registry, public("reserved.resolved.child.v1"), func(ctx context.Context, in input) (output, error) {
		childTrace = operation.TraceFromContext(ctx)
		return echo(ctx, in)
	})
	definition := public("reserved.resolved.parent.v1")
	definition.Resolve = func(context.Context, input) (context.Context, error) { return foreign, nil }
	parent := register(t, registry, definition, func(ctx context.Context, in input) (output, error) {
		parentTrace = operation.TraceFromContext(ctx)
		return child.Invoke(ctx, in)
	})
	freeze(t, registry)
	if _, err := parent.Invoke(original, input{}); err != nil {
		t.Fatal(err)
	}
	if parentTrace.SpanID != "original span" || childTrace.SpanID == "original span" || childTrace.SpanID == "foreign span" || childTrace.SpanID == "" || childTrace.ParentSpanID != parentTrace.SpanID {
		t.Fatalf("resolver redirected invocation identity: parent=%#v child=%#v", parentTrace, childTrace)
	}
	if got := spanreserve.Take(foreign); got != "foreign span" {
		t.Fatalf("nested invocation consumed unrelated reservation: %q", got)
	}
}

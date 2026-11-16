package operation_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mujhtech/trest/operation"
)

type invocationContextMode struct {
	name       string
	cancelable bool
	observed   bool
}

var invocationContextModes = []invocationContextMode{
	{name: "background"},
	{name: "cancelable", cancelable: true},
	{name: "observed", observed: true},
}

func TestInvocationRetainedContextAndIndependentTrace(t *testing.T) {
	for _, mode := range invocationContextModes {
		t.Run(mode.name, func(t *testing.T) {
			parent := context.Background()
			if mode.cancelable {
				var cancel context.CancelFunc
				parent, cancel = context.WithCancel(parent)
				defer cancel()
			}
			parentTrace := operation.TraceContext{TraceID: "parent trace", SpanID: "parent span"}
			principal := operation.Principal{ID: "actor", TenantID: "tenant"}
			parent = operation.WithTrace(parent, parentTrace)
			parent = operation.WithPrincipal(parent, principal)
			parent = operation.WithCorrelationID(parent, "request")
			var options []operation.Option
			var finalObserverContexts []context.Context
			if mode.observed {
				options = append(options, operation.WithObserver(operation.ObserverFunc(func(ctx context.Context, event operation.Event) {
					if event.Phase == "invoke" {
						finalObserverContexts = append(finalObserverContexts, ctx)
						if ctx.Err() != nil {
							t.Error("completion observer received a canceled invocation context")
						}
					}
				})))
			}
			registry := operation.NewRegistry(nil, options...)
			var retained []context.Context
			var traces []operation.TraceContext
			handle := register(t, registry, public("retained.v1"), func(ctx context.Context, in input) (output, error) {
				if ctx.Err() != nil {
					t.Fatal("handler context already canceled")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("handler has no deadline")
				}
				if got, ok := operation.PrincipalFromContext(ctx); !ok || got != principal {
					t.Fatalf("identity = %#v, %v", got, ok)
				}
				if operation.CorrelationID(ctx) != "request" {
					t.Fatal("correlation ID lost")
				}
				retained = append(retained, ctx)
				traces = append(traces, operation.TraceFromContext(ctx))
				return echo(ctx, in)
			})
			freeze(t, registry)
			for range 2 {
				if _, err := handle.Invoke(parent, input{Name: "Ada"}); err != nil {
					t.Fatal(err)
				}
			}
			if traces[0].SpanID == traces[1].SpanID || traces[0].SpanID == "" || traces[1].SpanID == "" {
				t.Fatalf("invocations did not receive distinct spans: %#v", traces)
			}
			for i, ctx := range retained {
				if ctx.Err() != context.Canceled || context.Cause(ctx) != context.Canceled {
					t.Fatalf("retained error/cause = %v/%v", ctx.Err(), context.Cause(ctx))
				}
				select {
				case <-ctx.Done():
				default:
					t.Fatal("retained Done is open")
				}
				if got := operation.TraceFromContext(ctx); got != traces[i] || got.TraceID != parentTrace.TraceID || got.ParentSpanID != parentTrace.SpanID {
					t.Fatalf("retained trace changed: %#v, want %#v", got, traces[i])
				}
			}
			if parent.Err() != nil || operation.TraceFromContext(parent) != parentTrace {
				t.Fatal("invocation changed parent lifetime or trace")
			}
			for _, ctx := range finalObserverContexts {
				if ctx.Err() != nil || context.Cause(ctx) != nil {
					t.Fatal("completion observer's retained context was canceled")
				}
			}
		})
	}
}

func TestInvocationHookOrderingAcrossContextPaths(t *testing.T) {
	type resourceKey struct{}
	for _, mode := range invocationContextModes {
		for _, outcome := range []string{"allowed", "invalid", "denied"} {
			t.Run(mode.name+"/"+outcome, func(t *testing.T) {
				parent := context.Background()
				if mode.cancelable {
					var cancel context.CancelFunc
					parent, cancel = context.WithCancel(parent)
					defer cancel()
				}
				parent = operation.WithPrincipal(parent, operation.Principal{ID: "actor"})
				var options []operation.Option
				if mode.observed {
					options = append(options, operation.WithObserver(operation.ObserverFunc(func(context.Context, operation.Event) {})))
				}
				registry := operation.NewRegistry(nil, options...)
				var steps []string
				definition := operation.Definition[input, output]{
					ID: "ordered.v1", Summary: "Ordered", Access: operation.Permission("read"),
					Validate: func(context.Context, input) error {
						steps = append(steps, "validate")
						if outcome == "invalid" {
							return operation.NewError(operation.InvalidInput, "invalid", nil)
						}
						return nil
					},
					Resolve: func(ctx context.Context, _ input) (context.Context, error) {
						steps = append(steps, "resolve")
						return context.WithValue(ctx, resourceKey{}, "resolved"), nil
					},
					Authorize: func(ctx context.Context, principal operation.Principal, _ input) error {
						steps = append(steps, "authorize")
						if principal.ID != "actor" || ctx.Value(resourceKey{}) != "resolved" {
							t.Fatal("authorization did not receive identity and resolved values")
						}
						if outcome == "denied" {
							return operation.NewError(operation.Forbidden, "denied", nil)
						}
						return nil
					},
					ValidateOutput: func(context.Context, output) error {
						steps = append(steps, "output validation")
						return nil
					},
				}
				handle := register(t, registry, definition, func(ctx context.Context, in input) (output, error) {
					steps = append(steps, "handler")
					return echo(ctx, in)
				})
				freeze(t, registry)
				_, err := handle.Invoke(parent, input{Name: "Ada"})
				want := []string{"validate", "resolve", "authorize", "handler", "output validation"}
				var wantCode operation.Code
				switch outcome {
				case "invalid":
					want, wantCode = want[:1], operation.InvalidInput
				case "denied":
					want, wantCode = want[:3], operation.Forbidden
				}
				if !reflect.DeepEqual(steps, want) || operation.CodeOf(err) != wantCode {
					t.Fatalf("steps=%v error=%v, want %v code=%v", steps, err, want, wantCode)
				}
			})
		}
	}
}

func TestUnobservedNestedInvocationKeepsParentAlive(t *testing.T) {
	registry := operation.NewRegistry(nil)
	var parentContext, childContext context.Context
	var parentTrace, childTrace operation.TraceContext
	child := register(t, registry, public("nested.child.v1"), func(ctx context.Context, in input) (output, error) {
		childContext = ctx
		childTrace = operation.TraceFromContext(ctx)
		return echo(ctx, in)
	})
	parent := register(t, registry, public("nested.parent.v1"), func(ctx context.Context, in input) (output, error) {
		parentContext = ctx
		parentTrace = operation.TraceFromContext(ctx)
		out, err := child.Invoke(ctx, in)
		if ctx.Err() != nil || context.Cause(ctx) != nil {
			t.Fatal("child completion canceled parent")
		}
		if childContext.Err() != context.Canceled || context.Cause(childContext) != context.Canceled {
			t.Fatal("child context did not cancel independently")
		}
		return out, err
	})
	freeze(t, registry)
	if _, err := parent.Invoke(context.Background(), input{Name: "Ada"}); err != nil {
		t.Fatal(err)
	}
	if childTrace.TraceID != parentTrace.TraceID || childTrace.ParentSpanID != parentTrace.SpanID || childTrace.SpanID == parentTrace.SpanID {
		t.Fatalf("nested trace links changed: parent=%#v child=%#v", parentTrace, childTrace)
	}
	if parentContext.Err() != context.Canceled || context.Cause(parentContext) != context.Canceled {
		t.Fatal("parent context did not cancel on completion")
	}
}

func TestUnobservedPanicCancelsRetainedContextAndReleasesCapacity(t *testing.T) {
	registry := operation.NewRegistry(nil, operation.WithMaxConcurrent(1))
	var retained context.Context
	handle := register(t, registry, public("panic.lifetime.v1"), func(ctx context.Context, in input) (output, error) {
		if retained == nil {
			retained = ctx
			panic("handler panic")
		}
		return echo(ctx, in)
	})
	freeze(t, registry)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("handler panic was swallowed")
			}
		}()
		_, _ = handle.Invoke(context.Background(), input{})
	}()
	if retained.Err() != context.Canceled || context.Cause(retained) != context.Canceled {
		t.Fatal("panic left invocation context alive")
	}
	if _, err := handle.Invoke(context.Background(), input{Name: "Ada"}); err != nil {
		t.Fatalf("panic did not release capacity: %v", err)
	}
}

func TestResolverCannotReplaceInvocationCancellationCause(t *testing.T) {
	for _, parentCanceled := range []bool{false, true} {
		name := "invocation completes"
		if parentCanceled {
			name = "parent canceled"
		}
		t.Run(name, func(t *testing.T) {
			type resourceKey struct{}
			parent := context.Background()
			var cancelParent context.CancelCauseFunc
			if parentCanceled {
				parent, cancelParent = context.WithCancelCause(parent)
				defer cancelParent(nil)
			}
			parentCause := errors.New("parent cancellation")
			resolverCause := errors.New("unrelated resolver cancellation")
			resolved, cancelResolved := context.WithCancelCause(context.WithValue(context.Background(), resourceKey{}, "resource"))
			cancelResolved(resolverCause)
			registry := operation.NewRegistry(nil)
			definition := public("resolver.cause.v1")
			definition.Resolve = func(context.Context, input) (context.Context, error) { return resolved, nil }
			var retained context.Context
			handle := register(t, registry, definition, func(ctx context.Context, in input) (output, error) {
				retained = ctx
				if ctx.Err() != nil || context.Cause(ctx) != nil || ctx.Value(resourceKey{}) != "resource" {
					t.Fatalf("resolver changed active lifetime or values: %v/%v", ctx.Err(), context.Cause(ctx))
				}
				if parentCanceled {
					cancelParent(parentCause)
				}
				return echo(ctx, in)
			})
			freeze(t, registry)
			_, err := handle.Invoke(parent, input{Name: "Ada"})
			wantCause := error(context.Canceled)
			if parentCanceled {
				wantCause = parentCause
				if operation.CodeOf(err) != operation.Canceled {
					t.Fatalf("invocation error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if retained == nil {
				t.Fatal("handler did not receive context")
			}
			if retained.Err() != context.Canceled || context.Cause(retained) != wantCause {
				t.Fatalf("retained error/cause = %v/%v, want canceled/%v", retained.Err(), context.Cause(retained), wantCause)
			}
		})
	}
}

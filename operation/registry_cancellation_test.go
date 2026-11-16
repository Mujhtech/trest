package operation_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/mujhtech/trest/operation"
)

func TestObserverCancellationAfterAuthorizationPreventsHandler(t *testing.T) {
	for _, access := range []string{"public", "protected"} {
		t.Run(access, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			parent = operation.WithPrincipal(parent, operation.Principal{ID: "actor"})
			var events []operation.Event
			registry := operation.NewRegistry(nil, operation.WithObserver(operation.ObserverFunc(func(_ context.Context, event operation.Event) {
				events = append(events, event)
				if event.Phase == "authorize" && event.Code == "" {
					cancel()
				}
			})))
			definition := public("observer.cancellation.v1")
			if access == "protected" {
				definition.Access = operation.Permission("read")
				definition.Authorize = func(context.Context, operation.Principal, input) error { return nil }
			}
			called := false
			handle := register(t, registry, definition, func(ctx context.Context, in input) (output, error) {
				called = true
				return echo(ctx, in)
			})
			freeze(t, registry)
			out, err := handle.Invoke(parent, input{Name: "Ada"})
			if called || out != (output{}) || operation.CodeOf(err) != operation.Canceled {
				t.Fatalf("handler=%v output=%#v error=%v", called, out, err)
			}
			if len(events) != 3 || events[0].Phase != "validate" || events[1].Phase != "authorize" || events[2].Phase != "invoke" || events[2].Code != operation.Canceled {
				t.Fatalf("cancellation observations changed: %#v", events)
			}
		})
	}
}

func TestCancellationAfterCallbacksStopsLaterApplicationPhases(t *testing.T) {
	stages := []string{"validate", "resolve", "authorize", "handler", "output validation"}
	for _, observed := range []bool{false, true} {
		mode := "unobserved"
		if observed {
			mode = "observed"
		}
		for index, cancelAt := range stages {
			t.Run(mode+"/"+cancelAt, func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				parent = operation.WithPrincipal(parent, operation.Principal{ID: "actor"})
				var options []operation.Option
				if observed {
					options = append(options, operation.WithObserver(operation.ObserverFunc(func(context.Context, operation.Event) {})))
				}
				registry := operation.NewRegistry(nil, options...)
				var steps []string
				visit := func(stage string) {
					steps = append(steps, stage)
					if stage == cancelAt {
						cancel()
					}
				}
				definition := operation.Definition[input, output]{
					ID: "callback.cancellation.v1", Summary: "Callback cancellation", Access: operation.Permission("read"),
					Validate: func(context.Context, input) error {
						visit("validate")
						return nil
					},
					Resolve: func(ctx context.Context, _ input) (context.Context, error) {
						visit("resolve")
						return ctx, nil
					},
					Authorize: func(context.Context, operation.Principal, input) error {
						visit("authorize")
						return nil
					},
					ValidateOutput: func(context.Context, output) error {
						visit("output validation")
						return nil
					},
				}
				handle := register(t, registry, definition, func(ctx context.Context, in input) (output, error) {
					visit("handler")
					return echo(ctx, in)
				})
				freeze(t, registry)
				out, err := handle.Invoke(parent, input{Name: "Ada"})
				if operation.CodeOf(err) != operation.Canceled || out != (output{}) {
					t.Fatalf("output=%#v error=%v", out, err)
				}
				if want := stages[:index+1]; !reflect.DeepEqual(steps, want) {
					t.Fatalf("application phases = %v, want %v", steps, want)
				}
			})
		}
	}
}

func TestNativeOutputStillRejectsHandlerCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := operation.NewRegistry(nil)
	handle := register(t, registry, public("native.cancellation.v1"), func(ctx context.Context, in input) (output, error) {
		cancel()
		return echo(ctx, in)
	})
	freeze(t, registry)
	out, err := handle.Invoke(parent, input{Name: "Ada"})
	if operation.CodeOf(err) != operation.Canceled || out != (output{}) {
		t.Fatalf("canceled native result was accepted: %#v %v", out, err)
	}
}

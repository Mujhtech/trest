package operation_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mujhtech/trest/operation"
)

func TestPublicNativePreparationDoesNotRunHandler(t *testing.T) {
	registry := operation.NewRegistry(nil)
	called := false
	register(t, registry, public("native.prepare.v1"), func(ctx context.Context, in input) (output, error) {
		called = true
		return echo(ctx, in)
	})
	freeze(t, registry)
	prepared, err := registry.PrepareJSON(context.Background(), "native.prepare.v1", json.RawMessage(`{"name":"Ada"}`))
	if err != nil || called || string(prepared) != `{"name":"Ada"}` {
		t.Fatalf("prepared=%s handler=%v error=%v", prepared, called, err)
	}
	if _, err := registry.PrepareJSON(context.Background(), "native.prepare.v1", json.RawMessage(`{"name":"Ada","extra":1}`)); operation.CodeOf(err) != operation.InvalidInput || called {
		t.Fatalf("invalid wire input accepted: handler=%v error=%v", called, err)
	}
}

func TestPublicNativeOperationsKeepApplicationHooks(t *testing.T) {
	for _, mode := range []string{"invoke", "prepare"} {
		t.Run(mode, func(t *testing.T) {
			registry := operation.NewRegistry(nil)
			var steps []string
			definition := public("native.hooks.v1")
			definition.Validate = func(context.Context, input) error {
				steps = append(steps, "validate")
				return nil
			}
			definition.Resolve = func(ctx context.Context, _ input) (context.Context, error) {
				steps = append(steps, "resolve")
				return ctx, nil
			}
			definition.ValidateOutput = func(context.Context, output) error {
				steps = append(steps, "output validation")
				return nil
			}
			handle := register(t, registry, definition, func(ctx context.Context, in input) (output, error) {
				steps = append(steps, "handler")
				return echo(ctx, in)
			})
			freeze(t, registry)
			var err error
			want := []string{"validate", "resolve", "handler", "output validation"}
			if mode == "invoke" {
				_, err = handle.Invoke(context.Background(), input{Name: "Ada"})
			} else {
				_, err = registry.PrepareJSON(context.Background(), definition.ID, json.RawMessage(`{"name":"Ada"}`))
				want = want[:2]
			}
			if err != nil || !reflect.DeepEqual(steps, want) {
				t.Fatalf("steps=%v error=%v, want %v", steps, err, want)
			}
		})
	}
}

func TestPublicNativeInputConstraintsStillRejectBeforeHandler(t *testing.T) {
	type constrainedInput struct {
		Name string `json:"name" enum:"allowed"`
	}
	registry := operation.NewRegistry(nil)
	called := false
	handle, err := operation.Register(registry, operation.Definition[constrainedInput, output]{
		ID: "native.constraint.v1", Summary: "Constrained input", Access: operation.Public(),
	}, func(context.Context, constrainedInput) (output, error) {
		called = true
		return output{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	freeze(t, registry)
	if _, err := handle.Invoke(context.Background(), constrainedInput{Name: "denied"}); operation.CodeOf(err) != operation.InvalidInput || called {
		t.Fatalf("constraint bypassed: handler=%v error=%v", called, err)
	}
}

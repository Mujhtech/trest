package operation_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"trest.local/trest/operation"
)

func TestJSONBridgePreparesWithoutEffectsAndReauthorizes(t *testing.T) {
	allowed := true
	calls, authCalls := 0, 0
	r := operation.NewRegistry(operation.AuthorizerFunc(func(_ context.Context, p operation.Principal, _ string, input any) error {
		authCalls++
		if p.ID != "actor" || p.TenantID != "tenant" {
			t.Fatal("lost trusted identity")
		}
		if _, ok := input.(struct {
			Name string  `json:"name"`
			Note *string `json:"note,omitempty"`
		}); !ok {
			t.Fatal("lost typed input")
		}
		if !allowed {
			return operation.NewError(operation.Forbidden, "access denied", nil)
		}
		return nil
	}))
	type request = struct {
		Name string  `json:"name"`
		Note *string `json:"note,omitempty"`
	}
	_, err := operation.Register(r, operation.Definition[request, output]{ID: "write.v1", Summary: "Write once", Access: operation.Permission("write")}, func(_ context.Context, in request) (output, error) { calls++; return output{Name: in.Name}, nil })
	if err != nil {
		t.Fatal(err)
	}
	freeze(t, r)
	ctx := operation.WithPrincipal(context.Background(), operation.Principal{ID: "actor", TenantID: "tenant"})
	prepared, err := r.PrepareJSON(ctx, "write.v1", json.RawMessage(" { \"note\":null, \"name\":\"Ada\" } "))
	if err != nil || string(prepared) != `{"name":"Ada"}` || calls != 0 || authCalls != 1 {
		t.Fatalf("prepare = %s %v calls %d auth %d", prepared, err, calls, authCalls)
	}
	allowed = false
	if _, err := r.InvokeJSON(ctx, "write.v1", prepared); operation.CodeOf(err) != operation.Forbidden || calls != 0 {
		t.Fatalf("revocation failed: %v calls %d", err, calls)
	}
	allowed = true
	out, err := r.InvokeJSON(ctx, "write.v1", prepared)
	if err != nil || string(out) != `{"name":"Ada"}` || calls != 1 || authCalls != 3 {
		t.Fatalf("invoke = %s %v calls %d auth %d", out, err, calls, authCalls)
	}
}

func TestJSONBridgeRejectsInvalidBeforeHandler(t *testing.T) {
	r := operation.NewRegistry(nil, operation.WithJSONLimits(48, 48))
	register(t, r, public("echo.v1"), func(_ context.Context, in input) (output, error) { return output(in), nil })
	if _, err := r.InvokeJSON(context.Background(), "echo.v1", json.RawMessage(`{"name":"x"}`)); err == nil {
		t.Fatal("invoked unfrozen")
	}
	freeze(t, r)
	for _, tt := range []struct {
		name string
		data string
	}{
		{"missing", `{}`}, {"unknown", `{"name":"x","role":"admin"}`}, {"duplicate", `{"name":"x","name":"y"}`}, {"case", `{"Name":"x"}`}, {"trailing", `{"name":"x"} {}`}, {"large", `{"name":"` + strings.Repeat("x", 49) + `"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := r.PrepareJSON(context.Background(), "echo.v1", json.RawMessage(tt.data))
			if operation.CodeOf(err) != operation.InvalidInput {
				t.Fatalf("got %v", err)
			}
		})
	}
	if _, err := r.InvokeJSON(context.Background(), "absent.v1", json.RawMessage(`{}`)); operation.CodeOf(err) != operation.NotFound {
		t.Fatalf("got %v", err)
	}
}

func TestJSONBridgeOutputLimitDoesNotRepeatEffect(t *testing.T) {
	r := operation.NewRegistry(nil, operation.WithJSONLimits(256, 16))
	calls := 0
	register(t, r, public("large.v1"), func(context.Context, input) (output, error) {
		calls++
		return output{Name: strings.Repeat("x", 50)}, nil
	})
	freeze(t, r)
	_, err := r.InvokeJSON(context.Background(), "large.v1", json.RawMessage(`{"name":"x"}`))
	if operation.CodeOf(err) != operation.Internal || calls != 1 {
		t.Fatalf("got %v calls %d", err, calls)
	}
}

func TestJSONBridgeCanceledPreparation(t *testing.T) {
	r := operation.NewRegistry(nil)
	register(t, r, public("echo.v1"), echo)
	freeze(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.PrepareJSON(ctx, "echo.v1", json.RawMessage(`{"name":"x"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

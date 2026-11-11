package operation_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mujhtech/trest/operation"
)

type panickingWire string

func (v panickingWire) MarshalJSON() ([]byte, error) { return json.Marshal(string(v)) }
func (*panickingWire) UnmarshalJSON([]byte) error    { panic("private decoder detail") }
func (panickingWire) TrestSchema() json.RawMessage   { return json.RawMessage(`{"type":"string"}`) }

type invalidatingWire string

func (v invalidatingWire) MarshalJSON() ([]byte, error) { return json.Marshal(string(v)) }
func (v *invalidatingWire) UnmarshalJSON([]byte) error  { *v = "invalid decoded value"; return nil }
func (invalidatingWire) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","enum":["valid"]}`)
}

func TestValidWireCannotBypassDecodedCodecValidation(t *testing.T) {
	t.Parallel()
	type input struct {
		Value invalidatingWire `json:"value"`
	}
	authorized, called := 0, 0
	registry := operation.NewRegistry(nil)
	handle, err := operation.Register(registry, operation.Definition[input, struct{}]{
		ID: "decoded.validation.v1", Summary: "Decoded codec validation", Access: operation.Permission("read"),
		Authorize: func(context.Context, operation.Principal, input) error { authorized++; return nil },
	}, func(context.Context, input) (struct{}, error) { called++; return struct{}{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Freeze(); err != nil {
		t.Fatal(err)
	}
	ctx := operation.WithPrincipal(context.Background(), operation.Principal{ID: "verified"})
	data := json.RawMessage(`{"value":"valid"}`)
	if err = handle.ValidateJSON(data); err != nil {
		t.Fatalf("valid wire input failed: %v", err)
	}
	for _, invoke := range []func(context.Context, string, json.RawMessage) (json.RawMessage, error){registry.PrepareJSON, registry.InvokeJSON} {
		if _, err = invoke(ctx, "decoded.validation.v1", data); operation.CodeOf(err) != operation.InvalidInput {
			t.Fatalf("invalid decoded value accepted: %v", err)
		}
	}
	if _, err = handle.Invoke(ctx, input{Value: "invalid direct value"}); operation.CodeOf(err) != operation.InvalidInput {
		t.Fatalf("invalid direct codec value accepted: %v", err)
	}
	if authorized != 0 || called != 0 {
		t.Fatalf("invalid decoded value reached authorization=%d handler=%d", authorized, called)
	}
}

func TestCustomDecoderPanicIsInputFailureBeforeExecution(t *testing.T) {
	t.Parallel()
	type input struct {
		Value panickingWire `json:"value"`
	}
	registry := operation.NewRegistry(nil)
	calls := 0
	_, err := operation.Register(registry, operation.Definition[input, struct{}]{ID: "codec.v1", Summary: "Codec", Access: operation.Public()}, func(context.Context, input) (struct{}, error) { calls++; return struct{}{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Freeze(); err != nil {
		t.Fatal(err)
	}
	for _, invoke := range []func(context.Context, string, json.RawMessage) (json.RawMessage, error){registry.PrepareJSON, registry.InvokeJSON} {
		_, err := invoke(context.Background(), "codec.v1", json.RawMessage(`{"value":"x"}`))
		if operation.CodeOf(err) != operation.InvalidInput || strings.Contains(operation.PublicError(err).Message, "private") {
			t.Fatalf("decoder failure: %v", err)
		}
	}
	if calls != 0 {
		t.Fatal("decoder failure executed handler")
	}
}

func TestOutputConstraintsAlwaysApplyToDirectCalls(t *testing.T) {
	t.Parallel()
	type result struct {
		Value string `json:"value" enum:"safe"`
	}
	registry := operation.NewRegistry(nil)
	handle, err := operation.Register(registry, operation.Definition[struct{}, result]{ID: "output.v1", Summary: "Output", Access: operation.Public()}, func(context.Context, struct{}) (result, error) { return result{Value: "private"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Freeze(); err != nil {
		t.Fatal(err)
	}
	out, err := handle.Invoke(context.Background(), struct{}{})
	if operation.CodeOf(err) != operation.Internal || out.Value != "" {
		t.Fatalf("unvalidated direct output: %#v %v", out, err)
	}
}

func TestResolverCannotDropInvocationDeadlineOrIdentity(t *testing.T) {
	t.Parallel()
	registry := operation.NewRegistry(nil)
	definition := operation.Definition[struct{}, struct{}]{ID: "resolved.v1", Summary: "Resolved", Access: operation.Permission("read"), Timeout: 10 * time.Millisecond,
		Resolve: func(context.Context, struct{}) (context.Context, error) { return context.Background(), nil },
		Authorize: func(_ context.Context, p operation.Principal, _ struct{}) error {
			if p.ID != "verified" {
				return errors.New("identity lost")
			}
			return nil
		},
	}
	handle, err := operation.Register(registry, definition, func(ctx context.Context, _ struct{}) (struct{}, error) { <-ctx.Done(); return struct{}{}, ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Freeze(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(operation.WithPrincipal(context.Background(), operation.Principal{ID: "verified"}), time.Second)
	defer cancel()
	_, err = handle.Invoke(ctx, struct{}{})
	if operation.CodeOf(err) != operation.DeadlineExceeded {
		t.Fatalf("deadline/identity lost: %v", err)
	}
}

func TestResolverCannotReplaceInvocationMetadata(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"direct", "prepare", "json"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			type resourceKey struct{}
			type parentValueKey struct{}
			verified := operation.Principal{ID: "verified", TenantID: "tenant"}
			var invocationTrace operation.TraceContext
			var events []operation.Event
			registry := operation.NewRegistry(nil, operation.WithObserver(operation.ObserverFunc(func(_ context.Context, event operation.Event) {
				events = append(events, event)
			})))
			authorized, called := false, false
			definition := operation.Definition[struct{}, struct{}]{
				ID: "resolved.metadata.v1", Summary: "Resolved metadata", Access: operation.Permission("read"),
				Resolve: func(ctx context.Context, _ struct{}) (context.Context, error) {
					invocationTrace = operation.TraceFromContext(ctx)
					resolved := operation.WithPrincipal(context.Background(), operation.Principal{ID: "forged", TenantID: "other"})
					resolved = operation.WithTrace(resolved, operation.TraceContext{TraceID: "forged", SpanID: "forged"})
					resolved = operation.WithCorrelationID(resolved, "forged")
					return context.WithValue(resolved, resourceKey{}, "resolved resource"), nil
				},
				Authorize: func(ctx context.Context, principal operation.Principal, _ struct{}) error {
					authorized = true
					if principal != verified {
						t.Errorf("authorization principal = %#v, want %#v", principal, verified)
						return nil // A policy trusting the forged principal would allow execution.
					}
					if got := operation.TraceFromContext(ctx); got != invocationTrace {
						t.Errorf("authorization trace = %#v, want %#v", got, invocationTrace)
					}
					if got := operation.CorrelationID(ctx); got != "request" {
						t.Errorf("authorization correlation = %q, want request", got)
					}
					if ctx.Value(resourceKey{}) != "resolved resource" || ctx.Value(parentValueKey{}) != "parent value" {
						t.Error("resolver or parent application value was lost")
					}
					return operation.NewError(operation.Forbidden, "access denied", nil)
				},
			}
			handle, err := operation.Register(registry, definition, func(context.Context, struct{}) (struct{}, error) {
				called = true
				return struct{}{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = registry.Freeze(); err != nil {
				t.Fatal(err)
			}
			ctx := operation.WithPrincipal(context.Background(), verified)
			ctx = operation.WithTrace(ctx, operation.TraceContext{TraceID: "trusted", SpanID: "parent"})
			ctx = operation.WithCorrelationID(ctx, "request")
			ctx = context.WithValue(ctx, parentValueKey{}, "parent value")
			switch mode {
			case "direct":
				_, err = handle.Invoke(ctx, struct{}{})
			case "prepare":
				_, err = registry.PrepareJSON(ctx, definition.ID, json.RawMessage(`{}`))
			case "json":
				_, err = registry.InvokeJSON(ctx, definition.ID, json.RawMessage(`{}`))
			}
			if operation.CodeOf(err) != operation.Forbidden || !authorized || called {
				t.Fatalf("authorization result: error=%v authorized=%t handler called=%t", err, authorized, called)
			}
			if invocationTrace.TraceID != "trusted" || invocationTrace.ParentSpanID == "" || invocationTrace.SpanID == "" || (mode == "direct" && invocationTrace.ParentSpanID != "parent") {
				t.Fatalf("invocation trace = %#v", invocationTrace)
			}
			for _, event := range events {
				expectedTrace := invocationTrace
				if strings.HasPrefix(event.Phase, "json_") {
					expectedTrace = operation.TraceContext{TraceID: "trusted", SpanID: invocationTrace.ParentSpanID, ParentSpanID: "parent"}
				}
				if event.PrincipalID != verified.ID || event.TenantID != verified.TenantID || event.TraceID != expectedTrace.TraceID || event.SpanID != expectedTrace.SpanID || event.ParentSpanID != expectedTrace.ParentSpanID || event.CorrelationID != "request" {
					t.Errorf("resolver changed observed metadata: %#v", event)
				}
			}
		})
	}
}

func TestResolverCannotInjectAnonymousInvocationMetadata(t *testing.T) {
	t.Parallel()
	registry := operation.NewRegistry(nil)
	var invocationTrace operation.TraceContext
	definition := operation.Definition[struct{}, struct{}]{
		ID: "resolved.anonymous.v1", Summary: "Anonymous resolution", Access: operation.Public(),
		Resolve: func(ctx context.Context, _ struct{}) (context.Context, error) {
			invocationTrace = operation.TraceFromContext(ctx)
			resolved := operation.WithPrincipal(context.Background(), operation.Principal{ID: "forged"})
			resolved = operation.WithTrace(resolved, operation.TraceContext{TraceID: "forged", SpanID: "forged"})
			return operation.WithCorrelationID(resolved, "forged"), nil
		},
	}
	handle, err := operation.Register(registry, definition, func(ctx context.Context, _ struct{}) (struct{}, error) {
		if principal, ok := operation.PrincipalFromContext(ctx); ok || principal.ID != "" {
			t.Errorf("resolver injected a principal: %#v", principal)
		}
		if got := operation.CorrelationID(ctx); got != "" {
			t.Errorf("resolver injected a correlation ID: %q", got)
		}
		if got := operation.TraceFromContext(ctx); got != invocationTrace {
			t.Errorf("resolver replaced invocation trace: %#v", got)
		}
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Freeze(); err != nil {
		t.Fatal(err)
	}
	if _, err = handle.Invoke(context.Background(), struct{}{}); err != nil {
		t.Fatal(err)
	}
}

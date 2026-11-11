package operation_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mujhtech/trest/operation"
)

type input struct {
	Name string `json:"name"`
}
type output struct {
	Name string `json:"name"`
}

func register(t *testing.T, r *operation.Registry, d operation.Definition[input, output], f func(context.Context, input) (output, error)) *operation.Handle[input, output] {
	t.Helper()
	h, err := operation.Register(r, d, f)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func freeze(t *testing.T, r *operation.Registry) {
	t.Helper()
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
}
func echo(_ context.Context, in input) (output, error) { return output(in), nil }
func public(id string) operation.Definition[input, output] {
	return operation.Definition[input, output]{ID: id, Summary: "Echo a name", Access: operation.Public()}
}

func TestRegisterDiagnostics(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*operation.Definition[input, output])
		code   string
	}{
		{"missing access", func(d *operation.Definition[input, output]) { d.Access = operation.Access{} }, "invalid_definition"},
		{"empty permission", func(d *operation.Definition[input, output]) { d.Access = operation.Permission("") }, "invalid_definition"},
		{"missing authorizer", func(d *operation.Definition[input, output]) { d.Access = operation.Permission("write") }, "invalid_definition"},
		{"invalid id", func(d *operation.Definition[input, output]) { d.ID = "not a stable id" }, "invalid_definition"},
		{"missing summary", func(d *operation.Definition[input, output]) { d.Summary = "" }, "invalid_definition"},
		{"negative timeout", func(d *operation.Definition[input, output]) { d.Timeout = -time.Second }, "invalid_definition"},
		{"invalid side effect", func(d *operation.Definition[input, output]) { d.SideEffect = "magic" }, "invalid_definition"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := public("echo.v1")
			tt.mutate(&d)
			r := operation.NewRegistry(nil)
			_, err := operation.Register(r, d, echo)
			var diagnostic *operation.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Code != tt.code {
				t.Fatalf("diagnostic = %v", err)
			}
			if len(r.Operations()) != 0 {
				t.Fatal("failed registration mutated registry")
			}
		})
	}
	t.Run("unsupported schema", func(t *testing.T) {
		r := operation.NewRegistry(nil)
		_, err := operation.Register(r, operation.Definition[map[int]string, output]{ID: "bad.v1", Summary: "Bad schema", Access: operation.Public()}, func(context.Context, map[int]string) (output, error) { return output{}, nil })
		var diagnostic *operation.Diagnostic
		if !errors.As(err, &diagnostic) || diagnostic.Code != "unsupported_schema" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("typed nil authorizer", func(t *testing.T) {
		var authorizer operation.AuthorizerFunc
		r := operation.NewRegistry(authorizer)
		d := public("echo.v1")
		d.Access = operation.Permission("write")
		if _, err := operation.Register(r, d, echo); err == nil {
			t.Fatal("accepted nil authorizer")
		}
	})
}

func TestRegistryLifecycleAndDetachedMetadata(t *testing.T) {
	r := operation.NewRegistry(nil)
	h := register(t, r, public("z.v1"), echo)
	register(t, r, public("a.v1"), echo)
	if _, err := h.Invoke(context.Background(), input{}); err == nil {
		t.Fatal("invoked before freeze")
	}
	if _, err := operation.Register(r, public("z.v1"), echo); err == nil {
		t.Fatal("duplicate accepted")
	}
	freeze(t, r)
	freeze(t, r)
	if _, err := operation.Register(r, public("late.v1"), echo); err == nil {
		t.Fatal("late registration accepted")
	}
	ops := r.Operations()
	if ops[0].ID != "a.v1" || ops[1].ID != "z.v1" {
		t.Fatalf("not sorted: %v", ops)
	}
	ops[0].InputSchema[0] = '!'
	m := h.Metadata()
	m.InputSchema[0] = '!'
	lookup, _ := r.Lookup("a.v1")
	lookup.InputSchema[0] = '!'
	if r.Operations()[0].InputSchema[0] == '!' || h.Metadata().InputSchema[0] == '!' {
		t.Fatal("metadata alias")
	}
	out, err := h.Invoke(context.Background(), input{Name: "Ada"})
	if err != nil || out.Name != "Ada" {
		t.Fatalf("invoke = %+v, %v", out, err)
	}
}

func TestFreezeRejectsInvalidLimits(t *testing.T) {
	for _, tt := range []struct {
		name string
		opt  operation.Option
	}{
		{"zero concurrency", operation.WithMaxConcurrent(0)},
		{"negative concurrency", operation.WithMaxConcurrent(-1)},
		{"zero timeout", operation.WithDefaultTimeout(0)},
		{"nil option", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := operation.NewRegistry(nil, tt.opt)
			if err := r.Freeze(); err == nil {
				t.Fatal("accepted invalid config")
			}
			if r.IsFrozen() {
				t.Fatal("froze invalid config")
			}
		})
	}
}

func TestInvokeEnforcement(t *testing.T) {
	for _, tt := range []struct {
		name      string
		principal operation.Principal
		invalid   bool
		denied    bool
		want      operation.Code
		expected  []string
	}{
		{"anonymous denied", operation.Principal{}, false, false, operation.Unauthenticated, []string{"validate"}},
		{"validation stops authorization", operation.Principal{ID: "u1", TenantID: "a"}, true, false, operation.InvalidInput, []string{"validate"}},
		{"permission denied", operation.Principal{ID: "u1", TenantID: "a"}, false, true, operation.Forbidden, []string{"validate", "authorize"}},
		{"allowed", operation.Principal{ID: "u1", TenantID: "a"}, false, false, "", []string{"validate", "authorize", "handler"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var steps []string
			r := operation.NewRegistry(operation.AuthorizerFunc(func(ctx context.Context, p operation.Principal, permission string, in any) error {
				steps = append(steps, "authorize")
				cp, ok := operation.PrincipalFromContext(ctx)
				if !ok || cp != p || p != tt.principal || permission != "echo:read" || in.(input).Name != "Ada" {
					t.Fatal("authorization identity/input changed")
				}
				if tt.denied {
					return errors.New("private policy detail")
				}
				return nil
			}))
			d := public("echo.v1")
			d.Access = operation.Permission("echo:read")
			d.Validate = func(_ context.Context, in input) error {
				steps = append(steps, "validate")
				if tt.invalid {
					return &operation.Error{Code: operation.InvalidInput, Message: "name is invalid", Fields: []operation.FieldError{{Path: "/name", Code: "invalid", Message: "invalid name"}}}
				}
				return nil
			}
			h := register(t, r, d, func(ctx context.Context, in input) (output, error) {
				steps = append(steps, "handler")
				p, ok := operation.PrincipalFromContext(ctx)
				if !ok || p != tt.principal {
					t.Fatal("handler tenant lost")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("deadline not set")
				}
				return echo(ctx, in)
			})
			freeze(t, r)
			ctx := operation.WithPrincipal(context.Background(), tt.principal)
			_, err := h.Invoke(ctx, input{Name: "Ada"})
			if operation.CodeOf(err) != tt.want {
				t.Fatalf("error %v, want %s", err, tt.want)
			}
			if strings.Join(steps, ",") != strings.Join(tt.expected, ",") {
				t.Fatalf("steps %v want %v", steps, tt.expected)
			}
			if tt.denied && strings.Contains(operation.PublicError(err).Message, "private") {
				t.Fatal("leaked policy detail")
			}
			if tt.invalid && operation.PublicError(err).Fields[0].Path != "/name" {
				t.Fatal("field path lost")
			}
		})
	}
}

func TestInvokePublicAndMapError(t *testing.T) {
	sentinel := errors.New("private database conflict")
	r := operation.NewRegistry(operation.AuthorizerFunc(func(context.Context, operation.Principal, string, any) error {
		t.Fatal("public operation authorized")
		return nil
	}))
	d := public("echo.v1")
	d.MapError = func(err error) error {
		if errors.Is(err, sentinel) {
			return operation.NewError(operation.Conflict, "name already exists", nil)
		}
		return nil
	}
	h := register(t, r, d, func(context.Context, input) (output, error) { return output{}, sentinel })
	freeze(t, r)
	_, err := h.Invoke(context.Background(), input{})
	if !errors.Is(err, sentinel) || operation.CodeOf(err) != operation.Conflict {
		t.Fatalf("lost mapped/original error: %v", err)
	}
	if operation.PublicError(err).Message != "name already exists" {
		t.Fatalf("wrong public error: %v", err)
	}
}

func TestInvokeCancellationAndDeadline(t *testing.T) {
	t.Run("pre canceled", func(t *testing.T) {
		r := operation.NewRegistry(nil)
		h := register(t, r, public("echo.v1"), func(context.Context, input) (output, error) { t.Fatal("canceled call ran"); return output{}, nil })
		freeze(t, r)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := h.Invoke(ctx, input{})
		if !errors.Is(err, context.Canceled) || operation.CodeOf(err) != operation.Canceled {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("deadline propagated", func(t *testing.T) {
		r := operation.NewRegistry(nil, operation.WithDefaultTimeout(5*time.Millisecond))
		h := register(t, r, public("echo.v1"), func(ctx context.Context, _ input) (output, error) { <-ctx.Done(); return output{}, ctx.Err() })
		freeze(t, r)
		_, err := h.Invoke(context.Background(), input{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("authorization cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := operation.NewRegistry(operation.AuthorizerFunc(func(context.Context, operation.Principal, string, any) error { cancel(); return nil }))
		d := public("echo.v1")
		d.Access = operation.Permission("read")
		h := register(t, r, d, func(context.Context, input) (output, error) {
			t.Fatal("handler ran after auth cancellation")
			return output{}, nil
		})
		freeze(t, r)
		_, err := h.Invoke(operation.WithPrincipal(ctx, operation.Principal{ID: "u"}), input{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestInvokeCapacityAndRelease(t *testing.T) {
	r := operation.NewRegistry(nil, operation.WithMaxConcurrent(1))
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	h := register(t, r, public("echo.v1"), func(ctx context.Context, in input) (output, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return echo(ctx, in)
	})
	freeze(t, r)
	done := make(chan error, 1)
	go func() { _, err := h.Invoke(context.Background(), input{}); done <- err }()
	<-entered
	_, err := h.Invoke(context.Background(), input{})
	close(release)
	if operation.CodeOf(err) != operation.RateLimited {
		t.Fatalf("got %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke(context.Background(), input{}); err != nil {
		t.Fatal("capacity not released", err)
	}
}

func TestInvokeOutputValidation(t *testing.T) {
	r := operation.NewRegistry(nil)
	d := public("echo.v1")
	d.ValidateOutput = func(context.Context, output) error { return errors.New("secret output detail") }
	h := register(t, r, d, echo)
	freeze(t, r)
	_, err := h.Invoke(context.Background(), input{})
	if operation.CodeOf(err) != operation.Internal || operation.PublicError(err).Message != "internal error" {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrentInvokeAndMetadata(t *testing.T) {
	r := operation.NewRegistry(nil)
	h := register(t, r, public("echo.v1"), echo)
	freeze(t, r)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if _, err := h.Invoke(context.Background(), input{}); err != nil {
					t.Error(err)
				}
				r.Operations()
				r.Lookup("echo.v1")
				h.Metadata()
			}
		}()
	}
	wg.Wait()
}

package operation_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mujhtech/trest/operation"
)

func TestPrincipalContextPreservesIdentityAndLifetime(t *testing.T) {
	t.Parallel()
	type applicationKey struct{}
	deadline := time.Now().Add(time.Minute)
	parent, stop := context.WithDeadline(context.WithValue(context.Background(), applicationKey{}, "value"), deadline)
	defer stop()
	parent, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	identity := operation.Principal{ID: "actor", TenantID: "tenant"}
	ctx := operation.WithPrincipal(parent, identity)
	identity.ID = "changed"
	got, ok := operation.PrincipalFromContext(ctx)
	if !ok || got.ID != "actor" || got.TenantID != "tenant" {
		t.Fatalf("identity changed after construction: %+v %v", got, ok)
	}
	got.TenantID = "changed"
	if got, _ := operation.PrincipalFromContext(ctx); got.TenantID != "tenant" {
		t.Fatal("accessor exposed mutable identity")
	}
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) || ctx.Done() != parent.Done() || ctx.Value(applicationKey{}) != "value" {
		t.Fatal("context lifetime or application value was lost")
	}
	cause := errors.New("canceled by application")
	cancel(cause)
	if ctx.Err() != context.Canceled || !errors.Is(context.Cause(ctx), cause) {
		t.Fatalf("cancellation/cause lost: %v %v", ctx.Err(), context.Cause(ctx))
	}
}

func TestPrincipalContextShadowingAndConcurrentReads(t *testing.T) {
	t.Parallel()
	want := operation.Principal{ID: "actor", TenantID: "tenant"}
	parent := operation.WithPrincipal(context.Background(), want)
	for _, identity := range []operation.Principal{{}, {TenantID: "anonymous-tenant"}, {ID: "child", TenantID: "other"}} {
		child := operation.WithPrincipal(parent, identity)
		got, ok := operation.PrincipalFromContext(child)
		if got != identity || ok != (identity.ID != "") {
			t.Fatalf("child did not shadow identity: %+v %v", got, ok)
		}
		if got, _ := operation.PrincipalFromContext(parent); got != want {
			t.Fatal("child changed parent identity")
		}
	}
	if got, ok := operation.PrincipalFromContext(context.Background()); got != (operation.Principal{}) || ok {
		t.Fatal("missing identity became authenticated")
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				got, ok := operation.PrincipalFromContext(parent)
				if !ok || got != want {
					t.Error("identity changed during concurrent reads")
					return
				}
				got.ID = "local-copy"
			}
		}()
	}
	wg.Wait()
}

func TestPrincipalContextRejectsNilParent(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil parent accepted")
		}
	}()
	operation.WithPrincipal(nil, operation.Principal{}) //nolint:staticcheck // Verify nil-parent rejection.
}

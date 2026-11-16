package operation_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/mujhtech/trest/internal/invocationctx"
	"github.com/mujhtech/trest/operation"
)

type invocationFrameContext struct {
	context.Context
	slot    invocationctx.Slot
	lookups atomic.Int32
}

func (c *invocationFrameContext) Value(key any) any {
	if invocationctx.IsSlotKey(key) {
		c.lookups.Add(1)
		return &c.slot
	}
	return c.Context.Value(key)
}

func TestRejectedInvocationDoesNotClaimRequestFrame(t *testing.T) {
	for _, rejection := range []string{"canceled", "capacity"} {
		t.Run(rejection, func(t *testing.T) {
			registry := operation.NewRegistry(nil, operation.WithMaxConcurrent(1))
			entered, release := make(chan struct{}), make(chan struct{})
			handle := register(t, registry, public("frame.rejection.v1"), func(ctx context.Context, in input) (output, error) {
				if in.Name == "occupy" {
					close(entered)
					<-release
				}
				return echo(ctx, in)
			})
			freeze(t, registry)
			parent := context.Background()
			want := operation.RateLimited
			if rejection == "canceled" {
				var cancel context.CancelFunc
				parent, cancel = context.WithCancel(parent)
				cancel()
				want = operation.Canceled
			} else {
				done := make(chan struct{})
				go func() {
					defer close(done)
					if _, err := handle.Invoke(context.Background(), input{Name: "occupy"}); err != nil {
						t.Error(err)
					}
				}()
				<-entered
				defer func() { close(release); <-done }()
			}
			request := &invocationFrameContext{Context: parent}
			if _, err := handle.Invoke(request, input{}); operation.CodeOf(err) != want {
				t.Fatalf("rejection = %v, want %v", err, want)
			}
			if request.lookups.Load() != 0 {
				t.Fatal("rejected invocation looked up request-owned storage")
			}
		})
	}
}

func TestResolverCannotClaimAnotherRequestFrame(t *testing.T) {
	registry := operation.NewRegistry(nil)
	foreign := &invocationFrameContext{Context: context.Background()}
	original := &invocationFrameContext{Context: context.Background()}
	child := register(t, registry, public("frame.child.v1"), echo)
	definition := public("frame.parent.v1")
	definition.Resolve = func(context.Context, input) (context.Context, error) { return foreign, nil }
	parent := register(t, registry, definition, func(ctx context.Context, in input) (output, error) {
		return child.Invoke(ctx, in)
	})
	freeze(t, registry)
	if _, err := parent.Invoke(original, input{Name: "Ada"}); err != nil {
		t.Fatal(err)
	}
	if foreign.lookups.Load() != 0 || original.lookups.Load() < 2 {
		t.Fatal("resolver redirected request-owned invocation storage")
	}
}

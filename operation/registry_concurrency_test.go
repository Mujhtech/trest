package operation_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mujhtech/trest/operation"
)

func TestFreezePublishesRegisteredHandle(t *testing.T) {
	registry := operation.NewRegistry(nil)
	wait, stopWaiting := context.WithTimeout(context.Background(), time.Second)
	defer stopWaiting()
	var handle *operation.Handle[input, output]
	const readers = 16
	var ready, done sync.WaitGroup
	ready.Add(readers)
	done.Add(readers)
	for range readers {
		go func() {
			defer done.Done()
			ready.Done()
			for !registry.IsFrozen() {
				select {
				case <-wait.Done():
					t.Error("Freeze did not publish registry")
					return
				default:
				}
				runtime.Gosched()
			}
			// The successful IsFrozen observation publishes both registration
			// and the caller's associated handle before invocation starts.
			if _, err := handle.Invoke(context.Background(), input{Name: "Ada"}); err != nil {
				t.Error(err)
			}
			metadata, ok := registry.Lookup("published.v1")
			if !ok || len(metadata.InputSchema) == 0 || len(metadata.OutputSchema) == 0 {
				t.Error("frozen registry did not publish complete metadata")
			}
		}()
	}
	ready.Wait()
	handle = register(t, registry, public("published.v1"), echo)
	freeze(t, registry)
	done.Wait()
}

func TestInvocationCapacityUnderContention(t *testing.T) {
	for _, mode := range invocationContextModes {
		t.Run(mode.name, func(t *testing.T) {
			const capacity, workers = 8, 64
			parent := context.Background()
			if mode.cancelable {
				var cancel context.CancelFunc
				parent, cancel = context.WithCancel(parent)
				defer cancel()
			}
			options := []operation.Option{operation.WithMaxConcurrent(capacity)}
			if mode.observed {
				options = append(options, operation.WithObserver(operation.ObserverFunc(func(context.Context, operation.Event) {})))
			}
			registry := operation.NewRegistry(nil, options...)
			entered := make(chan struct{}, workers)
			release := make(chan struct{})
			releaseAll := sync.OnceFunc(func() { close(release) })
			defer releaseAll()
			var calls, active, peak atomic.Int32
			handle := register(t, registry, public("capacity.v1"), func(ctx context.Context, in input) (output, error) {
				calls.Add(1)
				n := active.Add(1)
				defer active.Add(-1)
				for previous := peak.Load(); n > previous; previous = peak.Load() {
					if peak.CompareAndSwap(previous, n) {
						break
					}
				}
				entered <- struct{}{}
				<-release
				return echo(ctx, in)
			})
			freeze(t, registry)
			start := make(chan struct{})
			results := make(chan error, workers)
			for range workers {
				go func() {
					<-start
					_, err := handle.Invoke(parent, input{})
					results <- err
				}()
			}
			close(start)
			for range capacity {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("available capacity was not admitted")
				}
			}
			// Admitted handlers are still blocked, so every remaining caller
			// must be rejected without entering application code.
			for range workers - capacity {
				select {
				case err := <-results:
					if operation.CodeOf(err) != operation.RateLimited {
						t.Fatalf("full registry returned %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("excess invocation waited for capacity")
				}
			}
			if got := calls.Load(); got != capacity || peak.Load() != capacity {
				t.Fatalf("handler calls=%d peak=%d, want %d", got, peak.Load(), capacity)
			}
			releaseAll()
			for range capacity {
				if err := <-results; err != nil {
					t.Fatalf("admitted invocation failed: %v", err)
				}
			}
			if _, err := handle.Invoke(parent, input{}); err != nil {
				t.Fatalf("capacity was not released: %v", err)
			}
		})
	}
}

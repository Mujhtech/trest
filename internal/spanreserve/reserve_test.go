package spanreserve

import (
	"context"
	"sync"
	"testing"
)

type testContext struct {
	context.Context
	reservation Reservation
}

func (c *testContext) Value(k any) any {
	if IsKey(k) {
		return &c.reservation
	}
	return c.Context.Value(k)
}

func TestTakeReturnsReservationOnceAcrossContextWrappers(t *testing.T) {
	t.Parallel()
	ctx := &testContext{Context: context.Background(), reservation: New("reserved")}
	type applicationKey struct{}
	wrapped := context.WithValue(ctx, applicationKey{}, "value")
	if got := Take(wrapped); got != "reserved" {
		t.Fatalf("first claim = %q", got)
	}
	if got := Take(ctx); got != "" {
		t.Fatalf("second claim reused %q", got)
	}
	if wrapped.Value(applicationKey{}) != "value" || ctx.reservation.id != "reserved" {
		t.Fatal("claim changed immutable context values")
	}
}

func TestTakeConcurrentCallsHaveOneWinner(t *testing.T) {
	t.Parallel()
	ctx := &testContext{Context: context.Background(), reservation: New("reserved")}
	const workers = 64
	results := make(chan string, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- Take(ctx)
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for got := range results {
		if got == "reserved" {
			winners++
		} else if got != "" {
			t.Fatalf("unexpected reservation %q", got)
		}
	}
	if winners != 1 {
		t.Fatalf("reservation had %d consumers, want 1", winners)
	}
}

func TestTakeEmptyAndShadowedReservations(t *testing.T) {
	t.Parallel()
	parent := &testContext{Context: context.Background(), reservation: New("parent")}
	child := &testContext{Context: parent}
	if Take(context.Background()) != "" || Take(child) != "" || Take(child) != "" {
		t.Fatal("absent or empty reservation returned an ID")
	}
	if got := Take(parent); got != "parent" {
		t.Fatalf("empty child consumed parent reservation: %q", got)
	}
}

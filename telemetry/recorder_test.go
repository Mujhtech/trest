package telemetry_test

import (
	"context"
	"sync"
	"testing"

	"trest.local/trest/operation"
	"trest.local/trest/telemetry"
)

func TestRecorderBoundsAndDetachedSnapshots(t *testing.T) {
	r, err := telemetry.NewRecorder(3)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		r.Observe(context.Background(), operation.Event{TraceID: id, Phase: "invoke"})
	}
	if _, ok := r.Trace("a"); ok {
		t.Fatal("old event retained")
	}
	traces := r.Traces()
	if len(traces) != 3 || traces[0].ID != "b" {
		t.Fatalf("traces %+v", traces)
	}
	traces[0].Events[0].Phase = "modified"
	trace, _ := r.Trace("b")
	if trace.Events[0].Phase != "invoke" {
		t.Fatal("snapshot aliases recorder")
	}
}

func TestRecorderConcurrentAndMultiObserver(t *testing.T) {
	r, _ := telemetry.NewRecorder(16)
	observer := telemetry.MultiObserver(operation.ObserverFunc(func(context.Context, operation.Event) { panic("failure") }), r)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				observer.Observe(context.Background(), operation.Event{TraceID: "a"})
				r.Traces()
			}
		}()
	}
	wg.Wait()
	trace, ok := r.Trace("a")
	if !ok || len(trace.Events) != 16 {
		t.Fatalf("trace %+v", trace)
	}
}

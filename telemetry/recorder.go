// Package telemetry provides optional, bounded, metadata-only invocation traces.
// Exporters remain application-owned; importing this package starts no workers.
package telemetry

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/mujhtech/trest/operation"
)

// Trace contains retained completion events for one trace ID. The recorder keeps
// a bounded event window, so a trace may be partial after older events are evicted.
type Trace struct {
	ID     string            `json:"id"`
	Events []operation.Event `json:"events"`
}

// Recorder is a concurrency-safe ring of metadata events. It stores at most the
// configured number of events. No request/response body or raw error is stored.
// Applications must restrict access to identity and architecture metadata.
type Recorder struct {
	mu       sync.RWMutex
	events   []operation.Event
	capacity int
	next     int
}

// NewRecorder constructs an in-memory recorder with capacity measured in events.
// Capacities outside 1..100000 are rejected to catch accidental unbounded use.
func NewRecorder(capacity int) (*Recorder, error) {
	if capacity < 1 || capacity > 100000 {
		return nil, errors.New("telemetry: capacity must be between 1 and 100000 events")
	}
	return &Recorder{capacity: capacity, events: make([]operation.Event, 0, capacity)}, nil
}

// Observe implements operation.Observer. It never performs external IO.
func (r *Recorder) Observe(_ context.Context, event operation.Event) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) < r.capacity {
		r.events = append(r.events, event)
		return
	}
	r.events[r.next] = event
	r.next = (r.next + 1) % r.capacity
}

func (r *Recorder) snapshot() []operation.Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]operation.Event, 0, len(r.events))
	result = append(result, r.events[r.next:]...)
	result = append(result, r.events[:r.next]...)
	return result
}

// Traces returns detached traces in ID order; events retain completion order.
func (r *Recorder) Traces() []Trace {
	grouped := make(map[string][]operation.Event)
	for _, event := range r.snapshot() {
		grouped[event.TraceID] = append(grouped[event.TraceID], event)
	}
	result := make([]Trace, 0, len(grouped))
	for id, events := range grouped {
		result = append(result, Trace{ID: id, Events: events})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Trace retrieves the retained events for id without exposing internal storage.
func (r *Recorder) Trace(id string) (Trace, bool) {
	trace := Trace{ID: id}
	for _, event := range r.snapshot() {
		if event.TraceID == id {
			trace.Events = append(trace.Events, event)
		}
	}
	return trace, len(trace.Events) > 0
}

// MultiObserver forwards each event to the configured observers in order.
// A failing observer cannot prevent later observers from receiving it.
func MultiObserver(observers ...operation.Observer) operation.Observer {
	copied := append([]operation.Observer(nil), observers...)
	return operation.ObserverFunc(func(ctx context.Context, event operation.Event) {
		for _, observer := range copied {
			if observer != nil {
				func() { defer func() { _ = recover() }(); observer.Observe(ctx, event) }()
			}
		}
	})
}

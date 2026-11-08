package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type testInvoker struct{}

func (testInvoker) PrepareJSON(_ context.Context, _ string, input json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
func (testInvoker) InvokeJSON(_ context.Context, _ string, input json.RawMessage) (json.RawMessage, error) {
	return input, nil
}

func TestNewBounds(t *testing.T) {
	t.Parallel()
	if _, err := New(nil, testInvoker{}, Options{}); err == nil {
		t.Fatal("nil db accepted")
	}
	if _, err := New(&sql.DB{}, nil, Options{}); err == nil {
		t.Fatal("nil invoker accepted")
	}
	for _, tt := range []struct {
		name    string
		options Options
	}{
		{"lease", Options{Lease: time.Nanosecond}},
		{"attempts", Options{DefaultMaxAttempts: 101}},
		{"input", Options{MaxInputBytes: -1}},
		{"backoff", Options{BaseBackoff: time.Hour, MaxBackoff: time.Second}},
		{"retention", Options{Retention: time.Nanosecond}},
		{"version", Options{PayloadVersions: map[string]string{"x": ""}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(&sql.DB{}, testInvoker{}, tt.options); err == nil {
				t.Fatal("invalid options accepted")
			}
		})
	}
}

func TestPrepareBoundsAndCanonicalHash(t *testing.T) {
	t.Parallel()
	s, err := New(&sql.DB{}, testInvoker{}, Options{MaxInputBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	r := Request{OperationID: "test.v1", PayloadVersion: "1", CallerScope: "test", Input: json.RawMessage(`{"b":2,"a":1}`)}
	first, err := s.prepare(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.Input = json.RawMessage(`{"a":1,"b":2}`)
	second, err := s.prepare(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if first.InputHash != second.InputHash {
		t.Fatal("canonical duplicate changed hash")
	}
	r.Input = json.RawMessage(`"` + strings.Repeat("x", 101) + `"`)
	if _, err := s.prepare(context.Background(), r); err == nil {
		t.Fatal("oversized input accepted")
	}
	r.Input = json.RawMessage(`{}`)
	r.PayloadVersion = "2"
	if _, err := s.prepare(context.Background(), r); err == nil {
		t.Fatal("unsupported version accepted")
	}
}

func TestRetryDelayBounds(t *testing.T) {
	t.Parallel()
	s, err := New(&sql.DB{}, testInvoker{}, Options{BaseBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		attempt int
		max     time.Duration
	}{{1, 10 * time.Millisecond}, {2, 20 * time.Millisecond}, {3, 40 * time.Millisecond}, {100000, 40 * time.Millisecond}} {
		for range 25 {
			delay, err := s.retryDelay(tt.attempt)
			if err != nil {
				t.Fatal(err)
			}
			if delay < tt.max/2 || delay > tt.max {
				t.Fatalf("attempt %d delay %v not bounded by %v", tt.attempt, delay, tt.max)
			}
		}
	}
}

package trest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"trest.local/trest/internal/schema"
)

// StreamInfo describes the typed data carried by a server-sent event stream.
type StreamInfo struct {
	Format string          `json:"format"`
	Schema json.RawMessage `json:"schema"`
}

// WebSocketInfo describes JSON message bodies from the client's perspective.
// Input is sent by the client; Output is received from the server.
type WebSocketInfo struct {
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
}

// WebSocketMessages declares typed JSON message metadata for a native upgrade.
// The adapter must enforce these schemas when reading and writing each message;
// the optional WebSocket adapter's NewJSON constructor does that automatically.
func WebSocketMessages[I, O any](route *RawRoute) *RawRoute {
	if route == nil {
		return nil
	}
	c := *route
	c.protocol = "websocket"
	in, err := Schema[I]()
	if err != nil {
		c.configError = err
		return &c
	}
	out, err := Schema[O]()
	if err != nil {
		c.configError = err
		return &c
	}
	c.messages = &WebSocketInfo{Input: in, Output: out}
	return &c
}

// Events is a typed server-sent event response created by EventStream.
// It is HTTP-only; data schemas are shared by event validation and clients.
type Events[T any] struct {
	send func(context.Context, *JSONEventWriter[T]) error
}

// EventStream creates a typed JSON event stream. The callback owns production
// and honors the request context; writes provide synchronous backpressure.
func EventStream[T any](send func(context.Context, *JSONEventWriter[T]) error) Events[T] {
	return Events[T]{send: send}
}

// JSONEventWriter validates each typed data value before framing an SSE message.
// Existing SSE size limits, cancellation, flushing, and write deadlines apply.
type JSONEventWriter[T any] struct {
	writer    *EventWriter
	validator *schema.Validator
}

// JSONEvent carries typed data and optional SSE reconnection metadata. Clients
// expose Retry but never automatically replay requests or reconnect.
type JSONEvent[T any] struct {
	ID, Event string
	Data      T
	Retry     time.Duration
}

// Send writes one validated JSON event. Empty event uses the SSE default type;
// id may be empty when the application does not assign event identifiers.
func (w *JSONEventWriter[T]) Send(id, event string, value T) error {
	return w.Write(JSONEvent[T]{ID: id, Event: event, Data: value})
}

// Write emits one typed event, including an optional retry interval.
func (w *JSONEventWriter[T]) Write(message JSONEvent[T]) error {
	if w == nil || w.writer == nil || w.validator == nil {
		return errors.New("trest: event writer unavailable")
	}
	data, err := json.Marshal(message.Data)
	if err != nil {
		return err
	}
	issues, err := w.validator.Validate(data)
	if err != nil {
		return err
	}
	if len(issues) > 0 {
		return errors.New("trest: event violates its response schema")
	}
	return w.writer.Send(Message{ID: message.ID, Event: message.Event, Data: string(data), Retry: message.Retry})
}

// Heartbeat sends a comment without producing a data event.
func (w *JSONEventWriter[T]) Heartbeat() error {
	if w == nil || w.writer == nil {
		return errors.New("trest: event writer unavailable")
	}
	return w.writer.Heartbeat()
}

type typedEventStream interface {
	eventType() reflect.Type
	eventReply(*schema.Validator) Reply
}

func (Events[T]) eventType() reflect.Type { return reflect.TypeFor[T]() }
func (e Events[T]) eventReply(validator *schema.Validator) Reply {
	if e.send == nil {
		return Reply{err: errors.New("trest: nil event stream callback")}
	}
	return SSE(func(ctx context.Context, writer *EventWriter) error {
		return e.send(ctx, &JSONEventWriter[T]{writer: writer, validator: validator})
	})
}

package trestws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"unicode/utf8"

	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/internal/schema"
)

// MessageError describes a rejected JSON message. Direction is input or output;
// Fields contains safe structural violations and never includes message values
// or custom codec errors. A rejected message closes the connection immediately.
type MessageError struct {
	Direction string
	Fields    []trest.FieldError
}

func (e *MessageError) Error() string {
	if e == nil {
		return "websocket: invalid JSON message"
	}
	return "websocket: " + e.Direction + " message violates its contract"
}

// NewJSON constructs a typed text-message route with the same authentication,
// origin policy, deadlines, capacity, and shutdown behavior as New. Input and
// output contracts are compiled once and included in the OpenAPI extension.
// The application must still authorize resources referenced by each message.
func NewJSON[I, O any](path string, options Options, handler func(context.Context, *JSONConn[I, O]) error) (trest.Component, error) {
	if handler == nil {
		return nil, errors.New("websocket: JSON handler required")
	}
	input, err := compileMessage(reflect.TypeFor[I]())
	if err != nil {
		return nil, fmt.Errorf("websocket: input contract: %w", err)
	}
	output, err := compileMessage(reflect.TypeFor[O]())
	if err != nil {
		return nil, fmt.Errorf("websocket: output contract: %w", err)
	}
	endpoint, err := newEndpoint(options, func(ctx context.Context, conn *Conn) error {
		return handler(ctx, &JSONConn[I, O]{conn: conn, input: input, output: output})
	})
	if err != nil {
		return nil, err
	}
	route := trest.WebSocketMessages[I, O](endpoint.route(path))
	return trest.Group("", trest.Lifecycle(endpoint.start, endpoint.stop), route), nil
}

func compileMessage(typ reflect.Type) (*schema.Validator, error) {
	spec, err := schema.Generate(typ)
	if err != nil {
		return nil, err
	}
	return schema.Compile(spec)
}

// JSONConn exchanges the declared input and output models as JSON text frames.
// Use one reader. Writes are synchronous with bounded backpressure and may be
// concurrent. Messages carry no implicit permission or delivery guarantee.
type JSONConn[I, O any] struct {
	conn          *Conn
	input, output *schema.Validator
}

// Read returns one validated value. Duplicate/unknown fields, malformed JSON,
// constraint failures, custom codec failures, and binary frames close the socket
// before a value can reach the application. A zero value is returned on failure.
func (c *JSONConn[I, O]) Read(ctx context.Context) (I, error) {
	var zero I
	if c == nil || c.conn == nil || c.input == nil || ctx == nil {
		return zero, errors.New("websocket: JSON connection and context required")
	}
	kind, data, err := c.conn.Read(ctx)
	if err != nil {
		return zero, err
	}
	if kind != Text || !utf8.Valid(data) {
		return zero, c.reject("input", nil)
	}
	if fields, valid := messageValid(c.input, data); !valid {
		return zero, c.reject("input", fields)
	}
	var value I
	if err := decodeMessage(data, &value); err != nil {
		return zero, c.reject("input", nil)
	}
	// Validate the decoded representation too: a custom codec cannot normalize
	// valid wire data into a value that violates the same input contract.
	encoded, err := encodeMessage(value)
	if err != nil || int64(len(encoded)) > c.conn.maxBytes {
		return zero, c.reject("input", nil)
	}
	if fields, valid := messageValid(c.input, encoded); !valid {
		return zero, c.reject("input", fields)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := c.conn.ctx.Err(); err != nil {
		return zero, err
	}
	return value, nil
}

// Write validates the encoded output and its byte limit before sending any
// frame. Invalid output closes the socket without transmitting diagnostic text.
func (c *JSONConn[I, O]) Write(ctx context.Context, value O) error {
	if c == nil || c.conn == nil || c.output == nil || ctx == nil {
		return errors.New("websocket: JSON connection and context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := encodeMessage(value)
	if err != nil || int64(len(data)) > c.conn.maxBytes {
		return c.reject("output", nil)
	}
	if fields, valid := messageValid(c.output, data); !valid {
		return c.reject("output", fields)
	}
	return c.conn.Write(ctx, Text, data)
}

// Close immediately terminates the connection and unblocks pending I/O.
func (c *JSONConn[I, O]) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (c *JSONConn[I, O]) reject(direction string, fields []trest.FieldError) error {
	_ = c.Close()
	return &MessageError{Direction: direction, Fields: fields}
}

func messageValid(validator *schema.Validator, data []byte) ([]trest.FieldError, bool) {
	issues, err := validator.Validate(data)
	if err != nil {
		return nil, false
	}
	if len(issues) == 0 {
		return nil, true
	}
	fields := make([]trest.FieldError, len(issues))
	for i, issue := range issues {
		fields[i] = trest.FieldError{Path: issue.Path, Code: issue.Code, Message: issue.Message}
	}
	return fields, false
}

func decodeMessage(data []byte, target any) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("websocket: JSON decoder panicked")
		}
	}()
	return json.Unmarshal(data, target)
}

func encodeMessage(value any) (data []byte, err error) {
	defer func() {
		if recover() != nil {
			data = nil
			err = errors.New("websocket: JSON encoder panicked")
		}
	}()
	return json.Marshal(value)
}

package operation

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// WithJSONLimits sets positive input/output byte limits on the dynamic JSON
// bridge used by jobs and tools. Typed application calls retain their own values;
// HTTP has independent protocol limits. Defaults are 1 MiB in each direction.
func WithJSONLimits(inputBytes, outputBytes int) Option {
	return func(c *registryConfig) { c.maxInputBytes = inputBytes; c.maxOutputBytes = outputBytes }
}

type binding struct {
	prepare func(context.Context, json.RawMessage) (json.RawMessage, error)
	invoke  func(context.Context, json.RawMessage) (json.RawMessage, error)
}

func (r *Registry) binding(id string) (binding, error) {
	if r == nil {
		return binding{}, NewError(Internal, "invalid registry", nil)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.isFrozen {
		return binding{}, &Diagnostic{Code: "registry_not_frozen", OperationID: id, Message: "call Freeze before invocation"}
	}
	b, ok := r.bindings[id]
	if !ok {
		return binding{}, NewError(NotFound, "operation not found", nil)
	}
	if r.operations[id].JSONDisabled {
		return binding{}, NewError(InvalidInput, "operation requires native invocation", nil)
	}
	return b, nil
}

// PrepareJSON canonicalizes typed input and enforces validation and authorization
// without executing the handler. It grants no future authority: an adapter must
// invoke through InvokeJSON after a pause so permissions and resource state are
// checked again. Validators and authorizers must not cause privileged effects.
func (r *Registry) PrepareJSON(ctx context.Context, id string, data json.RawMessage) (json.RawMessage, error) {
	b, err := r.binding(id)
	if err != nil {
		return nil, err
	}
	return r.observeJSON(ctx, id, "json_prepare", data, b.prepare)
}

// InvokeJSON is the protocol bridge for adapters. It checks the exact registered
// input type and enters the same invoker as typed calls. Output encoding failures
// can occur after side effects; adapters must not blindly retry such failures.
// Application panics propagate so an adapter can record uncertain execution.
func (r *Registry) InvokeJSON(ctx context.Context, id string, data json.RawMessage) (json.RawMessage, error) {
	b, err := r.binding(id)
	if err != nil {
		return nil, err
	}
	return r.observeJSON(ctx, id, "json_invoke", data, b.invoke)
}

func (h *Handle[I, O]) decodeJSON(data json.RawMessage) (I, error) {
	var in I
	if len(data) > h.registry.maxInputBytes {
		return in, NewError(InvalidInput, "input exceeds size limit", nil)
	}
	if err := h.ValidateJSON(data); err != nil {
		return in, err
	}
	if err := unmarshalInput(data, &in); err != nil {
		return in, NewError(InvalidInput, "input does not match operation type", err)
	}
	return in, nil
}

func unmarshalInput(data []byte, value any) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("custom input decoder panicked")
		}
	}()
	return json.Unmarshal(data, value)
}
func (h *Handle[I, O]) prepareJSON(ctx context.Context, data json.RawMessage) (json.RawMessage, error) {
	in, err := h.decodeJSON(data)
	if err != nil {
		return nil, err
	}
	if _, err = h.execute(ctx, in, false); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(in)
	if err != nil {
		return nil, NewError(InvalidInput, "input cannot be encoded as JSON", err)
	}
	if len(encoded) > h.registry.maxInputBytes {
		return nil, NewError(InvalidInput, "canonical input exceeds size limit", nil)
	}
	return encoded, nil
}
func (h *Handle[I, O]) invokeJSON(ctx context.Context, data json.RawMessage) (json.RawMessage, error) {
	in, err := h.decodeJSON(data)
	if err != nil {
		return nil, err
	}
	out, err := h.Invoke(ctx, in)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, NewError(Internal, "output cannot be encoded as JSON", err)
	}
	if len(encoded) > h.registry.maxOutputBytes {
		return nil, NewError(Internal, "output exceeds size limit", nil)
	}
	return encoded, nil
}

// observeJSON includes decoding/encoding outcomes outside the typed handler span.
// A successful handler followed by an encoding error remains visible as both.
func (r *Registry) observeJSON(ctx context.Context, id, phase string, data json.RawMessage, call func(context.Context, json.RawMessage) (json.RawMessage, error)) (out json.RawMessage, err error) {
	if ctx == nil {
		return nil, NewError(Internal, "nil invocation context", nil)
	}
	ctx, err = newInvocationTrace(ctx)
	if err != nil {
		return nil, NewError(Internal, "could not start invocation", err)
	}
	if r.observer == nil {
		return call(ctx, data)
	}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			r.observe(ctx, id, phase, started, Internal)
			panic(recovered)
		}
		r.observe(ctx, id, phase, started, CodeOf(err))
	}()
	return call(ctx, data)
}

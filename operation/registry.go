// Package operation defines typed business capabilities and enforces their
// validation, authorization, concurrency, and cooperative execution deadlines.
package operation

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mujhtech/trest/internal/schema"
)

// Definition is the immutable contract captured by Register.
// Validate runs before authorization and must not perform privileged side effects.
// MapError translates handler errors into approved public errors; it does not
// translate validation, authorization, or execution-limit failures.
type Definition[I, O any] struct {
	ID             string
	Summary        string
	Access         Access
	SideEffect     string
	Timeout        time.Duration
	Validate       func(context.Context, I) error
	ValidateOutput func(context.Context, O) error
	MapError       func(error) error
	// Resolve attaches request values after validation and before authorization.
	// It also runs during PrepareJSON, so it must not cause privileged effects.
	// Only values from its returned context are used; the invoker retains its
	// original identity, trace, correlation ID, deadline, and cancellation.
	// A nil context is an invocation error.
	Resolve func(context.Context, I) (context.Context, error)
	// Authorize overrides the registry authorizer for this operation. It is
	// captured at registration, so all transports enforce the same policy.
	Authorize func(context.Context, Principal, I) error
	// JSONDisabled marks native inputs/results (uploads, streams, raw responses)
	// that cannot be safely invoked through a persisted JSON tool/job envelope.
	JSONDisabled bool
	// Source optionally identifies a higher-level route declaration. Absolute
	// paths are reduced to basenames before they enter metadata.
	Source SourceLocation
}

// Metadata describes an operation without exposing its executable binding.
type Metadata struct {
	ID           string          `json:"id"`
	Source       SourceLocation  `json:"source"`
	Summary      string          `json:"summary"`
	Access       Access          `json:"access"`
	SideEffect   string          `json:"side_effect,omitempty"`
	Timeout      time.Duration   `json:"timeout_ns"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	JSONDisabled bool            `json:"json_disabled,omitempty"`
}

func cloneMetadata(m Metadata) Metadata {
	m.InputSchema = append(json.RawMessage(nil), m.InputSchema...)
	m.OutputSchema = append(json.RawMessage(nil), m.OutputSchema...)
	return m
}

// Registry owns explicit registrations. Construct with NewRegistry, register all
// operations, then Freeze before constructing adapters or invoking operations.
// Registry methods are safe for concurrent use; registries must not be copied.
type Registry struct {
	mu             sync.RWMutex
	operations     map[string]Metadata
	bindings       map[string]binding
	observer       Observer
	maxInputBytes  int
	maxOutputBytes int
	authorizer     Authorizer
	defaultTimeout time.Duration
	maxConcurrent  int
	slots          chan struct{}
	isFrozen       bool
	configError    string
}

// Option configures invocation bounds during NewRegistry construction.
// Its private configuration target prevents applying options to a live registry.
type Option func(*registryConfig)

type registryConfig struct {
	observer       Observer
	maxInputBytes  int
	maxOutputBytes int
	defaultTimeout time.Duration
	maxConcurrent  int
	configError    string
}

// WithDefaultTimeout sets a positive default cooperative execution deadline.
func WithDefaultTimeout(timeout time.Duration) Option {
	return func(c *registryConfig) { c.defaultTimeout = timeout }
}

// WithMaxConcurrent sets a positive maximum for all concurrent invocations.
// Excess work is rejected immediately rather than queued.
func WithMaxConcurrent(n int) Option { return func(c *registryConfig) { c.maxConcurrent = n } }

// NewRegistry constructs a registry without starting any goroutines.
// Passing nil permits only public operations. Invalid options fail at Freeze.
func NewRegistry(authorizer Authorizer, options ...Option) *Registry {
	c := &registryConfig{defaultTimeout: 30 * time.Second, maxConcurrent: 128, maxInputBytes: 1 << 20, maxOutputBytes: 1 << 20}
	for _, option := range options {
		if option == nil {
			c.configError = "nil option"
			continue
		}
		option(c)
	}
	return &Registry{
		operations: make(map[string]Metadata), bindings: make(map[string]binding), authorizer: authorizer,
		observer: c.observer, maxInputBytes: c.maxInputBytes, maxOutputBytes: c.maxOutputBytes,
		defaultTimeout: c.defaultTimeout, maxConcurrent: c.maxConcurrent,
		configError: c.configError,
	}
}

var validID = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z0-9_-]+)*$`)

// Register captures a definition and typed handler, rejecting invalid contracts
// before any traffic can reach them. Registration never invokes application code.
func Register[I, O any](r *Registry, def Definition[I, O], handler func(context.Context, I) (O, error)) (*Handle[I, O], error) {
	fail := func(code, path, message string) (*Handle[I, O], error) {
		return nil, &Diagnostic{Code: code, OperationID: def.ID, Path: path, Message: message}
	}
	if r == nil {
		return fail("invalid_config", "registry", "use NewRegistry before registering operations")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isFrozen {
		return fail("registry_frozen", "registry", "register operations before calling Freeze")
	}
	if r.operations == nil {
		return fail("invalid_config", "registry", "construct the registry with NewRegistry")
	}
	if !validID.MatchString(def.ID) {
		return fail("invalid_definition", "id", "use a lowercase dotted identifier, such as invoices.create.v1")
	}
	if _, exists := r.operations[def.ID]; exists {
		return fail("duplicate_operation", "id", "choose a unique operation id")
	}
	if strings.TrimSpace(def.Summary) == "" {
		return fail("invalid_definition", "summary", "provide an operation summary")
	}
	if handler == nil {
		return fail("invalid_definition", "handler", "provide a handler")
	}
	switch def.Access.Mode {
	case "public":
		if def.Access.Permission != "" {
			return fail("invalid_definition", "access", "public access cannot declare a permission")
		}
	case "permission":
		if strings.TrimSpace(def.Access.Permission) == "" {
			return fail("invalid_definition", "access", "provide a nonempty permission")
		}
		if isNil(r.authorizer) && def.Authorize == nil {
			return fail("invalid_definition", "access", "provide an authorizer for protected operations")
		}
	default:
		return fail("invalid_definition", "access", "declare Public() or Permission(name) explicitly")
	}
	if def.SideEffect != "" && def.SideEffect != "read" && def.SideEffect != "write" {
		return fail("invalid_definition", "side_effect", "use read, write, or an empty declaration")
	}
	if def.Timeout < 0 {
		return fail("invalid_definition", "timeout", "use a positive timeout or zero for the default")
	}
	input, err := schema.Generate(reflect.TypeOf((*I)(nil)).Elem())
	if err != nil {
		return fail("unsupported_schema", "input", err.Error())
	}
	output, err := schema.Generate(reflect.TypeOf((*O)(nil)).Elem())
	if err != nil {
		return fail("unsupported_schema", "output", err.Error())
	}
	timeout := def.Timeout
	if timeout == 0 {
		timeout = r.defaultTimeout
	}
	metadata := Metadata{ID: def.ID, Summary: def.Summary, Access: def.Access, SideEffect: def.SideEffect, Timeout: timeout, InputSchema: input, OutputSchema: output, JSONDisabled: def.JSONDisabled}
	validator, err := schema.Compile(input)
	if err != nil {
		return fail("unsupported_schema", "input", err.Error())
	}
	outputValidator, err := schema.Compile(output)
	if err != nil {
		return fail("unsupported_schema", "output", err.Error())
	}
	if pc, file, line, ok := runtime.Caller(1); ok {
		metadata.Source = SourceLocation{File: filepath.Base(file), Line: line}
		if fn := runtime.FuncForPC(pc); fn != nil {
			metadata.Source.Function = fn.Name()
		}
	}
	if def.Source.File != "" && def.Source.Line > 0 {
		metadata.Source = def.Source
		metadata.Source.File = filepath.Base(def.Source.File)
	}
	r.operations[def.ID] = metadata
	h := &Handle[I, O]{registry: r, definition: def, handler: handler, metadata: metadata, validator: validator,
		inputValueValidator: validator.ForType(reflect.TypeFor[I]()), outputValidator: outputValidator.ForType(reflect.TypeFor[O]())}
	r.bindings[def.ID] = binding{prepare: h.prepareJSON, invoke: h.invokeJSON}
	return h, nil
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	}
	return false
}

// Freeze validates registry settings and permanently seals registrations.
// Calling Freeze again succeeds without changing the registry.
func (r *Registry) Freeze() error {
	if r == nil {
		return &Diagnostic{Code: "invalid_config", Message: "use NewRegistry"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isFrozen {
		return nil
	}
	if r.operations == nil || r.defaultTimeout <= 0 || r.maxConcurrent <= 0 || r.maxInputBytes <= 0 || r.maxOutputBytes <= 0 || r.configError != "" {
		return &Diagnostic{Code: "invalid_config", Message: "construct with NewRegistry and use positive timeout/concurrency/json limit options; nil options are invalid"}
	}
	r.slots = make(chan struct{}, r.maxConcurrent)
	r.isFrozen = true
	return nil
}

// IsFrozen reports whether registration is sealed and invocation is permitted.
func (r *Registry) IsFrozen() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isFrozen
}

// Operations returns a detached snapshot sorted by operation ID.
func (r *Registry) Operations() []Metadata {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Metadata, 0, len(r.operations))
	for _, metadata := range r.operations {
		result = append(result, cloneMetadata(metadata))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Lookup returns detached metadata for a registered operation.
func (r *Registry) Lookup(id string) (Metadata, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	metadata, ok := r.operations[id]
	return cloneMetadata(metadata), ok
}

// Handle binds a checked contract to its typed application function.
// A Handle must come from Register. Its zero value cannot be invoked.
type Handle[I, O any] struct {
	registry            *Registry
	definition          Definition[I, O]
	handler             func(context.Context, I) (O, error)
	metadata            Metadata
	validator           *schema.Validator
	inputValueValidator *schema.ValueValidator
	outputValidator     *schema.ValueValidator
}

// Metadata returns a detached copy of this operation's contract.
func (h *Handle[I, O]) Metadata() Metadata {
	if h == nil {
		return Metadata{}
	}
	return cloneMetadata(h.metadata)
}

// Registry returns the registry that owns this handle.
func (h *Handle[I, O]) Registry() *Registry {
	if h == nil {
		return nil
	}
	return h.registry
}

// ValidateJSON checks wire input against the same structure used for contracts.
// This performs structural validation only; Invoke enforces domain and access rules.
func (h *Handle[I, O]) ValidateJSON(data []byte) error {
	if h == nil || h.registry == nil {
		return NewError(Internal, "invalid operation handle", nil)
	}
	violations, err := h.validator.Validate(data)
	if err != nil {
		return NewError(InvalidInput, "invalid JSON input", err)
	}
	return inputViolations(violations)
}

func inputViolations(violations []schema.Violation) error {
	if len(violations) == 0 {
		return nil
	}
	fields := make([]FieldError, 0, len(violations))
	for _, v := range violations {
		fields = append(fields, FieldError{Path: v.Path, Code: v.Code, Message: v.Message})
	}
	return &Error{Code: InvalidInput, Message: "input does not match the operation contract", Fields: fields}
}

func invocationError(err error) error {
	if err == nil {
		return nil
	}
	return NewError(CodeOf(err), PublicError(err).Message, err)
}

// Invoke executes the shared enforcement path. Deadlines are cooperative: the
// handler and hooks must honor ctx. Trest does not abandon work in a goroutine or
// roll back side effects if the caller cancels. Hooks must be concurrency-safe.
func (h *Handle[I, O]) Invoke(ctx context.Context, in I) (O, error) {
	return h.execute(ctx, in, true)
}

func (h *Handle[I, O]) execute(ctx context.Context, in I, runHandler bool) (result O, resultErr error) {
	var zero O
	if h == nil || h.registry == nil || h.handler == nil {
		return zero, NewError(Internal, "invalid operation handle", nil)
	}
	r := h.registry
	if !r.IsFrozen() {
		return zero, &Diagnostic{Code: "registry_not_frozen", OperationID: h.metadata.ID, Message: "call Freeze before invocation"}
	}
	if ctx == nil {
		return zero, NewError(Internal, "nil invocation context", nil)
	}
	var traceErr error
	ctx, traceErr = newInvocationTrace(ctx)
	if traceErr != nil {
		return zero, NewError(Internal, "could not start invocation", traceErr)
	}
	if r.observer == nil {
		return h.run(ctx, in, runHandler)
	}
	phase := "invoke"
	if !runHandler {
		phase = "prepare"
	}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			r.observe(ctx, h.metadata.ID, phase, started, Internal)
			panic(recovered)
		}
		r.observe(ctx, h.metadata.ID, phase, started, CodeOf(resultErr))
	}()
	return h.run(ctx, in, runHandler)
}

func (h *Handle[I, O]) run(ctx context.Context, in I, runHandler bool) (O, error) {
	var zero O
	r := h.registry
	if err := ctx.Err(); err != nil {
		return zero, invocationError(err)
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return zero, NewError(RateLimited, "invocation capacity exceeded", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, h.metadata.Timeout)
	defer cancel()
	started := r.startPhase()
	if err := h.validate(ctx, in); err != nil {
		r.observe(ctx, h.metadata.ID, "validate", started, CodeOf(err))
		return zero, err
	}
	r.observe(ctx, h.metadata.ID, "validate", started, "")
	// Reject anonymous protected invocations before running resource resolvers.
	// Resource authorization still follows resolution and uses the resolved value.
	if h.metadata.Access.Mode == "permission" {
		if _, ok := PrincipalFromContext(ctx); !ok {
			err := NewError(Unauthenticated, "authentication required", nil)
			r.observe(ctx, h.metadata.ID, "authorize", r.startPhase(), Unauthenticated)
			return zero, err
		}
	}
	if h.definition.Resolve != nil {
		started = r.startPhase()
		resolved, err := h.definition.Resolve(ctx, in)
		if ctx.Err() != nil {
			err = invocationError(ctx.Err())
		}
		if err == nil && resolved == nil {
			err = NewError(Internal, "request resolver returned no context", nil)
		}
		r.observe(ctx, h.metadata.ID, "resolve", started, CodeOf(err))
		if err != nil {
			return zero, err
		}
		ctx = resolvedContext{Context: ctx, values: resolved}
	}
	started = r.startPhase()
	if err := h.authorize(ctx, in); err != nil {
		r.observe(ctx, h.metadata.ID, "authorize", started, CodeOf(err))
		return zero, err
	}
	r.observe(ctx, h.metadata.ID, "authorize", started, "")
	if !runHandler {
		return zero, nil
	}
	if err := ctx.Err(); err != nil {
		return zero, invocationError(err)
	}
	started = r.startPhase()
	out, err := h.call(ctx, in)
	r.observe(ctx, h.metadata.ID, "handler", started, CodeOf(err))
	if err != nil {
		return zero, err
	}
	return out, nil
}

// A resolver can attach application values, but the invocation owns framework
// metadata, cancellation, and its deadline even for an unrelated returned context.
type resolvedContext struct {
	context.Context
	values context.Context
}

func (c resolvedContext) Value(key any) any {
	switch key.(type) {
	case principalKey, traceKey, correlationKey:
		return c.Context.Value(key)
	}
	if value := c.values.Value(key); value != nil {
		return value
	}
	return c.Context.Value(key)
}

func (h *Handle[I, O]) validate(ctx context.Context, in I) error {
	violations, err := h.inputValueValidator.Validate(in)
	if err != nil {
		return NewError(InvalidInput, "input cannot be encoded as JSON", err)
	}
	if err = inputViolations(violations); err != nil {
		return err
	}
	if h.definition.Validate != nil {
		if err = h.definition.Validate(ctx, in); err != nil {
			if ctx.Err() != nil {
				return invocationError(ctx.Err())
			}
			var classified *Error
			if errors.As(err, &classified) && classified != nil {
				return err
			}
			return NewError(InvalidInput, "input validation failed", err)
		}
	}
	return invocationError(ctx.Err())
}

func (h *Handle[I, O]) authorize(ctx context.Context, in I) error {
	if err := ctx.Err(); err != nil {
		return invocationError(err)
	}
	if h.metadata.Access.Mode == "permission" {
		principal, ok := PrincipalFromContext(ctx)
		if !ok {
			return NewError(Unauthenticated, "authentication required", nil)
		}
		var err error
		if h.definition.Authorize != nil {
			err = h.definition.Authorize(ctx, principal, in)
		} else {
			err = h.registry.authorizer.Authorize(ctx, principal, h.metadata.Access.Permission, in)
		}
		if err != nil {
			if ctx.Err() != nil {
				return invocationError(ctx.Err())
			}
			switch CodeOf(err) {
			case Unauthenticated, Forbidden, NotFound, Unavailable:
				return err
			}
			return NewError(Forbidden, "access denied", err)
		}
	}
	return invocationError(ctx.Err())
}

func (h *Handle[I, O]) call(ctx context.Context, in I) (O, error) {
	var zero O
	out, err := h.handler(ctx, in)
	if ctx.Err() != nil {
		return zero, invocationError(ctx.Err())
	}
	if err != nil {
		if h.definition.MapError != nil {
			if mapped := h.definition.MapError(err); mapped != nil {
				err = errors.Join(mapped, err)
			}
		}
		return zero, err
	}
	issues, err := h.outputValidator.Validate(out)
	if err != nil {
		return zero, NewError(Internal, "output cannot be encoded as JSON", err)
	}
	if len(issues) > 0 {
		return zero, NewError(Internal, "output does not match the operation contract", err)
	}
	if h.definition.ValidateOutput != nil {
		if err = h.definition.ValidateOutput(ctx, out); err != nil {
			return zero, NewError(Internal, "output validation failed", err)
		}
	}
	if ctx.Err() != nil {
		return zero, invocationError(ctx.Err())
	}
	return out, nil
}

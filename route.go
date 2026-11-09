package trest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"

	"trest.local/trest/internal/schema"
	"trest.local/trest/internal/webinput"
	"trest.local/trest/operation"
)

// Parameter describes an HTTP input in the application contract.
type Parameter struct {
	Name     string          `json:"name"`
	In       string          `json:"in"`
	Required bool            `json:"required"`
	Schema   json.RawMessage `json:"schema"`
}

// RouteInfo is detached, public-safe metadata from the compiled application.
type RouteInfo struct {
	Name            string                   `json:"name"`
	OperationID     string                   `json:"operation_id,omitempty"`
	Method          string                   `json:"method"`
	Path            string                   `json:"path"`
	Summary         string                   `json:"summary"`
	Permission      string                   `json:"permission,omitempty"`
	Authenticated   bool                     `json:"authenticated"`
	Status          int                      `json:"status"`
	Responses       []ResponseInfo           `json:"responses,omitempty"`
	Parameters      []Parameter              `json:"parameters,omitempty"`
	BodySchema      json.RawMessage          `json:"body_schema,omitempty"`
	BodyContentType string                   `json:"body_content_type,omitempty"`
	BodyRequired    bool                     `json:"body_required,omitempty"`
	OutputSchema    json.RawMessage          `json:"output_schema,omitempty"`
	ContentType     string                   `json:"content_type,omitempty"`
	NativeOnly      bool                     `json:"native_only,omitempty"`
	Protocol        string                   `json:"protocol,omitempty"`
	Stream          *StreamInfo              `json:"stream,omitempty"`
	WebSocket       *WebSocketInfo           `json:"websocket,omitempty"`
	Source          operation.SourceLocation `json:"source"`
	streamValidator *schema.Validator
}

// Routes returns detached route metadata in deterministic path/method order.
func (a *App) Routes() []RouteInfo {
	data, _ := json.Marshal(a.routes)
	var copy []RouteInfo
	_ = json.Unmarshal(data, &copy)
	return copy
}

// Route retains concrete input and output types through construction. Its fluent
// configuration methods return copies, so a route can be reused in separate apps.
type Route[I, O any] struct {
	method, path, name, summary, permission, effect string
	status                                          int
	statuses                                        []int
	timeout                                         time.Duration
	handler                                         func(context.Context, I) (O, error)
	validate                                        func(context.Context, I) error
	authorize                                       func(context.Context, Principal, I) error
	mapError                                        func(error) error
	resolvers                                       []func(context.Context, I) (context.Context, error)
	after                                           []func(context.Context, I, O) (O, error)
	contentType                                     string
	permissionSet                                   bool
	configError                                     error
	source                                          operation.SourceLocation
}

// Get declares a typed GET route. Inputs use path/query/header/cookie tags.
func Get[I, O any](path string, fn func(context.Context, I) (O, error)) *Route[I, O] {
	return newRoute("GET", path, fn)
}

// Post declares a typed POST route. Untagged request structs are JSON bodies.
func Post[I, O any](path string, fn func(context.Context, I) (O, error)) *Route[I, O] {
	return newRoute("POST", path, fn)
}

// Put declares a typed PUT route.
func Put[I, O any](path string, fn func(context.Context, I) (O, error)) *Route[I, O] {
	return newRoute("PUT", path, fn)
}

// Patch declares a typed PATCH route.
func Patch[I, O any](path string, fn func(context.Context, I) (O, error)) *Route[I, O] {
	return newRoute("PATCH", path, fn)
}

// Delete declares a typed DELETE route.
func Delete[I, O any](path string, fn func(context.Context, I) (O, error)) *Route[I, O] {
	return newRoute("DELETE", path, fn)
}

// Head declares a typed HEAD route. It shares normal response metadata but sends no body.
func Head[I, O any](path string, fn func(context.Context, I) (O, error)) *Route[I, O] {
	return newRoute("HEAD", path, fn)
}

// Options declares an explicit OPTIONS route, overriding automatic preflight handling.
func Options[I, O any](path string, fn func(context.Context, I) (O, error)) *Route[I, O] {
	return newRoute("OPTIONS", path, fn)
}
func newRoute[I, O any](method, path string, fn func(context.Context, I) (O, error)) *Route[I, O] {
	effect := "write"
	if method == "GET" || method == "HEAD" || method == "OPTIONS" {
		effect = "read"
	}
	return &Route[I, O]{method: method, path: path, handler: fn, status: 200, effect: effect, source: callerSource(3)}
}

// Const is a typed handler for a constant response, e.g. Get("/", Const("Hello")).
// Supply immutable values; mutable application state belongs in a normal handler.
func Const[O any](value O) func(context.Context, Empty) (O, error) {
	return func(context.Context, Empty) (O, error) { return value, nil }
}

// Name supplies a stable operation name for docs and generated clients.
func (r *Route[I, O]) Name(name string) *Route[I, O] { c := *r; c.name = name; return &c }

// Summary describes the route in generated documentation.
func (r *Route[I, O]) Summary(summary string) *Route[I, O] { c := *r; c.summary = summary; return &c }

// Permission requires a resource-aware authorization decision on every invocation.
func (r *Route[I, O]) Permission(permission string) *Route[I, O] {
	c := *r
	c.permission = permission
	c.permissionSet = true
	return &c
}

// Status sets the normal success status. Use 204 for an empty response.
func (r *Route[I, O]) Status(status int) *Route[I, O] {
	c := *r
	c.status = status
	c.statuses = nil
	return &c
}

// Statuses declares allowed statuses for Response output, or the first and
// second branch statuses for Either output. It replaces a prior Status setting.
func (r *Route[I, O]) Statuses(statuses ...int) *Route[I, O] {
	c := *r
	c.statuses = append([]int(nil), statuses...)
	if len(statuses) == 0 {
		c.configError = errors.New("trest: Statuses must not be empty")
	} else {
		c.status = statuses[0]
	}
	return &c
}

// Timeout sets a positive cooperative handler deadline.
func (r *Route[I, O]) Timeout(timeout time.Duration) *Route[I, O] {
	c := *r
	c.timeout = timeout
	return &c
}

// Validate adds business validation after structural constraints and before authorization.
func (r *Route[I, O]) Validate(fn func(context.Context, I) error) *Route[I, O] {
	c := *r
	c.validate = fn
	return &c
}

// Authorize supplies a typed policy for this route. It requires authentication and
// is also used by the shared operation's direct, job, and tool invocations.
func (r *Route[I, O]) Authorize(fn func(context.Context, Principal, I) error) *Route[I, O] {
	c := *r
	c.authorize = fn
	if fn == nil {
		c.configError = errors.New("trest: nil typed authorizer")
	}
	return &c
}

// MapError translates domain errors to approved public error categories.
func (r *Route[I, O]) MapError(fn func(error) error) *Route[I, O] {
	c := *r
	c.mapError = fn
	return &c
}

// Effect declares read or write semantics for tooling; it does not enforce purity.
func (r *Route[I, O]) Effect(effect string) *Route[I, O] { c := *r; c.effect = effect; return &c }

// Produces documents a native Reply's media type. Ordinary typed values derive
// their schema automatically. A native response remains explicitly untyped to clients.
func (r *Route[I, O]) Produces(contentType string) *Route[I, O] {
	c := *r
	c.contentType = contentType
	return &c
}

var routeName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
var pathParameter = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)(?:\.\.\.)?\}`)

func prepareRoute(b *builder, s scope, method, path, name, summary, permission string, status int) (RouteInfo, error) {
	full, err := joinRoute(s.prefix, path)
	if err != nil {
		return RouteInfo{}, err
	}
	if full == "" {
		full = "/"
	}
	if name == "" {
		hash := sha256.Sum256([]byte(method + " " + full))
		name = strings.ToLower(method) + "_" + hex.EncodeToString(hash[:6])
	}
	if !routeName.MatchString(name) {
		return RouteInfo{}, fmt.Errorf("trest: route name %q must be an identifier", name)
	}
	if b.names[name] {
		return RouteInfo{}, fmt.Errorf("trest: duplicate route name %q", name)
	}
	if status < 200 || status > 399 {
		return RouteInfo{}, fmt.Errorf("trest: unsupported success status %d", status)
	}
	if summary == "" {
		summary = method + " " + full
	}
	return RouteInfo{Name: name, Method: method, Path: full, Summary: summary, Permission: permission, Authenticated: permission != "" || len(s.guards) > 0, Status: status}, nil
}

func (r *Route[I, O]) mount(b *builder, s scope) (err error) {
	defer func() {
		if err != nil {
			err = &ConstructionError{Method: r.method, Path: s.prefix + r.path, Name: r.name, Source: r.source, Cause: err}
		}
	}()
	if r.configError != nil {
		return r.configError
	}
	if r.permissionSet && strings.TrimSpace(r.permission) == "" {
		return errors.New("trest: permission must not be empty")
	}
	info, err := prepareRoute(b, s, r.method, r.path, r.name, r.summary, r.permission, r.status)
	if err != nil {
		return err
	}
	if r.handler == nil {
		return fmt.Errorf("trest: %s has nil handler", info.Name)
	}
	if r.timeout < 0 {
		return fmt.Errorf("trest: %s has negative timeout", info.Name)
	}
	outputType := reflect.TypeFor[O]()
	if outputType.Kind() == reflect.Pointer && isResponseWrapper(outputType) {
		return errors.New("trest: return response and event wrappers as values, not pointers")
	}
	plan, err := webinput.Compile(reflect.TypeFor[I](), r.method, info.Path)
	if err != nil {
		return fmt.Errorf("trest: %s input: %w", info.Name, err)
	}
	output, err := schema.Generate(reflect.TypeFor[O]())
	if err != nil {
		return fmt.Errorf("trest: %s output: %w", info.Name, err)
	}
	isReply := reflect.TypeFor[O]() == reflect.TypeFor[Reply]()
	typed, typedValidators, err := typedResponses(reflect.TypeFor[O](), r.statuses, r.status)
	if err != nil {
		return err
	}
	isTyped := len(typed) > 0
	events, isEvents := reflect.Zero(reflect.TypeFor[O]()).Interface().(typedEventStream)
	info.NativeOnly = plan.NativeOnly || isReply || isTyped || isEvents
	info.Responses = typed
	info.Source = r.source
	info.BodySchema = plan.BodySchema
	info.BodyContentType = plan.BodyContentType
	info.BodyRequired = plan.BodyRequired
	for _, p := range plan.Parameters {
		info.Parameters = append(info.Parameters, Parameter{Name: p.Name, In: p.In, Required: p.Required, Schema: p.Schema})
	}
	info.OutputSchema = output
	info.ContentType = "application/json"
	if reflect.TypeFor[O]().Kind() == reflect.String && !schema.HasCustomCodec(reflect.TypeFor[O]()) {
		info.ContentType = "text/plain"
	}
	if isReply {
		info.OutputSchema = nil
		info.ContentType = "application/octet-stream"
	}
	if r.contentType != "" {
		if !isReply {
			return fmt.Errorf("trest: Produces applies only to native Reply routes")
		}
		info.ContentType = r.contentType
	}
	if r.status == 204 || r.status == 304 {
		info.OutputSchema = nil
		info.ContentType = ""
	}
	if isTyped {
		info.OutputSchema = typed[0].Schema
		info.ContentType = typed[0].ContentType
	}
	if isEvents {
		if r.status != 200 || len(r.statuses) > 0 || r.contentType != "" {
			return errors.New("trest: typed event streams use status 200 and text/event-stream")
		}
		shape, err := schema.Generate(events.eventType())
		if err != nil {
			return err
		}
		validator, err := schema.Compile(shape)
		if err != nil {
			return err
		}
		info.Stream = &StreamInfo{Format: "sse", Schema: shape}
		info.streamValidator = validator
		info.OutputSchema = nil
		info.ContentType = "text/event-stream"
	}
	if r.authorize != nil {
		info.Authenticated = true
	}
	access := operation.Public()
	if info.Authenticated {
		permission := r.permission
		if permission == "" {
			permission = "authenticated"
		}
		access = operation.Permission(permission)
	}
	if r.permission != "" && r.authorize == nil && s.authorizer == nil {
		return fmt.Errorf("trest: %s permission requires an authorizer", info.Name)
	}
	hash := sha256.Sum256([]byte(info.Name))
	info.OperationID = "web." + strings.ToLower(info.Name) + "." + hex.EncodeToString(hash[:4])
	def := operation.Definition[I, O]{ID: info.OperationID, Summary: info.Summary, Access: access, SideEffect: r.effect, Timeout: r.timeout, Validate: r.validate, MapError: r.mapError, JSONDisabled: info.NativeOnly, Source: r.source}
	if len(r.resolvers) > 0 {
		def.Resolve = func(ctx context.Context, in I) (context.Context, error) {
			for _, resolve := range r.resolvers {
				var err error
				ctx, err = resolve(ctx, in)
				if err != nil {
					if r.mapError != nil {
						if mapped := r.mapError(err); mapped != nil {
							err = errors.Join(mapped, err)
						}
					}
					return nil, err
				}
			}
			return ctx, nil
		}
	}
	if info.Authenticated {
		def.Authorize = func(ctx context.Context, p Principal, in I) error {
			for _, guard := range s.guards {
				if err := guard(ctx, p); err != nil {
					return err
				}
			}
			if r.authorize != nil {
				return r.authorize(ctx, p, in)
			}
			if r.permission != "" {
				return s.authorizer.Authorize(ctx, p, r.permission, in)
			}
			return nil
		}
	}
	if isTyped {
		def.ValidateOutput = func(_ context.Context, out O) error {
			reply, ok := any(out).(typedResponse)
			if !ok {
				return errors.New("trest: invalid typed response")
			}
			index, body, _, err := selectResponse(reply, typed)
			if err != nil {
				return err
			}
			if typed[index].ContentType == "" {
				return nil
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				return err
			}
			issues, err := typedValidators[index].Validate(encoded)
			if err != nil {
				return err
			}
			if len(issues) > 0 {
				return errors.New("output violates response schema")
			}
			return nil
		}
	}

	invoke := r.handler
	if len(r.after) > 0 {
		invoke = func(ctx context.Context, in I) (out O, err error) {
			out, err = r.handler(ctx, in)
			if err != nil {
				return out, err
			}
			for _, after := range r.after {
				if err = ctx.Err(); err != nil {
					return out, err
				}
				out, err = after(ctx, in, out)
				if err != nil {
					return out, err
				}
			}
			return out, nil
		}
	}
	handle, err := operation.Register(b.app.registry, def, invoke)
	if err != nil {
		return err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		value, cleanup, err := plan.Decode(w, req, s.maxBody)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			status := 0
			var bindingErr *webinput.Error
			if errors.As(err, &bindingErr) {
				status = bindingErr.Status
			}
			writeError(w, req, s, err, status)
			return
		}
		in, ok := value.Interface().(I)
		if !ok {
			writeError(w, req, s, errors.New("input type mismatch"), 0)
			return
		}
		out, err := handle.Invoke(req.Context(), in)
		if err != nil {
			writeError(w, req, s, err, 0)
			return
		}
		if err = writeOutput(w, req, info, out, s.maxOutput); err != nil {
			if state, _ := req.Context().Value(requestStateKey{}).(*requestState); state != nil {
				state.err = err
			}
			if state, _ := req.Context().Value(requestStateKey{}).(*requestState); state != nil && state.writer.status == 0 {
				writeError(w, req, s, err, 0)
			} else {
				panic(http.ErrAbortHandler)
			}
		}
	})
	if err = b.add(r.method, info.Path, wrapRoute(s, info, handler)); err != nil {
		return err
	}
	b.names[info.Name] = true
	b.app.routes = append(b.app.routes, info)
	b.recordPreflight(info, s)
	return nil
}

// RawRoute integrates an ordinary http.Handler. It retains scoped authentication,
// authorization, middleware, error recovery, and tracing, without claiming a typed
// response contract or exposing the handler to JSON tools/jobs.
type RawRoute struct {
	method, path, name, summary, permission string
	handler                                 http.Handler
	hidden                                  bool
	permissionSet                           bool
	protocol                                string
	source                                  operation.SourceLocation
	messages                                *WebSocketInfo
	configError                             error
}

// Raw mounts a native HTTP handler. It is also the extension point for WebSockets.
func Raw(method, path string, h http.Handler) *RawRoute {
	return &RawRoute{method: strings.ToUpper(method), path: path, handler: h, source: callerSource(2)}
}

// Name gives a raw route a stable documentation/client name.
func (r *RawRoute) Name(name string) *RawRoute { c := *r; c.name = name; return &c }

// Summary describes a raw route.
func (r *RawRoute) Summary(summary string) *RawRoute { c := *r; c.summary = summary; return &c }

// Permission requires a policy decision before entering the native handler.
// The authorizer receives *http.Request as its resource input for raw routes.
func (r *RawRoute) Permission(permission string) *RawRoute {
	c := *r
	c.permission = permission
	c.permissionSet = true
	return &c
}

// WebSocket marks a raw GET handler as a WebSocket upgrade. Its contract is
// explicitly native, rather than claiming an ordinary JSON HTTP response.
func (r *RawRoute) WebSocket() *RawRoute { c := *r; c.protocol = "websocket"; return &c }
func (r *RawRoute) mount(b *builder, s scope) (err error) {
	defer func() {
		if err != nil {
			err = &ConstructionError{Method: r.method, Path: s.prefix + r.path, Name: r.name, Source: r.source, Cause: err}
		}
	}()
	if r.configError != nil {
		return r.configError
	}
	if r.permissionSet && strings.TrimSpace(r.permission) == "" {
		return errors.New("trest: permission must not be empty")
	}
	if r.protocol == "websocket" && r.method != "GET" {
		return errors.New("trest: WebSocket upgrades require GET")
	}
	switch r.method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		return fmt.Errorf("trest: invalid raw method %q", r.method)
	}
	info, err := prepareRoute(b, s, r.method, r.path, r.name, r.summary, r.permission, 200)
	if err != nil {
		return err
	}
	if r.handler == nil {
		return errors.New("trest: nil raw handler")
	}
	if r.permission != "" && s.authorizer == nil {
		return errors.New("trest: raw permission requires authorizer")
	}
	info.NativeOnly = true
	info.Source = r.source
	info.ContentType = "application/octet-stream"
	info.Protocol = r.protocol
	info.WebSocket = r.messages
	if r.protocol == "websocket" {
		info.Status = 101
		info.ContentType = ""
	}
	for _, match := range pathParameter.FindAllStringSubmatch(info.Path, -1) {
		info.Parameters = append(info.Parameters, Parameter{Name: match[1], In: "path", Required: true, Schema: json.RawMessage(`{"type":"string"}`)})
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if info.Authenticated {
			p, ok := Identity(req.Context())
			if !ok {
				writeError(w, req, s, operation.NewError(operation.Unauthenticated, "", nil), 0)
				return
			}
			for _, guard := range s.guards {
				if err := guard(req.Context(), p); err != nil {
					writeError(w, req, s, err, 0)
					return
				}
			}
			if r.permission != "" {
				if err := s.authorizer.Authorize(req.Context(), p, r.permission, req); err != nil {
					writeError(w, req, s, err, 0)
					return
				}
			}
		}
		r.handler.ServeHTTP(w, req)
	})
	if err = b.add(r.method, info.Path, wrapRoute(s, info, h)); err != nil {
		return err
	}
	b.names[info.Name] = true
	if !r.hidden {
		b.app.routes = append(b.app.routes, info)
	}
	b.recordPreflight(info, s)
	return nil
}

func callerSource(skip int) operation.SourceLocation {
	if pc, file, line, ok := runtime.Caller(skip); ok {
		source := operation.SourceLocation{File: filepath.Base(file), Line: line}
		if fn := runtime.FuncForPC(pc); fn != nil {
			source.Function = fn.Name()
		}
		return source
	}
	return operation.SourceLocation{}
}

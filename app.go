// Package trest builds typed web applications from routes, groups, and plugins.
// Construction validates and freezes the application without starting a server.
package trest

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mujhtech/trest/operation"
)

// Empty is the input for handlers that do not need request data.
type Empty = struct{}

// Principal is an identity established by application authentication.
type Principal = operation.Principal

// Component is a route, group, plugin, or application configuration. Components
// are compiled by New; application-defined plugins compose existing components.
type Component interface{ mount(*builder, scope) error }

// Middleware is ordinary net/http middleware, scoped to its containing group.
type Middleware func(http.Handler) http.Handler

type scope struct {
	prefix             string
	identity           *byte
	depth              int
	middleware         []Middleware
	authenticate       func(*http.Request) (Principal, error)
	authorizer         operation.Authorizer
	guards             []func(context.Context, Principal) error
	errorMap           func(context.Context, error) error
	after              []func(Event)
	maxBody, maxOutput int64
}

type builder struct {
	app             *App
	mux             *http.ServeMux
	names           map[string]bool
	plugins         map[string]pluginRecord
	active          map[*Router]bool
	preflights      map[string]map[string]scope
	registryOptions []operation.Option
}

// App is an immutable application and an http.Handler. It may be mounted on an
// existing server or served with Listen. Lifecycle hooks run only with Run/Listen.
type App struct {
	handler        http.Handler
	registry       *operation.Registry
	routes         []RouteInfo
	title, version string
	lifecycle      []lifecycleHook
	serveMu        sync.Mutex
	isRunning      bool
}

// New compiles components and reports construction errors together. Configuration
// components apply to every route in their containing group, independent of order.
// A child group inherits a snapshot; its configuration never changes siblings.
func New(components ...Component) (*App, error) {
	a := &App{registry: operation.NewRegistry(nil), title: "Trest API", version: "0.1.0"}
	b := &builder{app: a, mux: http.NewServeMux(), names: map[string]bool{}, plugins: map[string]pluginRecord{}, active: map[*Router]bool{}, preflights: map[string]map[string]scope{}}
	err := b.mountAll(components, scope{maxBody: 1 << 20, maxOutput: 1 << 20})
	if err != nil {
		return nil, err
	}
	if err = b.installOptions(); err != nil {
		return nil, err
	}
	if err = a.registry.Freeze(); err != nil {
		return nil, err
	}
	sort.Slice(a.routes, func(i, j int) bool {
		if a.routes[i].Path == a.routes[j].Path {
			return a.routes[i].Method < a.routes[j].Method
		}
		return a.routes[i].Path < a.routes[j].Path
	})
	a.handler = b.mux
	return a, nil
}

func (b *builder) mountAll(items []Component, s scope) error {
	s.identity = new(byte)
	s.middleware = append([]Middleware(nil), s.middleware...)
	s.guards = append([]func(context.Context, Principal) error(nil), s.guards...)
	s.after = append([]func(Event){}, s.after...)
	var failures []error
	for _, item := range items {
		if isNilComponent(item) {
			failures = append(failures, errors.New("trest: nil component"))
			continue
		}
		if c, ok := item.(*configuration); ok {
			if err := c.apply(b, &s); err != nil {
				failures = append(failures, &ConstructionError{Component: "configuration", Path: s.prefix, Source: c.source, Cause: err})
			}
		}
	}
	for _, item := range items {
		if isNilComponent(item) {
			continue
		}
		if _, ok := item.(*configuration); ok {
			continue
		}
		if err := item.mount(b, s); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func isNilComponent(c Component) bool {
	return c == nil || (reflect.ValueOf(c).Kind() == reflect.Pointer && reflect.ValueOf(c).IsNil())
}

// Router composes routes under a prefix. Treat components as immutable after
// construction; configuration methods return independent copies.
type Router struct {
	prefix, name string
	children     []Component
	requires     []string
	source       operation.SourceLocation
}

type pluginRecord struct {
	router *Router
	scope  *byte
}

// Group composes routes and settings under prefix, using Go's /{id} path syntax.
func Group(prefix string, items ...Component) *Router {
	return &Router{source: callerSource(2), prefix: prefix, children: append([]Component(nil), items...)}
}

// Plugin names a reusable group. Reusing the same instance at the same app scope
// is deduplicated; different definitions with the same name are rejected.
func Plugin(name string, items ...Component) *Router {
	return &Router{source: callerSource(2), name: name, children: append([]Component(nil), items...)}
}

// Requires declares plugin names that must already be mounted in this app.
func (r *Router) Requires(names ...string) *Router {
	c := *r
	c.requires = append(append([]string(nil), r.requires...), names...)
	return &c
}

// Use returns a group with additional components, without modifying the source.
func (r *Router) Use(items ...Component) *Router {
	c := *r
	c.children = append(append([]Component(nil), r.children...), items...)
	return &c
}

func (r *Router) mount(b *builder, s scope) (err error) {
	defer func() {
		if err != nil {
			label := "group"
			if r.name != "" {
				label = "plugin " + r.name
			}
			err = &ConstructionError{Component: label, Path: s.prefix + r.prefix, Source: r.source, Cause: err}
		}
	}()
	if b.active[r] {
		return errors.New("trest: cyclic group/plugin dependency")
	}
	if r.name != "" {
		key := s.prefix + "\x00" + r.name
		if previous, ok := b.plugins[key]; ok {
			if previous.router == r && previous.scope == s.identity {
				return nil
			}
			return fmt.Errorf("trest: conflicting plugin %q", r.name)
		}
		for _, name := range r.requires {
			if _, ok := b.plugins[s.prefix+"\x00"+name]; !ok {
				return fmt.Errorf("trest: plugin %q requires %q first in the same scope", r.name, name)
			}
		}
		b.plugins[key] = pluginRecord{router: r, scope: s.identity}
	}
	prefix, err := joinRoute(s.prefix, r.prefix)
	if err != nil {
		return err
	}
	s.prefix = prefix
	s.depth++
	b.active[r] = true
	defer delete(b.active, r)
	return b.mountAll(r.children, s)
}

type configuration struct {
	apply  func(*builder, *scope) error
	source operation.SourceLocation
}

func (c *configuration) mount(*builder, scope) error { return nil }

// Use installs middleware on routes in the containing group. The first middleware
// is outermost. Keep authentication policy in Authenticate/Authorization/Guard.
func Use(m ...Middleware) Component {
	return &configuration{source: callerSource(2), apply: func(_ *builder, s *scope) error {
		for _, fn := range m {
			if fn == nil {
				return errors.New("trest: nil middleware")
			}
		}
		s.middleware = append(s.middleware, m...)
		return nil
	}}
}

// Authenticate verifies credentials. An empty principal and nil error means
// anonymous access; protected routes reject it before their handler runs.
func Authenticate(fn func(*http.Request) (Principal, error)) Component {
	return &configuration{source: callerSource(2), apply: func(_ *builder, s *scope) error {
		if fn == nil {
			return errors.New("trest: nil authenticator")
		}
		s.authenticate = fn
		return nil
	}}
}

// Authorization supplies the resource-aware permission policy in this scope.
func Authorization(a operation.Authorizer) Component {
	return &configuration{source: callerSource(2), apply: func(_ *builder, s *scope) error {
		if a == nil || ((reflect.ValueOf(a).Kind() == reflect.Pointer || reflect.ValueOf(a).Kind() == reflect.Func) && reflect.ValueOf(a).IsNil()) {
			return errors.New("trest: nil authorizer")
		}
		s.authorizer = a
		return nil
	}}
}

// Guard adds an authenticated policy to every route in a group. It cannot be
// disabled by a child route and also runs through direct JSON/job/tool invocation.
func Guard(fn func(context.Context, Principal) error) Component {
	return &configuration{source: callerSource(2), apply: func(_ *builder, s *scope) error {
		if fn == nil {
			return errors.New("trest: nil guard")
		}
		s.guards = append(s.guards, fn)
		return nil
	}}
}

// RequireAuth requires a verified nonempty principal for every route in scope.
func RequireAuth() Component { return Guard(func(context.Context, Principal) error { return nil }) }

// Limits sets positive request and encoded response byte limits in this scope.
// Streaming responses instead apply backpressure and per-write deadlines.
func Limits(bodyBytes, outputBytes int64) Component {
	return &configuration{source: callerSource(2), apply: func(_ *builder, s *scope) error {
		if bodyBytes <= 0 || outputBytes < 256 {
			return errors.New("trest: body limit must be positive and output limit at least 256")
		}
		s.maxBody = bodyBytes
		s.maxOutput = outputBytes
		return nil
	}}
}

// Info names the generated contract. Put it at app scope.
func Info(title, version string) Component {
	return &configuration{source: callerSource(2), apply: func(b *builder, s *scope) error {
		if s.depth != 0 {
			return errors.New("trest: Info belongs at app scope")
		}
		if strings.TrimSpace(title) == "" || strings.TrimSpace(version) == "" {
			return errors.New("trest: title and version are required")
		}
		b.app.title = title
		b.app.version = version
		return nil
	}}
}

// Observe connects metadata-only operation tracing to an explicit observer.
// It applies at app scope and includes direct JSON, job, and tool invocations.
// AfterResponse additionally reports HTTP decoding/delivery outcomes.
func Observe(observer operation.Observer) Component {
	return &configuration{source: callerSource(2), apply: func(b *builder, s *scope) error {
		if s.depth != 0 {
			return errors.New("trest: Observe belongs at app scope")
		}
		b.registryOptions = append(b.registryOptions, operation.WithObserver(observer))
		b.app.registry = operation.NewRegistry(nil, b.registryOptions...)
		return nil
	}}
}

// Concurrency sets a positive per-app operation admission limit. Excess calls
// are rejected instead of accumulating an unbounded queue.
func Concurrency(max int) Component {
	return &configuration{source: callerSource(2), apply: func(b *builder, s *scope) error {
		if s.depth != 0 || max <= 0 {
			return errors.New("trest: positive Concurrency belongs at app scope")
		}
		b.registryOptions = append(b.registryOptions, operation.WithMaxConcurrent(max))
		b.app.registry = operation.NewRegistry(nil, b.registryOptions...)
		return nil
	}}
}

// OnError maps request errors into approved public errors. Unclassified errors
// remain redacted. It cannot change a response after headers have been sent.
func OnError(fn func(context.Context, error) error) Component {
	return &configuration{source: callerSource(2), apply: func(_ *builder, s *scope) error {
		if fn == nil {
			return errors.New("trest: nil error hook")
		}
		s.errorMap = fn
		return nil
	}}
}

// Event contains request outcome metadata, never request/response bodies.
type Event struct {
	Method, Path, Operation, RequestID string
	Status                             int
	Duration                           time.Duration
	Code                               operation.Code
}

// AfterResponse observes completed requests in this scope. Hooks must be fast,
// concurrency-safe, and must not retain request data. Observer panics are isolated.
func AfterResponse(fn func(Event)) Component {
	return &configuration{source: callerSource(2), apply: func(_ *builder, s *scope) error {
		if fn == nil {
			return errors.New("trest: nil after-response hook")
		}
		s.after = append(s.after, fn)
		return nil
	}}
}

// OnStart runs when Run/Listen starts, before accepting requests.
func OnStart(fn func(context.Context) error) Component {
	return &configuration{source: callerSource(2), apply: func(b *builder, _ *scope) error {
		if fn == nil {
			return errors.New("trest: nil start hook")
		}
		b.app.lifecycle = append(b.app.lifecycle, lifecycleHook{start: fn})
		return nil
	}}
}

// OnStop runs in reverse registration order after request cancellation starts.
func OnStop(fn func(context.Context) error) Component {
	return &configuration{source: callerSource(2), apply: func(b *builder, _ *scope) error {
		if fn == nil {
			return errors.New("trest: nil stop hook")
		}
		b.app.lifecycle = append(b.app.lifecycle, lifecycleHook{stop: fn})
		return nil
	}}
}

type lifecycleHook struct{ start, stop func(context.Context) error }

// Lifecycle pairs resource acquisition with cleanup. If startup fails, only
// successfully started components are stopped; a failed component cleans up any
// partial acquisition itself. This prevents rollback from stopping another owner.
func Lifecycle(start, stop func(context.Context) error) Component {
	return &configuration{source: callerSource(2), apply: func(b *builder, _ *scope) error {
		if start == nil || stop == nil {
			return errors.New("trest: lifecycle requires start and stop")
		}
		b.app.lifecycle = append(b.app.lifecycle, lifecycleHook{start: start, stop: stop})
		return nil
	}}
}

func callLifecycle(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("trest: lifecycle hook panicked")
		}
	}()
	return fn(ctx)
}

// Registry returns the frozen shared operation model. Native HTTP-only routes
// reject JSON invocation; JSON routes preserve the same authorization for tools/jobs.
func (a *App) Registry() *operation.Registry { return a.registry }

// ServeHTTP serves the immutable app; it starts no lifecycle workers.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// Listen opens a TCP listener and runs the app until ctx is canceled.
func (a *App) Listen(ctx context.Context, address string) error {
	if ctx == nil {
		return errors.New("trest: nil serve context")
	}
	l, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	return a.Run(ctx, l)
}

// Run owns listener and serves with the default HTTP timeouts. Use RunServer
// when the application needs its own TLS, connection, or timeout configuration.
func (a *App) Run(ctx context.Context, listener net.Listener) error {
	return a.RunServer(ctx, &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}, listener)
}

func joinRoute(prefix, path string) (string, error) {
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		return "", fmt.Errorf("trest: invalid prefix %q", prefix)
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("trest: route must start with /: %q", path)
	}
	full := strings.TrimSuffix(prefix, "/") + path
	if full == "" {
		return "", nil
	}
	if strings.ContainsAny(full, "?# \t\r\n") || strings.Contains(full, "//") {
		return "", fmt.Errorf("trest: invalid route %q", full)
	}
	for _, p := range strings.Split(full, "/") {
		if p == "." || p == ".." {
			return "", fmt.Errorf("trest: invalid route %q", full)
		}
	}
	return full, nil
}

func (b *builder) add(method, path string, h http.Handler) (err error) {
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("trest: invalid or conflicting route %s %s", method, path)
		}
	}()
	pattern := method + " " + path
	if strings.HasSuffix(path, "/") {
		pattern += "{$}"
	}
	b.mux.Handle(pattern, h)
	return nil
}

func (b *builder) recordPreflight(info RouteInfo, s scope) {
	if b.preflights[info.Path] == nil {
		b.preflights[info.Path] = map[string]scope{}
	}
	b.preflights[info.Path][info.Method] = s
}
func (b *builder) installOptions() error {
	for path, methods := range b.preflights {
		if _, ok := methods["OPTIONS"]; ok {
			continue
		}
		var allowed []string
		for method := range methods {
			allowed = append(allowed, method)
		}
		sort.Strings(allowed)
		handlers := map[string]http.Handler{}
		for method, s := range methods {
			local := s
			local.authenticate = nil
			info := RouteInfo{Name: "preflight", Path: path, Method: "OPTIONS"}
			handlers[method] = wrapRoute(local, info, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Allow", strings.Join(append(allowed, "OPTIONS"), ", "))
				w.WriteHeader(http.StatusNoContent)
			}))
		}
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method := r.Header.Get("Access-Control-Request-Method")
			if method != "" {
				if handler, ok := handlers[method]; ok {
					handler.ServeHTTP(w, r)
					return
				}
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Allow", strings.Join(append(allowed, "OPTIONS"), ", "))
			w.WriteHeader(204)
		})
		if err := b.add("OPTIONS", path, h); err != nil {
			return err
		}
	}
	return nil
}

type requestState struct {
	context.Context
	requestID string
	err       error
	writer    responseWriter
}
type requestStateKey struct{}

func (s *requestState) Value(key any) any {
	if _, ok := key.(requestStateKey); ok {
		return s
	}
	return s.Context.Value(key)
}

func wrapRoute(s scope, info RouteInfo, h http.Handler) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authenticate != nil {
			p, err := s.authenticate(r)
			if err != nil {
				writeError(w, r, s, err, 0)
				return
			}
			r = r.WithContext(operation.WithPrincipal(r.Context(), p))
		}
		h.ServeHTTP(w, r)
	})
	var wrapped http.Handler = inner
	for i := len(s.middleware) - 1; i >= 0; i-- {
		wrapped = s.middleware[i](wrapped)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Request duration is observable only when a completion hook is present.
		var started time.Time
		if len(s.after) > 0 {
			started = time.Now()
		}
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		id := hex.EncodeToString(bytes[:])
		state := &requestState{Context: operation.WithCorrelationID(r.Context(), id), requestID: id, writer: responseWriter{ResponseWriter: w}}
		r = r.WithContext(state)
		w.Header().Set("X-Request-ID", id)
		recorded := &state.writer
		defer func() {
			abort := false
			if recovered := recover(); recovered != nil {
				if state.err == nil {
					state.err = operation.NewError(operation.Internal, "", errors.New("application panic"))
				}
				if recorded.status == 0 {
					writeError(recorded, r, s, state.err, 0)
				} else {
					abort = true
				}
			}
			status := recorded.status
			if status == 0 {
				status = 200
			}
			if len(s.after) > 0 {
				event := Event{Method: r.Method, Path: info.Path, Operation: info.Name, RequestID: id, Status: status, Duration: time.Since(started), Code: operation.CodeOf(state.err)}
				for _, hook := range s.after {
					func() { defer func() { _ = recover() }(); hook(event) }()
				}
			}
			if abort {
				panic(http.ErrAbortHandler)
			}
		}()
		wrapped.ServeHTTP(recorded, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(code int) {
	if code < 100 || code > 999 {
		panic("trest: invalid HTTP status")
	}
	if code < 200 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}

// WriteString preserves a native writer's allocation-free string path while
// recording the same implicit status as Write.
func (w *responseWriter) WriteString(s string) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return io.WriteString(w.ResponseWriter, s)
}
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) Flush() {
	_ = w.FlushError()
}

func (w *responseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, r, e := http.NewResponseController(w.ResponseWriter).Hijack()
	if e == nil {
		w.status = 101
	}
	return c, r, e
}

func writeError(w http.ResponseWriter, r *http.Request, s scope, err error, status int) {
	state, _ := r.Context().Value(requestStateKey{}).(*requestState)
	if s.errorMap != nil {
		if mapped := mapRequestError(s.errorMap, r.Context(), err); mapped != nil {
			err = mapped
		}
	}
	if state != nil {
		state.err = err
	}
	public := operation.PublicError(err)
	if public == nil {
		public = operation.PublicError(errors.New("unknown error"))
	}
	if status == 0 {
		status = statusFor(public.Code)
	}
	id := ""
	if state != nil {
		id = state.requestID
	}
	body := struct {
		Code      operation.Code         `json:"code"`
		Message   string                 `json:"message"`
		RequestID string                 `json:"request_id"`
		Fields    []operation.FieldError `json:"fields,omitempty"`
	}{public.Code, public.Message, id, public.Fields}
	data, marshalErr := json.Marshal(body)
	if marshalErr != nil || int64(len(data)) > s.maxOutput {
		body.Message = "request failed"
		body.Fields = nil
		data, _ = json.Marshal(body)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != "HEAD" {
		_, _ = w.Write(data)
	}
}

func mapRequestError(fn func(context.Context, error) error, ctx context.Context, err error) (mapped error) {
	defer func() {
		if recover() != nil {
			mapped = operation.NewError(operation.Internal, "", errors.New("error hook panicked"))
		}
	}()
	return fn(ctx, err)
}

func statusFor(code operation.Code) int {
	switch code {
	case operation.InvalidInput:
		return 400
	case operation.Unauthenticated:
		return 401
	case operation.Forbidden:
		return 403
	case operation.NotFound:
		return 404
	case operation.Conflict:
		return 409
	case operation.RateLimited:
		return 429
	case operation.Unavailable:
		return 503
	case operation.DeadlineExceeded:
		return 504
	case operation.Canceled:
		return 408
	default:
		return 500
	}
}

// Identity retrieves the principal verified by the application authenticator.
func Identity(ctx context.Context) (Principal, bool) { return operation.PrincipalFromContext(ctx) }

// Key is an opaque, typed key for request-scoped values. Create it once and pass
// it explicitly to the middleware and handlers that share that request value.
type Key[T any] struct{ identity *byte }

// NewKey creates a unique typed request value key.
func NewKey[T any]() Key[T] { return Key[T]{new(byte)} }

// WithValue returns a context with a typed request-local value.
func WithValue[T any](ctx context.Context, key Key[T], value T) context.Context {
	if key.identity == nil {
		panic("trest: use NewKey")
	}
	return context.WithValue(ctx, key, value)
}

// Value reads a typed request-local value.
func Value[T any](ctx context.Context, key Key[T]) (T, bool) {
	v, ok := ctx.Value(key).(T)
	return v, ok
}

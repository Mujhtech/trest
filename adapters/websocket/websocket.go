// Package trestws integrates the coder/websocket SDK with Trest's scoped routing.
// Authentication and permission checks run before the WebSocket handshake.
package trestws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/websocket"
	"trest.local/trest"
)

// MessageType identifies a text or binary WebSocket message.
type MessageType = sdk.MessageType

const (
	Text   = sdk.MessageText
	Binary = sdk.MessageBinary
)

// Options bounds each connection and makes browser origin and route access
// explicit. Public and Permission are mutually exclusive; one is required.
type Options struct {
	Name               string
	Permission         string
	Public             bool
	Origins            []string
	AllowMissingOrigin bool
	MaxMessageBytes    int64
	MaxConnections     int
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	SessionTimeout     time.Duration
	// OnError receives private connection errors; the application owns redaction.
	// It runs on the handler goroutine and must return promptly.
	OnError func(error)
}

// Handler owns one connection until it returns. It should keep reading to process
// close/ping frames and honor ctx. Return cancels the connection and releases its
// slot; there is no hidden replay, message queue, or durable delivery guarantee.
type Handler func(context.Context, *Conn) error

// New creates a route and lifecycle hooks. It starts no listener or goroutine.
// Defaults: 64 KiB messages, 128 connections, 30s read, 10s write, 5m session.
func New(path string, options Options, handler Handler) (trest.Component, error) {
	endpoint, err := newEndpoint(options, handler)
	if err != nil {
		return nil, err
	}
	route := endpoint.route(path)
	return trest.Group("", trest.Lifecycle(endpoint.start, endpoint.stop), route), nil
}

func (e *endpoint) route(path string) *trest.RawRoute {
	route := trest.Raw(http.MethodGet, path, http.HandlerFunc(e.serve)).WebSocket().Summary("WebSocket connection")
	options := e.options
	if options.Name != "" {
		route = route.Name(options.Name)
	}
	if options.Permission != "" {
		route = route.Permission(options.Permission)
	}
	return route
}

type active struct{ cancel context.CancelFunc }
type endpoint struct {
	mu       sync.Mutex
	options  Options
	handler  Handler
	origins  map[string]bool
	sessions map[uint64]*active
	next     uint64
	idle     chan struct{}
	stopping bool
	running  bool
	lifetime context.Context
}

func newEndpoint(options Options, handler Handler) (*endpoint, error) {
	if handler == nil {
		return nil, errors.New("websocket: handler required")
	}
	if (options.Public && options.Permission != "") || (!options.Public && options.Permission == "") {
		return nil, errors.New("websocket: choose a permission or explicit public access")
	}
	if options.MaxMessageBytes == 0 {
		options.MaxMessageBytes = 64 << 10
	}
	if options.MaxConnections == 0 {
		options.MaxConnections = 128
	}
	if options.ReadTimeout == 0 {
		options.ReadTimeout = 30 * time.Second
	}
	if options.WriteTimeout == 0 {
		options.WriteTimeout = 10 * time.Second
	}
	if options.SessionTimeout == 0 {
		options.SessionTimeout = 5 * time.Minute
	}
	if options.MaxMessageBytes < 1 || options.MaxMessageBytes > 64<<20 || options.MaxConnections < 1 || options.MaxConnections > 100000 || options.ReadTimeout < 0 || options.WriteTimeout < 0 || options.SessionTimeout < 0 {
		return nil, errors.New("websocket: invalid limits")
	}
	if len(options.Origins) == 0 && !options.AllowMissingOrigin {
		return nil, errors.New("websocket: configure allowed origins or explicitly allow origin-less clients")
	}
	origins := make(map[string]bool, len(options.Origins))
	for _, origin := range options.Origins {
		normalized, ok := normalizeOrigin(origin)
		if !ok {
			return nil, errors.New("websocket: origins must be exact http or https origins")
		}
		origins[normalized] = true
	}
	idle := make(chan struct{})
	close(idle)
	return &endpoint{options: options, handler: handler, origins: origins, sessions: make(map[uint64]*active), idle: idle}, nil
}
func (e *endpoint) start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("websocket: nil lifecycle context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running || len(e.sessions) != 0 {
		return errors.New("websocket: endpoint already active")
	}
	e.running = true
	e.stopping = false
	e.lifetime = ctx
	return nil
}
func (e *endpoint) stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("websocket: nil stop context")
	}
	e.mu.Lock()
	e.stopping = true
	e.running = false
	sessions := make([]*active, 0, len(e.sessions))
	for _, session := range e.sessions {
		if session != nil {
			sessions = append(sessions, session)
		}
	}
	idle := e.idle
	e.mu.Unlock()
	for _, session := range sessions {
		session.cancel()
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *endpoint) reserve() (uint64, context.Context, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopping || len(e.sessions) >= e.options.MaxConnections {
		return 0, nil, false
	}
	if len(e.sessions) == 0 {
		e.idle = make(chan struct{})
	}
	e.next++
	id := e.next
	e.sessions[id] = nil
	return id, e.lifetime, true
}
func (e *endpoint) release(id uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.sessions, id)
	if len(e.sessions) == 0 {
		close(e.idle)
	}
}
func (e *endpoint) serve(w http.ResponseWriter, r *http.Request) {
	if !e.originAllowed(r) {
		http.Error(w, "websocket origin denied", http.StatusForbidden)
		return
	}
	id, lifetime, ok := e.reserve()
	if !ok {
		http.Error(w, "websocket capacity unavailable", http.StatusServiceUnavailable)
		return
	}
	defer e.release(id)
	// Exact full-origin policy was checked above. Disable the SDK's broader default
	// (which always permits the request host) so only this explicit policy applies.
	socket, err := sdk.Accept(w, r, &sdk.AcceptOptions{InsecureSkipVerify: true, CompressionMode: sdk.CompressionDisabled})
	if err != nil {
		return
	}
	defer func() { _ = socket.CloseNow() }()
	socket.SetReadLimit(e.options.MaxMessageBytes)
	// A hijacked net/http request context can be canceled independently of the
	// socket. Preserve identity values, then bind cancellation to app/session life.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), e.options.SessionTimeout)
	defer cancel()
	stopSocket := context.AfterFunc(ctx, func() { _ = socket.CloseNow() })
	defer stopSocket()
	if lifetime != nil {
		stopLifetime := context.AfterFunc(lifetime, cancel)
		defer stopLifetime()
	}
	e.mu.Lock()
	e.sessions[id] = &active{cancel: cancel}
	stopping := e.stopping
	e.mu.Unlock()
	if stopping {
		cancel()
		return
	}
	conn := &Conn{socket: socket, ctx: ctx, readTimeout: e.options.ReadTimeout, writeTimeout: e.options.WriteTimeout, maxBytes: e.options.MaxMessageBytes}
	err = e.call(ctx, conn)
	if err != nil && e.options.OnError != nil && ctx.Err() == nil && sdk.CloseStatus(err) != sdk.StatusNormalClosure {
		e.report(err)
	}
}
func (e *endpoint) call(ctx context.Context, conn *Conn) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("websocket: handler panicked")
		}
	}()
	return e.handler(ctx, conn)
}
func (e *endpoint) report(err error) { defer func() { _ = recover() }(); e.options.OnError(err) }
func (e *endpoint) originAllowed(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return e.options.AllowMissingOrigin
	}
	if len(values) != 1 {
		return false
	}
	origin, ok := normalizeOrigin(values[0])
	return ok && e.origins[origin]
}
func normalizeOrigin(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") || strings.ContainsAny(origin, "* \t\r\n,") {
		return "", false
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), true
}

// Conn exposes bounded reads and writes while retaining SDK protocol behavior.
// Concurrent writes are supported by the SDK. Use one reader per connection.
type Conn struct {
	socket                    *sdk.Conn
	ctx                       context.Context
	readTimeout, writeTimeout time.Duration
	maxBytes                  int64
}

// Read waits for one message with the configured read deadline and size limit.
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
	if c == nil || c.socket == nil || c.ctx == nil || ctx == nil {
		return 0, nil, errors.New("websocket: connection and context required")
	}
	if err := c.ctx.Err(); err != nil {
		return 0, nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, c.readTimeout)
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	defer cancel()
	return c.socket.Read(bounded)
}

// Write sends a complete message; writes exceeding the configured size limit are
// rejected before delivery. A deadline/cancellation closes the underlying socket.
func (c *Conn) Write(ctx context.Context, kind MessageType, data []byte) error {
	if c == nil || c.socket == nil || c.ctx == nil || ctx == nil {
		return errors.New("websocket: connection and context required")
	}
	if kind != Text && kind != Binary {
		return errors.New("websocket: invalid message type")
	}
	if int64(len(data)) > c.maxBytes {
		return errors.New("websocket: outgoing message exceeds size limit")
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, c.writeTimeout)
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	defer cancel()
	return c.socket.Write(bounded, kind, data)
}

// Close immediately closes the socket and unblocks pending reads/writes. It does
// not wait for a peer close handshake, so shutdown is independent of peer behavior.
func (c *Conn) Close() error {
	if c == nil || c.socket == nil {
		return nil
	}
	return c.socket.CloseNow()
}

// ReadJSON accepts one text message and decodes it into T with unknown-field
// rejection. Application business validation remains the handler's responsibility.
func ReadJSON[T any](ctx context.Context, c *Conn) (T, error) {
	var value T
	kind, data, err := c.Read(ctx)
	if err != nil {
		return value, err
	}
	if kind != Text {
		return value, errors.New("websocket: JSON requires a text message")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&value); err != nil {
		var zero T
		return zero, errors.New("websocket: invalid JSON message")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		var zero T
		return zero, errors.New("websocket: invalid JSON message")
	}
	return value, nil
}

// WriteJSON encodes a typed value and applies the normal output size/deadline limits.
func WriteJSON[T any](ctx context.Context, c *Conn, value T) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.Write(ctx, Text, data)
}

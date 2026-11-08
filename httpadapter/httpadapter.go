// Package httpadapter exposes explicitly bound operations through net/http.
//
// Construct an Adapter from a frozen operation registry, bind routes, then serve
// the supplied mux. Binding is a startup operation. Existing mux handlers remain
// application-owned. This experimental adapter supports literal paths and JSON
// bodies; it does not infer path, query, or header bindings from struct tags.
package httpadapter

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"

	"trest.local/trest/operation"
)

const (
	// DefaultMaxBodyBytes bounds incoming JSON before decoding.
	DefaultMaxBodyBytes int64 = 1 << 20
	// DefaultMaxOutputBytes bounds each serialized success or error response.
	DefaultMaxOutputBytes int64 = 1 << 20
)

// Options configures protocol limits and application-owned authentication.
// Zero limits select the defaults. MaxOutputBytes must be at least 256 so a
// minimal error with a correlation ID always fits. Negative limits are invalid.
// Authenticate must verify credentials before returning a principal. Returning
// an empty principal and nil error represents an anonymous request. When it is
// nil, identity must come from trusted middleware using operation.WithPrincipal.
// Request fields and headers never establish identity automatically.
type Options struct {
	MaxBodyBytes   int64
	MaxOutputBytes int64
	Authenticate   func(*http.Request) (operation.Principal, error)
}

// Route explicitly maps an operation to a literal HTTP path. GET and DELETE
// accept only empty struct inputs and no request body. POST, PUT, and PATCH
// require application/json. SuccessStatus defaults to 200 and supports 200,
// 201, and 202; successful responses always have a JSON representation.
type Route struct {
	Method        string
	Path          string
	SuccessStatus int
}

// Binding describes a successfully installed route for contract generation.
type Binding struct {
	OperationID   string `json:"operation_id"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	SuccessStatus int    `json:"success_status"`
}

// Adapter binds operations to an application-owned ServeMux. A registry is
// immutable before adapter construction. Bindings cannot change after the first
// operation request; complete all bindings before serving any traffic.
type Adapter struct {
	mux      *http.ServeMux
	registry *operation.Registry
	opts     Options

	mu         sync.Mutex
	hasServed  bool
	bindings   []Binding
	operations map[string]bool
}

// New constructs an adapter without starting a listener or goroutine.
func New(mux *http.ServeMux, registry *operation.Registry, opts Options) (*Adapter, error) {
	if mux == nil {
		return nil, errors.New("httpadapter: mux is nil")
	}
	if registry == nil || !registry.IsFrozen() {
		return nil, errors.New("httpadapter: registry must be frozen before constructing the adapter")
	}
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.MaxOutputBytes == 0 {
		opts.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if opts.MaxBodyBytes < 0 {
		return nil, errors.New("httpadapter: max body bytes must be positive")
	}
	if opts.MaxOutputBytes < 256 {
		return nil, errors.New("httpadapter: max output bytes must be at least 256")
	}
	return &Adapter{mux: mux, registry: registry, opts: opts, operations: make(map[string]bool)}, nil
}

// Bind installs a typed operation handler. It returns startup errors for invalid
// routes, foreign handles, duplicate operation exposure, and mux conflicts.
// No handler or authorization callback executes during binding.
func Bind[I, O any](a *Adapter, h *operation.Handle[I, O], route Route) error {
	if a == nil || h == nil {
		return errors.New("httpadapter: adapter and handle must not be nil")
	}
	if h.Registry() != a.registry {
		return errors.New("httpadapter: handle belongs to another registry")
	}
	if err := validateRoute(route); err != nil {
		return err
	}
	if route.SuccessStatus == 0 {
		route.SuccessStatus = http.StatusOK
	}
	hasBody := route.Method == http.MethodPost || route.Method == http.MethodPut || route.Method == http.MethodPatch
	if !hasBody {
		t := reflect.TypeFor[I]()
		if t.Kind() != reflect.Struct || t.NumField() != 0 {
			return fmt.Errorf("httpadapter: %s input must be an empty struct; query and path binding is not supported", route.Method)
		}
	}
	id := h.Metadata().ID
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hasServed {
		return errors.New("httpadapter: bindings are frozen after serving starts")
	}
	if a.operations[id] {
		return fmt.Errorf("httpadapter: operation %q already has an http binding", id)
	}
	pattern := route.Method + " " + route.Path
	if route.Path == "/" {
		pattern += "{$}"
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.hasServed = true
		a.mu.Unlock()
		requestID, err := newRequestID()
		if err != nil {
			a.writeError(w, r, "", operation.NewError(operation.Internal, "", err), 0)
			return
		}
		w.Header().Set("X-Request-ID", requestID)
		r = r.WithContext(operation.WithCorrelationID(r.Context(), requestID))
		// Nothing is written until invocation and serialization finish, so a panic
		// in application code can be converted into one redacted response.
		defer func() {
			if recovered := recover(); recovered != nil {
				a.writeError(w, r, requestID, operation.NewError(operation.Internal, "", errors.New("application panic")), 0)
			}
		}()
		if err := r.Context().Err(); err != nil {
			a.writeError(w, r, requestID, err, 0)
			return
		}
		if a.opts.Authenticate != nil {
			principal, err := a.opts.Authenticate(r)
			if err != nil {
				// Authentication is a protocol boundary: arbitrary authenticator
				// errors must not leak credentials or provider internals.
				a.writeError(w, r, requestID, operation.NewError(operation.Unauthenticated, "authentication required", err), 0)
				return
			}
			r = r.WithContext(operation.WithPrincipal(r.Context(), principal))
		}
		var in I
		if status, err := decodeInput(w, r, h, a.opts.MaxBodyBytes, hasBody, &in); err != nil {
			a.writeError(w, r, requestID, err, status)
			return
		}
		out, err := h.Invoke(r.Context(), in)
		if err != nil {
			a.writeError(w, r, requestID, err, 0)
			return
		}
		encoded, err := json.Marshal(out)
		if err != nil || int64(len(encoded))+1 > a.opts.MaxOutputBytes {
			if err == nil {
				err = errors.New("httpadapter: response exceeds output limit")
			}
			a.writeError(w, r, requestID, operation.NewError(operation.Internal, "", err), 0)
			return
		}
		writeJSON(w, r, route.SuccessStatus, append(encoded, '\n'))
	})
	if err := install(a.mux, pattern, handler); err != nil {
		return err
	}
	a.operations[id] = true
	a.bindings = append(a.bindings, Binding{OperationID: id, Method: route.Method, Path: route.Path, SuccessStatus: route.SuccessStatus})
	return nil
}

// Bindings returns a sorted, detached snapshot of installed routes.
func (a *Adapter) Bindings() []Binding {
	a.mu.Lock()
	defer a.mu.Unlock()
	bindings := append([]Binding(nil), a.bindings...)
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].Path != bindings[j].Path {
			return bindings[i].Path < bindings[j].Path
		}
		return bindings[i].Method < bindings[j].Method
	})
	return bindings
}

func validateRoute(route Route) error {
	switch route.Method {
	case http.MethodGet, http.MethodDelete, http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return fmt.Errorf("httpadapter: unsupported method %q", route.Method)
	}
	if route.Path == "" || route.Path[0] != '/' {
		return errors.New("httpadapter: path must start with a slash")
	}
	if route.Path != "/" {
		for _, segment := range strings.Split(route.Path[1:], "/") {
			if segment == "" || segment == "." || segment == ".." {
				return errors.New("httpadapter: path must have nonempty literal segments and no trailing slash")
			}
			for _, c := range segment {
				if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("-._~", c) {
					return fmt.Errorf("httpadapter: path %q contains unsupported syntax; use literal ascii paths", route.Path)
				}
			}
		}
	}
	switch route.SuccessStatus {
	case 0, http.StatusOK, http.StatusCreated, http.StatusAccepted:
	default:
		return errors.New("httpadapter: success status must be 200, 201, or 202")
	}
	return nil
}

func install(mux *http.ServeMux, pattern string, handler http.Handler) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("httpadapter: cannot bind %q: %v", pattern, recovered)
		}
	}()
	mux.Handle(pattern, handler)
	return nil
}

func decodeInput[I, O any](w http.ResponseWriter, r *http.Request, h *operation.Handle[I, O], maxBytes int64, hasBody bool, in *I) (int, error) {
	if hasBody {
		contentType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
			return http.StatusUnsupportedMediaType, operation.NewError(operation.InvalidInput, "content-type must be application/json with utf-8 encoding", err)
		}
	}
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	limited := http.MaxBytesReader(w, body, maxBytes)
	defer func() {
		// Read-only request cleanup cannot change a completed response.
		_ = limited.Close()
	}()
	data, err := io.ReadAll(limited)
	if err != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			return http.StatusRequestEntityTooLarge, operation.NewError(operation.InvalidInput, "request body exceeds size limit", err)
		}
		if r.Context().Err() != nil {
			return 0, r.Context().Err()
		}
		return 0, operation.NewError(operation.InvalidInput, "could not read request body", err)
	}
	if !hasBody {
		if len(data) != 0 {
			return 0, operation.NewError(operation.InvalidInput, "request body is not supported for this method", nil)
		}
		return 0, nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return 0, operation.NewError(operation.InvalidInput, "request body must contain one json value", nil)
	}
	if err := h.ValidateJSON(data); err != nil {
		return 0, err
	}
	if err := json.Unmarshal(data, in); err != nil {
		return 0, operation.NewError(operation.InvalidInput, "request body does not match the operation input", err)
	}
	return 0, nil
}

// ErrorResponse is the public, transport-level error representation. Causes are
// deliberately omitted. RequestID matches the response's X-Request-ID header.
type ErrorResponse struct {
	Code      operation.Code         `json:"code"`
	Message   string                 `json:"message"`
	Fields    []operation.FieldError `json:"fields,omitempty"`
	RequestID string                 `json:"request_id"`
}

func (a *Adapter) writeError(w http.ResponseWriter, r *http.Request, requestID string, err error, overrideStatus int) {
	public := operation.PublicError(err)
	status := statusCode(public.Code)
	if overrideStatus != 0 {
		status = overrideStatus
	}
	body := ErrorResponse{Code: public.Code, Message: public.Message, Fields: public.Fields, RequestID: requestID}
	encoded, marshalErr := json.Marshal(body)
	if marshalErr != nil || int64(len(encoded))+1 > a.opts.MaxOutputBytes {
		// The small fallback fits every accepted MaxOutputBytes configuration.
		status = http.StatusInternalServerError
		body = ErrorResponse{Code: operation.Internal, Message: "internal error", RequestID: requestID}
		encoded, _ = json.Marshal(body)
	}
	writeJSON(w, r, status, append(encoded, '\n'))
}

func statusCode(code operation.Code) int {
	switch code {
	case operation.InvalidInput:
		return http.StatusBadRequest
	case operation.Unauthenticated:
		return http.StatusUnauthorized
	case operation.Forbidden:
		return http.StatusForbidden
	case operation.NotFound:
		return http.StatusNotFound
	case operation.Conflict:
		return http.StatusConflict
	case operation.RateLimited:
		return http.StatusTooManyRequests
	case operation.Unavailable:
		return http.StatusServiceUnavailable
	case operation.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case operation.Canceled:
		return http.StatusRequestTimeout
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		// A transport write error cannot be replaced with a second JSON response.
		_, _ = w.Write(data)
	}
}

func newRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

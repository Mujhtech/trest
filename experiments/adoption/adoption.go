// Package adoption provides equal application tasks through three HTTP stacks.
// This isolated experiment is not a production billing or idempotency library.
package adoption

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/mujhtech/trest/contract"
	"github.com/mujhtech/trest/httpadapter"
	"github.com/mujhtech/trest/operation"
)

const Body = `{"customer_id":"customer-acme","amount_minor":12500,"currency":"NGN","idempotency_key":"same-request"}`

type Input struct {
	CustomerID     string `json:"customer_id"`
	AmountMinor    int64  `json:"amount_minor"`
	Currency       string `json:"currency"`
	IdempotencyKey string `json:"idempotency_key"`
}

type Invoice struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type failure struct{ status int }

func (e *failure) Error() string { return http.StatusText(e.status) }
func status(err error) int {
	var e *failure
	if errors.As(err, &e) {
		return e.status
	}
	return http.StatusInternalServerError
}
func validate(in Input) error {
	if in.CustomerID == "" || in.AmountMinor <= 0 || in.AmountMinor > 100_000_000 || (in.Currency != "NGN" && in.Currency != "USD") || in.IdempotencyKey == "" {
		return &failure{http.StatusBadRequest}
	}
	return nil
}
func authorize(tenant string, in Input) error {
	if tenant == "" {
		return &failure{http.StatusUnauthorized}
	}
	if in.CustomerID != "customer-"+tenant {
		return &failure{http.StatusForbidden}
	}
	return nil
}

type stored struct {
	hash    [32]byte
	invoice Invoice
}
type Store struct {
	mu      sync.Mutex
	records map[string]stored
	writes  int
}

func (s *Store) Count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.writes }
func newStore() *Store      { return &Store{records: make(map[string]stored)} }
func (s *Store) execute(ctx context.Context, tenant string, in Input, preview bool) (Invoice, error) {
	if err := ctx.Err(); err != nil {
		return Invoice{}, err
	}
	if err := validate(in); err != nil {
		return Invoice{}, err
	}
	if err := authorize(tenant, in); err != nil {
		return Invoice{}, err
	}
	if preview {
		return Invoice{ID: "preview", TenantID: tenant, AmountMinor: in.AmountMinor, Currency: in.Currency}, nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return Invoice{}, err
	}
	hash := sha256.Sum256(data)
	// The in-memory scope includes tenant, operation version, and the synthetic
	// caller. A real database uses an atomic uniqueness constraint and transaction.
	key := tenant + "/invoices.create.v1/demo-user/" + in.IdempotencyKey
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.records[key]; ok {
		if previous.hash != hash {
			return Invoice{}, &failure{http.StatusConflict}
		}
		return previous.invoice, nil
	}
	s.writes++
	out := Invoice{ID: fmt.Sprintf("inv_%d", s.writes), TenantID: tenant, AmountMinor: in.AmountMinor, Currency: in.Currency}
	s.records[key] = stored{hash: hash, invoice: out}
	return out, nil
}

type Factory struct {
	Name string
	New  func() (*Fixture, error)
}
type Fixture struct {
	Handler http.Handler
	Store   *Store
	Export  func() ([]byte, error)
}

func Factories() []Factory {
	return []Factory{{"net_http", NewPlain}, {"trest", NewTrest}, {"huma", NewHuma}}
}
func tenant(r *http.Request) string {
	switch r.Header.Get("Authorization") {
	case "Bearer acme-test":
		return "acme"
	case "Bearer globex-test":
		return "globex"
	default:
		return ""
	}
}
func write(w http.ResponseWriter, code int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

// NewPlain implements the same domain task with handwritten net/http binding.
func NewPlain() (*Fixture, error) {
	store := newStore()
	mux := http.NewServeMux()
	for _, route := range []struct {
		path    string
		preview bool
	}{{"/invoices", false}, {"/invoices/preview", true}} {
		mux.HandleFunc("POST "+route.path, func(w http.ResponseWriter, r *http.Request) {
			identity := tenant(r)
			if identity == "" {
				write(w, 401, map[string]string{"message": "authentication required"})
				return
			}
			if r.Header.Get("Content-Type") != "application/json" {
				write(w, 415, map[string]string{"message": "application/json required"})
				return
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
			decoder.DisallowUnknownFields()
			var in Input
			if err := decoder.Decode(&in); err != nil {
				write(w, 400, map[string]string{"message": "invalid JSON"})
				return
			}
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				write(w, 400, map[string]string{"message": "invalid JSON"})
				return
			}
			if err := validate(in); err != nil {
				write(w, status(err), map[string]string{"message": err.Error()})
				return
			}
			if err := authorize(identity, in); err != nil {
				write(w, status(err), map[string]string{"message": err.Error()})
				return
			}
			out, err := store.execute(r.Context(), identity, in, route.preview)
			if err != nil {
				write(w, status(err), map[string]string{"message": err.Error()})
				return
			}
			code := 201
			if route.preview {
				code = 200
			}
			write(w, code, out)
		})
	}
	return &Fixture{Handler: mux, Store: store}, nil
}

func operationError(err error) error {
	var code operation.Code
	switch status(err) {
	case 400:
		code = operation.InvalidInput
	case 401:
		code = operation.Unauthenticated
	case 403:
		code = operation.Forbidden
	case 409:
		code = operation.Conflict
	default:
		code = operation.Internal
	}
	return operation.NewError(code, http.StatusText(status(err)), err)
}

// NewTrest uses operation enforcement and the standard HTTP adapter.
func NewTrest() (*Fixture, error) {
	store := newStore()
	registry := operation.NewRegistry(operation.AuthorizerFunc(func(_ context.Context, p operation.Principal, _ string, input any) error {
		in, ok := input.(Input)
		if !ok {
			return operation.NewError(operation.Internal, "", nil)
		}
		if err := authorize(p.TenantID, in); err != nil {
			return operationError(err)
		}
		return nil
	}))
	handles := make([]*operation.Handle[Input, Invoice], 0, 2)
	for _, route := range []struct {
		id      string
		preview bool
	}{{"invoices.create.v1", false}, {"invoices.preview.v1", true}} {
		effect := "write"
		if route.preview {
			effect = "read"
		}
		h, err := operation.Register(registry, operation.Definition[Input, Invoice]{ID: route.id, Summary: route.id, Access: operation.Permission("invoices:write"), SideEffect: effect, Validate: func(_ context.Context, in Input) error {
			if err := validate(in); err != nil {
				return operationError(err)
			}
			return nil
		}, MapError: operationError}, func(ctx context.Context, in Input) (Invoice, error) {
			p, _ := operation.PrincipalFromContext(ctx)
			return store.execute(ctx, p.TenantID, in, route.preview)
		})
		if err != nil {
			return nil, err
		}
		handles = append(handles, h)
	}
	if err := registry.Freeze(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	adapter, err := httpadapter.New(mux, registry, httpadapter.Options{Authenticate: func(r *http.Request) (operation.Principal, error) {
		identity := tenant(r)
		if identity == "" {
			return operation.Principal{}, operation.NewError(operation.Unauthenticated, "", nil)
		}
		return operation.Principal{ID: "demo-user", TenantID: identity}, nil
	}})
	if err != nil {
		return nil, err
	}
	if err := httpadapter.Bind(adapter, handles[0], httpadapter.Route{Method: "POST", Path: "/invoices", SuccessStatus: 201}); err != nil {
		return nil, err
	}
	if err := httpadapter.Bind(adapter, handles[1], httpadapter.Route{Method: "POST", Path: "/invoices/preview"}); err != nil {
		return nil, err
	}
	return &Fixture{Handler: mux, Store: store, Export: func() ([]byte, error) {
		return contract.Export(registry, adapter.Bindings(), contract.Info{Title: "Adoption experiment", Version: "1"})
	}}, nil
}

type tenantKey struct{}
type humaInput struct{ Body Input }
type humaOutput struct{ Body Invoice }

func humaError(err error) error { return huma.NewError(status(err), http.StatusText(status(err))) }

// NewHuma uses Huma's typed operations with its official net/http adapter.
func NewHuma() (*Fixture, error) {
	store := newStore()
	mux := http.NewServeMux()
	config := huma.DefaultConfig("Adoption experiment", "1")
	config.OpenAPIPath = ""
	config.DocsPath = ""
	config.SchemasPath = ""
	api := humago.New(mux, config)
	api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		identity := ""
		switch ctx.Header("Authorization") {
		case "Bearer acme-test":
			identity = "acme"
		case "Bearer globex-test":
			identity = "globex"
		}
		if identity == "" {
			_ = huma.WriteErr(api, ctx, 401, "authentication required")
			return
		}
		if !strings.HasPrefix(ctx.Header("Content-Type"), "application/json") {
			_ = huma.WriteErr(api, ctx, 415, "application/json required")
			return
		}
		next(huma.WithValue(ctx, tenantKey{}, identity))
	})
	for _, route := range []struct {
		id, path string
		preview  bool
	}{{"invoices.create.v1", "/invoices", false}, {"invoices.preview.v1", "/invoices/preview", true}} {
		code := 201
		if route.preview {
			code = 200
		}
		huma.Register(api, huma.Operation{OperationID: route.id, Method: "POST", Path: route.path, DefaultStatus: code, MaxBodyBytes: 1 << 20}, func(ctx context.Context, in *humaInput) (*humaOutput, error) {
			identity, _ := ctx.Value(tenantKey{}).(string)
			if err := validate(in.Body); err != nil {
				return nil, humaError(err)
			}
			if err := authorize(identity, in.Body); err != nil {
				return nil, humaError(err)
			}
			out, err := store.execute(ctx, identity, in.Body, route.preview)
			if err != nil {
				return nil, humaError(err)
			}
			return &humaOutput{Body: out}, nil
		})
	}
	return &Fixture{Handler: mux, Store: store, Export: func() ([]byte, error) { return json.Marshal(api.OpenAPI()) }}, nil
}

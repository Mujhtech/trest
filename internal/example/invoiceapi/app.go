// Package invoiceapi is the composition root for the invoice example.
package invoiceapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/mujhtech/trest/contract"
	"github.com/mujhtech/trest/httpadapter"
	"github.com/mujhtech/trest/internal/example/invoices"
	"github.com/mujhtech/trest/operation"
)

// App owns an explicitly constructed example API and its ephemeral storage.
type App struct {
	Handler    http.Handler
	Repository *invoices.MemoryRepository
	registry   *operation.Registry
	adapter    *httpadapter.Adapter
}

// New constructs the application without opening a port or contacting a service.
// Credentials map bearer tokens to trusted identities. An empty map is useful for
// contract export and denies all protected requests. This is a local demo, not a
// production authentication provider.
func New(credentials map[string]operation.Principal, options ...operation.Option) (*App, error) {
	repository := invoices.NewMemoryRepository(map[string]string{
		"customer-acme":   "acme",
		"customer-globex": "globex",
	})
	service := invoices.NewService(repository)
	authorizer := operation.AuthorizerFunc(func(ctx context.Context, principal operation.Principal, permission string, input any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		in, ok := input.(invoices.CreateInput)
		if !ok || permission != "invoices:write" || !repository.OwnsCustomer(principal.TenantID, in.CustomerID) {
			return operation.NewError(operation.Forbidden, "You cannot invoice this customer.", nil)
		}
		return nil
	})
	registry := operation.NewRegistry(authorizer, options...)
	create, err := operation.Register(registry, operation.Definition[invoices.CreateInput, invoices.Invoice]{
		ID: "invoices.create.v1", Summary: "Create an invoice",
		Access: operation.Permission("invoices:write"), SideEffect: "write",
		Validate: validateCreate,
		MapError: func(err error) error {
			switch {
			case errors.Is(err, invoices.ErrForbidden):
				return operation.NewError(operation.Forbidden, "You cannot invoice this customer.", err)
			case errors.Is(err, invoices.ErrInvalidInput):
				return operation.NewError(operation.InvalidInput, "Invoice details are invalid.", err)
			default:
				return err
			}
		},
	}, func(ctx context.Context, in invoices.CreateInput) (invoices.Invoice, error) {
		principal, ok := operation.PrincipalFromContext(ctx)
		if !ok {
			return invoices.Invoice{}, operation.NewError(operation.Unauthenticated, "Authentication is required.", nil)
		}
		return service.Create(ctx, principal.TenantID, in)
	})
	if err != nil {
		return nil, err
	}
	if err := registry.Freeze(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"ready\":true}\n"))
	})
	adapter, err := httpadapter.New(mux, registry, httpadapter.Options{Authenticate: authenticate(credentials)})
	if err != nil {
		return nil, err
	}
	if err := httpadapter.Bind(adapter, create, httpadapter.Route{
		Method: http.MethodPost, Path: "/invoices", SuccessStatus: http.StatusCreated,
	}); err != nil {
		return nil, err
	}
	return &App{Handler: mux, Repository: repository, registry: registry, adapter: adapter}, nil
}

func validateCreate(_ context.Context, in invoices.CreateInput) error {
	var fields []operation.FieldError
	if in.CustomerID == "" {
		fields = append(fields, operation.FieldError{Path: "/customer_id", Code: "required", Message: "Choose a customer."})
	}
	if in.AmountMinor <= 0 || in.AmountMinor > 100_000_000 {
		fields = append(fields, operation.FieldError{Path: "/amount_minor", Code: "range", Message: "Use an amount between 1 and 100000000 minor units."})
	}
	if in.Currency != "USD" && in.Currency != "NGN" {
		fields = append(fields, operation.FieldError{Path: "/currency", Code: "enum", Message: "Use USD or NGN."})
	}
	if len(fields) > 0 {
		return &operation.Error{Code: operation.InvalidInput, Message: "Invoice details are invalid.", Fields: fields}
	}
	return nil
}

func authenticate(credentials map[string]operation.Principal) func(*http.Request) (operation.Principal, error) {
	copied := make(map[string]operation.Principal, len(credentials))
	for token, principal := range credentials {
		if token != "" {
			copied[token] = principal
		}
	}
	return func(r *http.Request) (operation.Principal, error) {
		header := r.Header.Get("Authorization")
		scheme, token, ok := strings.Cut(header, " ")
		if ok && strings.EqualFold(scheme, "Bearer") && token != "" {
			for expected, principal := range copied {
				if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
					return principal, nil
				}
			}
		}
		return operation.Principal{}, operation.NewError(operation.Unauthenticated, "A valid bearer token is required.", nil)
	}
}

// OpenAPI returns a deterministic contract without requiring credentials.
func (a *App) OpenAPI() ([]byte, error) {
	return contract.Export(a.registry, a.adapter.Bindings(), contract.Info{Title: "Trest invoice example", Version: "0.1.0"})
}

// Registry exposes the already frozen operation model for local developer tools.
func (a *App) Registry() *operation.Registry { return a.registry }

// Bindings returns detached transport metadata for inspection and checking.
func (a *App) Bindings() []httpadapter.Binding { return a.adapter.Bindings() }

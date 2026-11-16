package adoption

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/go-chi/chi/v5"
	"github.com/labstack/echo/v5"
	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/operation"
)

// NewChi serves the common invoice task through Chi's router.
func NewChi() (*Fixture, error) {
	store := newStore()
	router := chi.NewRouter()
	for _, route := range invoiceRoutes() {
		router.Post(route.path, func(w http.ResponseWriter, r *http.Request) {
			in, identity, err := decodeAuthorizedInvoice(w, r)
			if err != nil {
				write(w, status(err), map[string]string{"message": err.Error()})
				return
			}
			out, err := store.execute(r.Context(), identity, in, route.preview)
			if err != nil {
				write(w, status(err), map[string]string{"message": err.Error()})
				return
			}
			write(w, route.status, out)
		})
	}
	return &Fixture{Handler: router, Store: store}, nil
}

// NewGin uses Gin routing and JSON responses without request-logging middleware.
// The benchmark runner selects GIN_MODE=release before the process starts.
func NewGin() (*Fixture, error) {
	store := newStore()
	router := gin.New()
	for _, route := range invoiceRoutes() {
		router.POST(route.path, func(c *gin.Context) {
			in, identity, err := decodeAuthorizedInvoice(c.Writer, c.Request)
			if err != nil {
				c.JSON(status(err), map[string]string{"message": err.Error()})
				return
			}
			out, err := store.execute(c.Request.Context(), identity, in, route.preview)
			if err != nil {
				c.JSON(status(err), map[string]string{"message": err.Error()})
				return
			}
			c.JSON(route.status, out)
		})
	}
	return &Fixture{Handler: router, Store: store}, nil
}

// NewEcho uses Echo routing and JSON responses without request-logging middleware.
func NewEcho() (*Fixture, error) {
	store := newStore()
	router := echo.New()
	for _, route := range invoiceRoutes() {
		router.POST(route.path, func(c *echo.Context) error {
			in, identity, err := decodeAuthorizedInvoice(c.Response(), c.Request())
			if err != nil {
				return c.JSON(status(err), map[string]string{"message": err.Error()})
			}
			out, err := store.execute(c.Request().Context(), identity, in, route.preview)
			if err != nil {
				return c.JSON(status(err), map[string]string{"message": err.Error()})
			}
			return c.JSON(route.status, out)
		})
	}
	return &Fixture{Handler: router, Store: store}, nil
}

// NewTrestApp exercises Trest's top-level application API. NewTrest remains the
// original operation registry and HTTP adapter fixture for historical comparison.
func NewTrestApp() (*Fixture, error) {
	store := newStore()
	components := []trest.Component{
		trest.Info("Framework comparison", "1"),
		trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
			identity := tenant(r)
			if identity == "" {
				return trest.Principal{}, operation.NewError(operation.Unauthenticated, "", nil)
			}
			return trest.Principal{ID: "demo-user", TenantID: identity}, nil
		}),
		trest.RequireAuth(),
		trest.Limits(1<<20, 1<<20),
	}
	for _, route := range invoiceRoutes() {
		endpoint := trest.Post(route.path, func(ctx context.Context, in Input) (Invoice, error) {
			principal, _ := trest.Identity(ctx)
			return store.execute(ctx, principal.TenantID, in, route.preview)
		}).Name(route.name).Status(route.status).Validate(func(_ context.Context, in Input) error {
			if err := validate(in); err != nil {
				return operationError(err)
			}
			return nil
		}).Authorize(func(_ context.Context, principal trest.Principal, in Input) error {
			if err := authorize(principal.TenantID, in); err != nil {
				return operationError(err)
			}
			return nil
		}).MapError(operationError)
		components = append(components, endpoint)
	}
	app, err := trest.New(components...)
	if err != nil {
		return nil, err
	}
	return &Fixture{Handler: app, Store: store, Export: app.OpenAPI}, nil
}

type invoiceRoute struct {
	name    string
	path    string
	status  int
	preview bool
}

func invoiceRoutes() []invoiceRoute {
	return []invoiceRoute{
		{name: "createInvoice", path: "/invoices", status: http.StatusCreated},
		{name: "previewInvoice", path: "/invoices/preview", status: http.StatusOK, preview: true},
	}
}

// decodeAuthorizedInvoice gives Chi, Gin, and Echo the same strict standard-
// library JSON binding and business-policy checks as the plain HTTP fixture.
// It deliberately avoids their default binders, whose unknown-field and trailing-
// value rules differ. Each router still performs its own dispatch and response
// handling; no request passes through a second router.
func decodeAuthorizedInvoice(w http.ResponseWriter, r *http.Request) (Input, string, error) {
	identity := tenant(r)
	if identity == "" {
		return Input{}, "", &failure{status: http.StatusUnauthorized}
	}
	if r.Header.Get("Content-Type") != "application/json" {
		return Input{}, "", &failure{status: http.StatusUnsupportedMediaType}
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var in Input
	if err := decoder.Decode(&in); err != nil {
		return Input{}, "", &failure{status: http.StatusBadRequest}
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Input{}, "", &failure{status: http.StatusBadRequest}
	}
	if err := validate(in); err != nil {
		return Input{}, "", err
	}
	if err := authorize(identity, in); err != nil {
		return Input{}, "", err
	}
	return in, identity, nil
}

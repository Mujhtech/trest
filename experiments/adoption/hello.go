package adoption

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/gin-gonic/gin"
	"github.com/go-chi/chi/v5"
	"github.com/labstack/echo/v5"
	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/httpadapter"
	"github.com/mujhtech/trest/operation"
)

// Hello is serialized for every request, including in the plain HTTP baseline.
// No implementation returns pre-encoded bytes or a framework response cache.
type Hello struct {
	Message string `json:"message"`
}

// HelloFactories uses one static GET route without authentication or optional
// middleware. Framework defaults, including Trest's invocation checks, remain on.
func HelloFactories() []Factory {
	return []Factory{
		{"net_http", func() (*Fixture, error) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /hello", func(w http.ResponseWriter, _ *http.Request) {
				write(w, http.StatusOK, Hello{Message: "hello"})
			})
			return &Fixture{Handler: mux}, nil
		}},
		{"trest_app", func() (*Fixture, error) {
			app, err := trest.New(trest.Get("/hello", trest.Const(Hello{Message: "hello"})).Name("hello"))
			return &Fixture{Handler: app}, err
		}},
		{"trest", newAdapterHello},
		{"huma", func() (*Fixture, error) {
			mux := http.NewServeMux()
			config := huma.DefaultConfig("Framework benchmark", "1")
			// Omit the optional schema-link response transform so all stacks
			// serialize the same one-field JSON object on this endpoint.
			config.CreateHooks = nil
			config.OpenAPIPath, config.DocsPath, config.SchemasPath = "", "", ""
			api := humago.New(mux, config)
			type output struct{ Body Hello }
			huma.Register(api, huma.Operation{OperationID: "hello", Method: http.MethodGet, Path: "/hello"},
				func(context.Context, *struct{}) (*output, error) {
					return &output{Body: Hello{Message: "hello"}}, nil
				})
			return &Fixture{Handler: mux}, nil
		}},
		{"chi", func() (*Fixture, error) {
			mux := chi.NewRouter()
			mux.Get("/hello", func(w http.ResponseWriter, _ *http.Request) {
				write(w, http.StatusOK, Hello{Message: "hello"})
			})
			return &Fixture{Handler: mux}, nil
		}},
		{"gin", func() (*Fixture, error) {
			app := gin.New()
			app.GET("/hello", func(ctx *gin.Context) {
				ctx.JSON(http.StatusOK, Hello{Message: "hello"})
			})
			return &Fixture{Handler: app}, nil
		}},
		{"echo", func() (*Fixture, error) {
			app := echo.New()
			app.GET("/hello", func(ctx *echo.Context) error {
				return ctx.JSON(http.StatusOK, Hello{Message: "hello"})
			})
			return &Fixture{Handler: app}, nil
		}},
	}
}

func newAdapterHello() (*Fixture, error) {
	registry := operation.NewRegistry(nil)
	handle, err := operation.Register(registry, operation.Definition[struct{}, Hello]{
		ID: "hello", Summary: "Hello", Access: operation.Public(),
	}, func(context.Context, struct{}) (Hello, error) {
		return Hello{Message: "hello"}, nil
	})
	if err != nil {
		return nil, err
	}
	if err := registry.Freeze(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	adapter, err := httpadapter.New(mux, registry, httpadapter.Options{})
	if err != nil {
		return nil, err
	}
	if err := httpadapter.Bind(adapter, handle, httpadapter.Route{Method: http.MethodGet, Path: "/hello"}); err != nil {
		return nil, err
	}
	return &Fixture{Handler: mux}, nil
}

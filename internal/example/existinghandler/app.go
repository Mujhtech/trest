// Package existinghandler demonstrates mounting an operation beside an ordinary
// http.Handler on an application-owned ServeMux.
package existinghandler

import (
	"context"
	"net/http"

	"trest.local/trest/contract"
	"trest.local/trest/httpadapter"
	"trest.local/trest/operation"
)

// Greeting is the public operation's response.
type Greeting struct {
	Message string `json:"message"`
}

// App exposes a mixed application with a normal handler and a typed operation.
type App struct {
	Handler  http.Handler
	registry *operation.Registry
	adapter  *httpadapter.Adapter
}

func New() (*App, error) {
	registry := operation.NewRegistry(nil)
	greet, err := operation.Register(registry, operation.Definition[struct{}, Greeting]{
		ID: "greeting.read.v1", Summary: "Read a greeting", Access: operation.Public(), SideEffect: "read",
	}, func(context.Context, struct{}) (Greeting, error) {
		return Greeting{Message: "Hello from Trest"}, nil
	})
	if err != nil {
		return nil, err
	}
	if err := registry.Freeze(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	adapter, err := httpadapter.New(mux, registry, httpadapter.Options{})
	if err != nil {
		return nil, err
	}
	if err := httpadapter.Bind(adapter, greet, httpadapter.Route{Method: http.MethodGet, Path: "/hello"}); err != nil {
		return nil, err
	}
	return &App{Handler: mux, registry: registry, adapter: adapter}, nil
}

// OpenAPI describes the explicitly bound Trest operation; the handwritten
// /healthz handler remains application-owned and is not inferred automatically.
func (a *App) OpenAPI() ([]byte, error) {
	return contract.Export(a.registry, a.adapter.Bindings(), contract.Info{Title: "Trest existing-handler example", Version: "0.1.0"})
}

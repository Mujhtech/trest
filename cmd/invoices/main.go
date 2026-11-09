// Command invoices runs the local invoice example or exports its contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"trest.local/trest/internal/example/invoiceapi"
	"trest.local/trest/internal/example/server"
	"trest.local/trest/operation"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	address := flag.String("addr", "127.0.0.1:8080", "listen address")
	export := flag.Bool("openapi", false, "write OpenAPI JSON and exit without listening")
	flag.Parse()
	credentials := make(map[string]operation.Principal)
	if token := os.Getenv("TREST_DEMO_TOKEN"); token != "" {
		credentials[token] = operation.Principal{ID: "demo-user", TenantID: "acme"}
	} else if !*export {
		return fmt.Errorf("set TREST_DEMO_TOKEN before starting the local demo")
	}
	app, err := invoiceapi.New(credentials)
	if err != nil {
		return err
	}
	if *export {
		data, err := app.OpenAPI()
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(append(data, '\n'))
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Printf("invoice demo listening on %s; data is kept in memory", *address)
	instance := os.Getenv("TREST_DEV_INSTANCE")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if instance != "" && r.URL.Path == "/readyz" {
			w.Header().Set("X-Trest-Dev-Instance", instance)
		}
		app.Handler.ServeHTTP(w, r)
	})
	return server.Run(ctx, *address, handler)
}

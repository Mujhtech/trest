// Command trest-demo serves the public-API invoice workshop or exports its contract.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"trest.local/trest"
	"trest.local/trest/clientgen"
	"trest.local/trest/examples/invoices"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	address := flag.String("addr", "127.0.0.1:3000", "HTTP listen address")
	manifest := flag.Bool("manifest", false, "write a route manifest to stdout without serving")
	openapi := flag.Bool("openapi", false, "write OpenAPI to stdout without serving")
	ts := flag.Bool("client-ts", false, "write a TypeScript client to stdout without serving")
	goClient := flag.Bool("client-go", false, "write a Go client to stdout without serving")
	flag.Parse()
	count := 0
	for _, enabled := range []bool{*openapi, *ts, *goClient, *manifest} {
		if enabled {
			count++
		}
	}
	if count > 1 {
		return fmt.Errorf("choose one export flag")
	}
	token := os.Getenv("TREST_DEMO_TOKEN")
	credentials := map[string]string{}
	if token != "" {
		credentials[token] = "acme"
	}
	if token == "" && count == 0 {
		return fmt.Errorf("set TREST_DEMO_TOKEN to a local test token before starting")
	}
	instance := os.Getenv("TREST_DEV_INSTANCE")
	app, err := invoices.App(invoices.NewService(), credentials, trest.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/readyz" && instance != "" {
				w.Header().Set("X-Trest-Dev-Instance", instance)
			}
			next.ServeHTTP(w, r)
		})
	}))
	if err != nil {
		return err
	}
	if *manifest {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema_version": "trest.inspect.v1", "routes": app.Routes()})
	}
	if count > 0 {
		document, err := app.OpenAPI()
		if err != nil {
			return err
		}
		output := document
		if *ts {
			output, err = clientgen.TypeScript(document)
		}
		if *goClient {
			output, err = clientgen.Go(document, "invoiceclient")
		}
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(output)
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	fmt.Fprintf(os.Stderr, "Invoice workshop: http://%s/docs\n", *address)
	return app.Listen(ctx, *address)
}

// Command existing-handler runs normal net/http handlers alongside Trest.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"trest.local/trest/internal/example/existinghandler"
	"trest.local/trest/internal/example/server"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	address := flag.String("addr", "127.0.0.1:8081", "listen address")
	export := flag.Bool("openapi", false, "write OpenAPI JSON and exit without listening")
	flag.Parse()
	app, err := existinghandler.New()
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
	log.Printf("existing-handler example listening on %s", *address)
	return server.Run(ctx, *address, app.Handler)
}

// Command echo serves a local authenticated, typed JSON WebSocket fixture.
package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mujhtech/trest"
	trestws "github.com/mujhtech/trest/adapters/websocket"
	"github.com/mujhtech/trest/operation"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

type message struct {
	Room string `json:"room" minLength:"1" maxLength:"64"`
	Text string `json:"text" minLength:"1" maxLength:"1024"`
}

func run() error {
	address := flag.String("addr", "127.0.0.1:8082", "local listen address")
	origin := flag.String("origin", "http://localhost:3000", "exact allowed browser origin")
	flag.Parse()
	token := os.Getenv("TREST_WS_TOKEN")
	if token == "" {
		return fmt.Errorf("set TREST_WS_TOKEN to a local demo token")
	}
	socket, err := trestws.NewJSON[message, message]("/echo", trestws.Options{Name: "echo", Permission: "socket:connect", Origins: []string{*origin}, AllowMissingOrigin: true}, func(ctx context.Context, c *trestws.JSONConn[message, message]) error {
		for {
			payload, err := c.Read(ctx)
			if err != nil {
				return err
			}
			// Connection authorization does not authorize arbitrary rooms. The
			// application checks the resource named by each validated message.
			principal, ok := trest.Identity(ctx)
			if !ok || payload.Room != principal.TenantID {
				return trest.NewError(trest.Forbidden, "room access denied", nil)
			}
			if err = c.Write(ctx, payload); err != nil {
				return err
			}
		}
	})
	if err != nil {
		return err
	}
	app, err := trest.New(
		trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
			scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			if ok && strings.EqualFold(scheme, "Bearer") && subtle.ConstantTimeCompare([]byte(value), []byte(token)) == 1 {
				return trest.Principal{ID: "local-demo", TenantID: "fixture"}, nil
			}
			return trest.Principal{}, nil
		}),
		trest.Authorization(operation.AuthorizerFunc(func(_ context.Context, p operation.Principal, permission string, _ any) error {
			if p.ID == "local-demo" && permission == "socket:connect" {
				return nil
			}
			return operation.NewError(operation.Forbidden, "access denied", nil)
		})),
		socket,
	)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return app.Listen(ctx, *address)
}

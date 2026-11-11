// Command demo serves one protected endpoint with a synthetic local JWT.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mujhtech/trest"
	trestjwt "github.com/mujhtech/trest/adapters/jwt"
)

type claims struct {
	jwt.RegisteredClaims
	Tenant string `json:"tenant"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// This short-lived synthetic key belongs only to the loopback demo.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	authentication, err := trestjwt.New(trestjwt.Options[*claims]{
		Algorithm: "HS256", Issuer: "trest-local-demo", Audience: "trest-local-api",
		NewClaims: func() *claims { return &claims{} },
		Key: func(_ context.Context, kid string) (any, error) {
			if kid != "local" {
				return nil, errors.New("unknown key")
			}
			return key, nil
		},
		Principal: func(_ context.Context, c *claims) (trest.Principal, error) {
			return trest.Principal{ID: c.Subject, TenantID: c.Tenant}, nil
		},
	})
	if err != nil {
		return err
	}
	app, err := trest.New(authentication, trest.RequireAuth(),
		trest.Get("/me", func(ctx context.Context, _ trest.Empty) (trest.Principal, error) {
			principal, _ := trest.Identity(ctx)
			return principal, nil
		}),
	)
	if err != nil {
		return err
	}
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "trest-local-demo", Audience: jwt.ClaimStrings{"trest-local-api"}, Subject: "demo-user",
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
		},
		Tenant: "acme",
	})
	token.Header["kid"] = "local"
	encoded, err := token.SignedString(key)
	if err != nil {
		return err
	}
	fmt.Printf("Synthetic local token (expires in five minutes):\n%s\n\nGET http://127.0.0.1:8083/me with Authorization: Bearer <token>\n", encoded)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return app.Listen(ctx, "127.0.0.1:8083")
}

# Trest

Trest is a Go web framework inspired by Elysia's route-first composition. Typed handlers, scoped policies, validation, OpenAPI, and generated clients share one application definition. The core requires Go 1.23 and uses only the standard library.

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"

    "trest.local/trest"
)

func main() {
    app, err := trest.New(
        trest.Get("/", trest.Const("Hello Trest")),
        trest.Docs("/docs"),
    )
    if err != nil {
        log.Fatal(err)
    }

    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
    defer stop()
    if err := app.Listen(ctx, "127.0.0.1:3000"); err != nil {
        log.Fatal(err)
    }
}
```

`New` validates and freezes the application; `Listen` starts it and shuts it down when the context is canceled. An `App` is also an `http.Handler`. Handlers receive ordinary `context.Context` values, and business services remain ordinary Go code with explicitly constructed dependencies.

APIs and the module path `trest.local/trest` are provisional. No public installation or release location has been selected. The project does not claim full Elysia parity.

## Try the invoice workshop

From this checkout, choose an explicit local demo token:

```sh
export TREST_DEMO_TOKEN='choose-a-local-test-token'
go run ./cmd/trest-demo
```

Open [the interactive docs](http://127.0.0.1:3000/docs) and enter `Bearer ` followed by your token in the Authorization field. In another terminal, set the same token and create an invoice:

```sh
export TREST_DEMO_TOKEN='choose-a-local-test-token'
curl http://127.0.0.1:3000/api/invoices \
  -H "Authorization: Bearer $TREST_DEMO_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"customer_id":"customer-acme","amount_minor":12500,"currency":"NGN"}'
```

The workshop includes CRUD, typed query and path inputs, tenant authorization, receipt uploads, and an event stream. It stores data in memory and maps the demo token to tenant `acme`. See the [workshop guide](examples/invoices/README.md) for the complete flow and typed client example.

## Start your own app

Run these commands from this checkout; choose a destination that does not already exist:

```sh
go run ./cmd/trest new /tmp/my-trest-app
go run ./cmd/trest dev --dir /tmp/my-trest-app --watch
```

The scaffold is an independent Go module with a server, interactive docs, and a request test. Its `go.mod` uses a local `replace` pointing to this checkout until Trest has a public module identity. The development server defaults to `127.0.0.1:8080`; open `/docs` or `/greet/Ada`.

Export its contract and generate a TypeScript client without starting the server:

```sh
go run ./cmd/trest contract export --dir /tmp/my-trest-app --out /tmp/my-trest-app/openapi.json
go run ./cmd/trest client --spec /tmp/my-trest-app/openapi.json --lang ts --out /tmp/my-trest-app/client.ts
```

Use `--lang go --package client` for a Go client. Existing output files require `--overwrite`. The [development guide](docs/development.md) covers arbitrary local projects, inspection, client regeneration, and tests.

## Build with Trest

- [Framework guide](docs/framework.md): typed routes, recursive models, declared codecs, resource resolution, guards, response variants, event streams, and lifecycle ownership.
- [Web plugins](docs/web-plugins.md): CORS, signed cookies, static files, interval tasks, and optional [typed WebSockets](adapters/websocket/README.md), [JWT authentication](adapters/jwt/README.md), and [OpenTelemetry tracing](adapters/otel/README.md).
- [Generated clients](clientgen/README.md): Go and TypeScript models, typed request and response variants, forms/uploads, event streams, and WebSocket messages.
- [Jobs](docs/milestone-3.md) and [AI tools](docs/milestone-4.md): explicit opt-in integrations using the shared operation model. Native HTTP routes are excluded from the JSON invocation bridge.

Schemas come from supported Go types and declarative tags, including recursive and embedded JSON models. Custom JSON/text codecs declare their wire contract through `TrestSchema`; typed status responses and real-time messages share validation and client metadata. Arbitrary interface unions and unrestricted JSON Schema remain outside the supported subset. Authentication, resource authorization, and provider configuration stay application-owned; see the framework and client guides for exact boundaries.

## Develop Trest

```sh
go test -race ./...
go vet ./...
go build ./...
scripts/check-all.sh
```

The last command checks every module. WebSocket and JWT adapters require Go 1.23; MCP and PostgreSQL adapters require Go 1.25; the OpenTelemetry adapter and Huma adoption experiment require Go 1.26. PostgreSQL integration tests require an isolated disposable database.

The lower-level `operation`, `httpadapter`, `contract`, inspection, scenario, job, and tool packages remain available for explicit integration and compatibility. The [design paper](docs/Go_Framework_Design_Paper.pdf) records the architectural foundation; [implementation status](docs/status.md) records completed verification and documented boundaries.

The [performance report](docs/benchmarks/optimization/README.md) records the latest profiles and paired benchmarks: typed POST time fell 53% and allocations fell 75%, with validation and authorization preserved. Plain `net/http` remains faster in the measured workloads.

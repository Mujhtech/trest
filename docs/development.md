# Development guide

The commands below run from the Trest checkout unless a different directory is shown. Trest's module path is `github.com/mujhtech/trest`. The scaffold links to your checkout with a local `replace` for local development instead of downloading a release.

## Create and run a project

Choose a destination that does not already exist:

```sh
go run ./cmd/trest new /tmp/my-trest-app
```

The command creates `go.mod`, `cmd/server/main.go`, a request test, a README, and `.gitignore`. The default module name is `example.local/my-trest-app`. Override it with `--module example.com/myapp`; use `--trest-dir /path/to/trest` if the command cannot locate this checkout. It refuses to overwrite an existing destination.

Run the independent project directly:

```sh
cd /tmp/my-trest-app
go test ./...
go run ./cmd/server
```

Open [the starter docs](http://127.0.0.1:8080/docs) or request [a greeting](http://127.0.0.1:8080/greet/Ada). The server uses `signal.NotifyContext` for graceful shutdown. Moving either project or checkout requires updating the local replacement in the generated `go.mod`.

## Rebuild a local application

From the Trest checkout:

```sh
go run ./cmd/trest dev --dir /tmp/my-trest-app --watch
```

`dev` builds `./cmd/server` in the selected directory, starts it on `127.0.0.1:8080`, and checks readiness. Use `--package ./cmd/api` for another local main package, `--addr 127.0.0.1:8085` for another loopback port, and `--ready-path /healthz` for another readiness route. The directory must contain `go.mod`; package patterns and arbitrary shell commands are not accepted.

The selected app must implement these conventions, which the scaffold already supplies:

- `-addr` selects its HTTP address.
- `-openapi` writes OpenAPI JSON to stdout and exits without opening a listener.
- `-manifest` writes a JSON object with `schema_version: "trest.app.v1"` and `routes: app.Routes()` and exits without serving.
- Its readiness endpoint returns HTTP 200 with `X-Trest-Dev-Instance` equal to the `TREST_DEV_INSTANCE` environment value. This distinguishes the newly built child from a different process on that port.

With `--dir`, the child inherits your environment. Your application still owns its configuration and credentials. These commands build and execute local application code, including export modes; they do not discover or sandbox arbitrary services. Keep export construction free of network or privileged side effects.

`--watch` polls Go sources and module files. A failed rebuild or client-generation step preserves the previous running process and emits diagnostics. A successful rebuild stops the old process, starts the new one, and checks its instance marker. A replacement that cannot start or become ready fails the command. In-memory application data resets on restart. Ctrl-C cancels the child and cleans up temporary build files.

`--poll` defaults to `500ms` and must be at least `50ms`. `--build-timeout` defaults to one minute and cannot exceed ten minutes. Without `--watch`, `dev` builds once and runs until stopped.

Omitting both `--dir` and `--package` retains the earlier invoice fixture mode using `./cmd/invoices` and the synthetic token `local-dev-fixture-token`. For the public-API workshop, select it explicitly:

```sh
export TREST_DEMO_TOKEN='choose-a-local-test-token'
go run ./cmd/trest dev --dir . --package ./cmd/trest-demo --addr 127.0.0.1:3000 --watch
```

## Inspect and export

For a scaffolded project:

```sh
go run ./cmd/trest inspect --dir /tmp/my-trest-app --json
go run ./cmd/trest contract export --dir /tmp/my-trest-app --out /tmp/my-trest-app/openapi.json
```

These commands build the chosen main package and invoke its export flag. `inspect` without `--json` prints method, path, name, and summary. The JSON output also preserves route schemas, policies, and caller `Source` metadata supplied by the application. Add `--package` for a nondefault main package.

`contract export` writes stdout by default; `--out` creates a file. Existing regular output files require `--overwrite`; symlinks and directories are rejected. Parent output directories must already exist. The public workshop also exports directly without a token or listener:

```sh
go run ./cmd/trest-demo -openapi > /tmp/invoices.openapi.json
go run ./cmd/trest-demo -manifest > /tmp/invoices.routes.json
```

For operation-level tracing, compose `trest.Observe(recorder)` into the application. `trest.AfterResponse` adds HTTP request and delivery outcomes. A route's caller source and shared request ID connect those records; see the [framework guide](framework.md#documentation-clients-and-observation).

## Check your application and contract

```sh
go run ./cmd/trest check --dir /tmp/my-trest-app --json
go run ./cmd/trest contract export --dir /tmp/my-trest-app --baseline --out /tmp/my-trest-app/openapi.baseline.json
go run ./cmd/trest check --dir /tmp/my-trest-app --baseline /tmp/my-trest-app/openapi.baseline.json --run-go-checks --json
```

`check --dir` builds the selected main package once and runs its `-manifest` and `-openapi` exports. It checks that route names, methods, paths, authorization metadata, native protocols, and success statuses agree between them. JSON reports include the selected application's route manifest under `application`. Add `--package ./cmd/api` to choose another main package. Construction and compiler errors appear on stderr while stdout remains a JSON report.

`--baseline` compares the complete contract with a saved export. Project baselines normalize JSON formatting and the display title/version; changes to routes, schemas, statuses, permissions, and other contract fields still cause `contract_drift` and a nonzero exit. This is a review signal, not an automatic determination that a change breaks clients. After reviewing a change, deliberately replace the baseline using `contract export --baseline --overwrite`. Ordinary contract export preserves the application's title and version.

Without `--run-go-checks`, project checking still builds and executes the application's export modes. The flag additionally runs `go test -race ./...`, `go vet ./...`, and `go build ./...` in that project. `--timeout` bounds the complete project check, including exports, and defaults to two minutes. The no-directory fixture mode described below remains available.

## Generate Go and TypeScript clients

```sh
go run ./cmd/trest client --spec /tmp/my-trest-app/openapi.json --lang ts --out /tmp/my-trest-app/client.ts
mkdir -p /tmp/my-trest-app/client
go run ./cmd/trest client --spec /tmp/my-trest-app/openapi.json --lang go --package client --out /tmp/my-trest-app/client/client.go
```

Generation reads a bounded local OpenAPI 3.1 JSON file and makes no network request. Omit `--out` to print source to stdout. Add `--overwrite` when intentionally regenerating an existing file. The generated Go client uses the standard library; TypeScript uses web APIs such as `fetch` and `FormData`.

Regenerate a client after each successful development build:

```sh
go run ./cmd/trest dev --dir /tmp/my-trest-app --watch --client ts --client-out client.ts
```

For Go, use `--client go --client-package client --client-out client/client.go`. Relative output paths resolve against the app directory. The watcher creates a missing file or replaces one bearing Trest's generated-file marker; it refuses to overwrite handwritten code. Unchanged output is left untouched to avoid rebuild loops.

Clients have named input and result types, preserve recursive model references, status and headers, and return public API errors. Declared response alternatives select a typed body by status. Form and multipart inputs expose typed fields and uploads, with explicit native-body options where needed. `Events[T]` and `NewJSON` WebSocket routes supply typed event/message contracts to generated readers and native connection wrappers. Arbitrary untyped streams and binary responses remain native response objects; consume or close their bodies. Configure authentication explicitly. See [client generation](../clientgen/README.md) for exact generated APIs, transport ownership, response bounds, browser cookie behavior, and schema limits.

The [invoice workshop](../examples/invoices/README.md) includes a generated TypeScript client and a typed usage example covering create, read, update, upload, list, stream snapshot, and delete.

## Test changes

Core changes require:

```sh
go test -race ./...
go vet ./...
go build ./...
```

Core tests use in-memory fakes and `httptest`; no production credentials are needed. Test authorization rejection before handler execution and verify that invalid input has no business effect. Keep contracts deterministic.

Run every module explicitly:

```sh
scripts/check-all.sh
```

| Module | Minimum Go version | Dependencies |
| --- | --- | --- |
| Root framework, plugins, CLI, examples | 1.23 | Standard library only |
| `adapters/websocket` | 1.23 | Coder WebSocket SDK |
| `adapters/jwt` | 1.23 | golang-jwt/jwt v5 |
| `adapters/mcp` | 1.25 | MCP SDK |
| `adapters/postgres` | 1.25 | pgx |
| `adapters/otel` | 1.26 | OpenTelemetry API; SDK for tests |
| `experiments/adoption` | 1.26 | Huma |

Go's root `./...` does not cross nested module boundaries. Keep optional integrations and their dependencies in their own modules. The all-module script runs race tests, vet, and build in each module with `GOWORK=off`.

Set `TREST_TEST_WEB=1` to include the root module's integration suite in that run. Add `TREST_TEST_WEBSOCKET=1` for the adapter's real loopback connection tests:

```sh
TREST_TSC=/path/to/typescript/bin/tsc \
  TREST_TEST_WEB=1 TREST_TEST_WEBSOCKET=1 scripts/check-all.sh
```

Integration checks explicitly compile generated clients and exercise scaffold/build/export/reload behavior:

```sh
TREST_TSC=/path/to/typescript/bin/tsc \
  go test -race -tags=integration ./clientgen ./examples/invoices ./cmd/trest
```

The TypeScript checks require Node.js and use `TREST_TSC`, then `tsc` on PATH, reporting a skip if neither compiler location is available. Go compiler checks still run. CLI integration tests create disposable projects and loopback listeners. Generated-client tests include compile-time rejection of incorrect input types and runtime requests against controlled transports; the invoice Go client also exercises the actual in-memory app.

PostgreSQL integration uses `TREST_TEST_POSTGRES_DSN` and requires an isolated disposable database; never point it at an existing application database. Follow the [jobs guide](milestone-3.md) for setup and cleanup.

The source contains coverage for hello/docs contracts, typed request decoding, scoped authorization and direct invocation, native response boundaries, lifecycle rollback, stream cancellation, web plugins, generated clients, and CLI reloads. These test files describe expected behavior; [implementation status](status.md) records executed verification and its limits.

## Lower-level compatibility workflows

The earlier `operation`, `httpadapter`, `contract`, `inspect`, `check`, `scenario`, `telemetry`, `job`, and `aitool` packages remain available. The following commands still use the built-in lower-level invoice fixture:

```sh
go run ./cmd/trest inspect --json
go run ./cmd/trest check --run-go-checks --json
go run ./cmd/trest scenario list --json
go run ./cmd/trest scenario run invoice-create --local --json --trace-file /tmp/trest-traces.json
go run ./cmd/trest trace list /tmp/trest-traces.json --json
```

`check` without `--dir` validates the built-in fixture, and without `--run-go-checks` executes no Go subprocess. Supplying `--dir` selects the project checking workflow above. Scenario execution still uses the fixture and requires `--local`; trace export creates a new private file rather than replacing an existing one. Use a fresh filename for each export. See the [original developer tooling guide](milestone-2.md) for fixture scenarios, baselines, and trace handling.

Keep business services independent of Trest and construct dependencies explicitly. The [architecture paper](Go_Framework_Design_Paper.pdf) remains the architecture source; the [framework guide](framework.md) describes the implemented web API.

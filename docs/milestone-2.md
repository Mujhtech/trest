# Milestone 2: inspect, check, run, and observe

Trest now provides a runnable local developer loop around the invoice example. The reusable `inspect`, `check`, `scenario`, and `telemetry` packages are independent of the example command. The command is explicit application composition, not automatic discovery of arbitrary applications.

## Reproduce the loop

From the repository root:

```sh
go run ./cmd/trest inspect --json
go run ./cmd/trest contract export --format openapi
go run ./cmd/trest check --json
go run ./cmd/trest check --run-go-checks --json
go run ./cmd/trest scenario list --json
go run ./cmd/trest scenario run invoice-create --local --json --trace-file /tmp/trest-local-traces.json
go run ./cmd/trest trace list /tmp/trest-local-traces.json --json
# Use a trace ID returned by the previous commands:
go run ./cmd/trest trace show /tmp/trest-local-traces.json TRACE_ID --json
```

Use a new trace-file path for each run: trace export creates a private file exclusively and never overwrites an existing file. No production credentials or external database are needed. `invoice-create`, `invoice-denied`, and `invoice-validation` use fresh in-memory storage and synthetic identities on every run. Their assertions are authored in `cmd/trest/scenarios.go`; the runner does not generate tests or decide expected outcomes.

The feedback loop is concrete: inspect the operation and its source location, change ordinary Go application code, run checks, execute a configured scenario, and inspect its HTTP status, storage assertion, and phase trace. A scenario's output includes the generated trace IDs; events carry the HTTP response's request ID. Tests exercise this loop end to end.

## Inspect actual registration and save a manifest

`inspect.Build` requires a frozen registry and validates the supplied HTTP bindings. The `trest.inspect.v1` envelope contains sorted operation metadata, input/output schemas, access declarations, timeouts, declared effects, registration source location, and explicit transport mappings. It contains no handlers, repository objects, credentials, or request/response bodies.

Source locations identify the call to `Register`, not an inferred graph of service dependencies. Paths are reduced to filenames to avoid leaking local home directories. Runtime inspection and a saved manifest use the same deterministic representation:

```sh
go run ./cmd/trest inspect --json > /tmp/trest-application.json
```

The saved manifest can be reviewed without starting the application. It may still reveal architecture and permissions, so the application owns access policy for published metadata. No public metadata endpoint is enabled by these packages.

## Check and contract drift

The default `check` command validates metadata and bindings without executing application code or spawning a process. `--run-go-checks` explicitly runs `go test -race ./...`, `go vet ./...`, and `go build ./...` in order. The package accepts only configured `test`, `vet`, and `build` checks and validated package arguments, passed directly to `exec.CommandContext`; no shell string is interpreted. All check configuration comes from trusted application code or explicit CLI flags, not inspection metadata or model output.

The `trest.check.v1` envelope contains success state, stable diagnostic codes, operation/path details where known, named checks, and exit statuses. Raw subprocess output is excluded because it can contain secrets or private test data; a failed check tells the developer which normal Go command to run for local diagnostics. `--timeout` bounds the check process. Application-authored tests remain responsible for cancellation of their own work and child processes.

For a deterministic contract baseline:

```sh
go run ./cmd/trest contract export --baseline > /tmp/trest-baseline.json
go run ./cmd/trest check --baseline /tmp/trest-baseline.json --json
```

A changed baseline produces `contract_drift` and a nonzero exit. This is a review signal. It does not classify every change as breaking, and it cannot discover a business meaning change whose schema stays the same. Updating a baseline is always explicit.

## Local execution policy and trace limits

Scenario registration and listing are read-only. Execution requires both `Enabled: true` and `Environment: "local"`; the example command uses `--local` for this opt-in. Unconfigured cases do not execute. Cases must return 1-128 application-authored assertions with bounded public-safe names. Unknown errors and panics are reported with stable categories and generic messages, never raw error strings. Scenario deadlines are cooperative, so authored functions must honor their context.

Tracing is optional and metadata-only. `operation.WithObserver` adds synchronous hooks for validation, authorization, handler, preparation, and overall invocation. Observers must be fast and concurrency-safe; observer panics do not change invocation results. The bounded recorder keeps an event window, so old traces can be partial after eviction. Identity and correlation metadata may still be sensitive; the application chooses retention and redaction policy.

CLI trace files use a versioned `trest.traces.v1` envelope, exclusive creation, mode `0600`, and a 4 MiB read/write limit. Trace commands read only the requested regular file; they do not connect to a production trace backend. The local cases use synthetic identities and omit payloads, credentials, and raw errors.

## Local development process

Start the application-owned local fixture explicitly:

```sh
go run ./cmd/trest dev --addr 127.0.0.1:8080
# Opt into source polling and reload:
go run ./cmd/trest dev --watch --poll 500ms --build-timeout 1m
```

The supervisor builds `./cmd/invoices` into a private temporary directory and starts it with the synthetic `local-dev-fixture-token`. It accepts only loopback IP addresses, uses a limited environment, applies a build deadline, and checks `/readyz` against a per-process instance marker. It reports `building`, `ready`, `build_failed`, and `stopping` events. Readiness reflects this in-memory example's ability to serve, not a general dependency probe.

Polling compares Go source and module-file modification metadata. A successful rebuild gracefully replaces the process; a compile failure keeps the previous process running. Reload resets the example's in-memory invoices. Stop sends interrupt, waits for bounded graceful shutdown, then kills if necessary. No watcher, listener, or subprocess is started by importing Trest. Unix builds cancel a timed-out compiler process group; other platforms use the standard command cancellation behavior.

## Comparison evidence

The isolated [adoption experiment](../experiments/adoption/README.md) implements the same three maintenance tasks through plain `net/http`, Trest, and Huma: a scoped invoice-preview endpoint, a rejected invalid request, and concurrent duplicate-effect handling. It includes tests, a repeatable measurement command, and a checked-in measured report.

The measured local workload showed higher per-request latency and allocations for Trest than either comparison. That is evidence for optimization and adoption evaluation, not a performance win. No human or coding-agent task timings, review-effort scores, or external adoption claims were fabricated. A controlled participant study remains a product decision gate rather than something these unit tests can establish.

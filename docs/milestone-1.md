# Milestone 1: prove the core

The source design is [A Go Application Framework for the AI Era, draft 0.1](Go_Framework_Design_Paper.pdf), especially pages 3-5 and 10. The working name is **Trest**. This implementation is an experimental prototype, not a stable release or a claim of superior performance.

## Scope

The milestone includes typed operation definitions, immutable registration, one enforced invoker, domain validation hooks, structured errors, a standard HTTP adapter, a documented schema subset, deterministic OpenAPI export, and both requested examples.

The package split follows the paper. `operation` is independent of HTTP. `httpadapter` owns protocol details. `contract` consumes operation metadata and explicit bindings. The examples construct dependencies manually and own their HTTP lifecycle. Their domain service imports neither Trest nor `net/http`.

## Decisions made for the prototype

| Question | Decision |
| --- | --- |
| Module identity | Use `github.com/mujhtech/trest`, selected on 11 November 2026. |
| Go requirement | Go 1.23 language baseline, standard-library dependencies only. |
| Registration failures | Return useful errors immediately; `Freeze` validates registry configuration and seals it before use. |
| Authorization | Explicit `Public()` or one named permission; an application authorizer receives trusted principal and typed input for resource checks. |
| Identity | Context carries a principal; adapters only establish it through application-owned verified authentication. |
| Resource bounds | Positive default deadline, bounded concurrency, bounded HTTP input/output; cancellation is cooperative. |
| HTTP bindings | One literal route per operation, JSON bodies for POST/PUT/PATCH, empty inputs for GET/DELETE. Expand deliberately after examples reveal needs. |
| Schema support | A narrow structural subset shared by registration, request validation, and export; unsupported shapes fail early. |
| OpenAPI security | Export access metadata as `x-trest-access`; do not invent a bearer or OAuth contract for an application-supplied authenticator. |
| Domain errors | Registration can map handler errors; unclassified public failures are redacted. |
| Lifecycle | Applications start and stop listeners. No hidden setup, automatic database transactions, or background workers. |

## Acceptance coverage

The implementation's test suites exercise the behavior required by the paper:

- Core invocation tests cover startup diagnostics, immutable metadata, validation, denied calls, trusted identity propagation, limits, cancellation, and error handling.
- HTTP tests use `httptest` to exercise malformed payloads, body limits, protected access, cancellation, response limits, and public error redaction.
- Schema and contract tests cover supported and rejected types, structural validation, and deterministic export.
- The invoice example's end-to-end tests create an invoice for the authenticated tenant, reject cross-tenant resource access and forged payload identity, and verify rejected requests leave storage unchanged.
- The existing-handler example verifies that handwritten `/healthz` and typed `/hello` handlers coexist on the same mux. Its contract contains only the explicitly bound operation.

Run the checks from the repository root:

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

Contract export does not require authentication credentials or a running service:

```sh
go run ./cmd/invoices -openapi
go run ./cmd/existing-handler -openapi
```

An OpenAPI diff is a review signal. A field changing meaning without changing type remains an application-level compatibility decision. Business invariants and effects still need domain tests.

## Limits and next milestones

This prototype does not include schema overrides, dynamic path/query/header binding, streaming, automatic client generation, production authentication providers, persistence, idempotency, transactions, jobs, or AI tools. The invoice repository is intentionally in memory. It demonstrates an authorization boundary, not a billing system.

Milestone 2 adds versioned inspect/check output, source mapping, tracing hooks, application-authored scenarios, and an adoption comparison. Milestone 3 introduces persistent PostgreSQL job delivery with fault-injection tests and a recovery guide. Milestone 4 adds explicit AI tool exposure and approvals through existing SDKs. Each remains subject to the design paper's decision gates.

Before publishing, choose the module/repository identity and license, verify the supported Go-version matrix, run external adoption trials, and document any API changes. No publication or deployment is part of milestone 1.

## Verification record

Verified locally on 7 November 2026 with Go 1.27.3 on macOS arm64:

- `go test -race ./...`, `go vet ./...`, and `go build ./...` passed.
- `golangci-lint run ./...` reported zero issues; Go formatting was clean.
- Schema fuzzing completed 120,557 executions without a failure.
- Both built example commands exported OpenAPI without credentials. Repeated invoice exports were byte-for-byte identical; the contract tests also check the committed baseline in `contract/testdata/invoices.openapi.json`.
- Live localhost smoke checks returned 401 without invoice credentials, 201 for an authorized invoice, and 403 for a cross-tenant customer. The existing-handler example returned 200 for both `/healthz` and `/hello`.
- Both example servers exited successfully after an interrupt, exercising graceful shutdown.

The Go 1.23 minimum is the declared language baseline; this run tested the installed Go 1.27.3 toolchain, not a multi-version support matrix.

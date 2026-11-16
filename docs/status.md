# Framework completion record

Updated 16 November 2026; implementation verification below is dated per run. Trest implements the Go web framework scope in [the direction document](elysia-direction.md): a small first app, typed application composition, shared runtime contracts, clients, real-time transports, standard plugins, and a complete local development workflow. The [framework guide](framework.md), [development guide](development.md), and [invoice workshop](../examples/invoices/README.md) use the public application API without registry or adapter assembly.

This record describes completed local functionality and its checks. The module identity is `github.com/mujhtech/trest`, selected on 11 November 2026. Publishing a stable package still requires a license and version selected by the maintainer. No repository was published and no service was deployed by this implementation work.

## Acceptance matrix

| User workflow | Delivered behavior | Executable evidence |
| --- | --- | --- |
| First request and composition | `New`, typed HTTP methods, constants, groups/prefixes, named plugins, ordered prerequisites, scoped auth/guards/middleware | `app_test.go`, `diagnostics_test.go`, `cmd/trest/project_integration_test.go` |
| Typed inputs | Path/query/header/cookie, JSON, URL-encoded forms, multipart uploads, bounded bodies | `internal/webinput/*_test.go`, `app_test.go`, generated upload fixtures |
| Shared models | Recursive models, JSON embedding, enums/ranges/lengths/patterns, explicit JSON/text codecs | `internal/schema/*_test.go`, `internal/webinput/codec_test.go`, client recursion fixtures |
| Reusable policy | Typed resolvers after input validation and before resource authorization; typed result transforms before output validation | `hooks_test.go`, `operation/codec_test.go`; invoice resource policy |
| Response contracts | JSON/text, native files/HTML/redirects/empty responses, typed status/body variants, headers and cookies | `typed_response_test.go`, `response_protocol_test.go`, live client status fixtures |
| Lifecycle | Explicit `Start`/`Runtime.Shutdown`, `RunServer` for custom servers, rollback, admission stop, request cancellation, handler drain before resource cleanup | `lifecycle_test.go`, `lifecycle_integration_test.go`, schedule plugin tests |
| Documentation | Deterministic OpenAPI 3.1, recursive references, offline interactive request explorer, source-bearing construction errors | OpenAPI/contract tests, `internal/docsui/page_test.go`, browser and JavaScript behavior checks |
| Typed clients | Generated standalone Go/TypeScript types, recursive models, form/upload fields, typed response statuses, headers, public errors | Actual compiler acceptance/rejection, runtime fetch tests, checked-in client drift checks |
| Real-time clients | Typed JSON SSE iterators and WebSocket send/receive clients; injected Go socket transport and TypeScript factory | Live SSE/WS generated-client fixtures; native adapter integration suite |
| Real-time servers | Validated event and socket messages, size/queue bounds, synchronous backpressure, cancellation, connection cleanup | `realtime_test.go`, WebSocket real-frame tests, invoice event feed |
| Standard plugins | CORS, signed cookies, static files, scheduling, docs; optional JWT verification and OpenTelemetry instrumentation | Plugin tests; JWT cryptographic/policy tests and demo; OTel in-memory exporter tests |
| Developer workflow | Scaffold, first request, route inspection, project contract checks/baselines, reload, last-good-build recovery, client regeneration, shutdown | Fresh-project integration tests plus project-check fixtures |
| Real application | Tenant-scoped invoice CRUD, filtering/pagination, resource authorization, upload, typed snapshot/live events, docs, clients | `examples/invoices/app_test.go`, live generated Go/TypeScript workshop |

Tests check authorization rejection before a protected handler or stream producer executes. JSON job/tool invocation rejects native HTTP responses, uploads, event streams, and raw handlers. Compatible routes reuse the same validation, resolver, authorization, handler, and output-validation path.

## Architecture

The root module is standard-library-only and declares Go 1.23. Dependencies are constructed explicitly; app construction starts neither goroutines nor listeners. Business services are ordinary Go code. Optional integrations remain separate modules:

| Module | Minimum declared Go | Purpose |
| --- | --- | --- |
| Root | 1.23 | Application, operation enforcement, schemas, contracts, clients, development tools, jobs and AI-tool policy |
| `adapters/websocket` | 1.23 | Native and typed WebSocket transport |
| `adapters/jwt` | 1.23 | Explicit algorithm/key/issuer/audience JWT verification |
| `adapters/mcp` | 1.25 | Official MCP SDK application/development protocol adapters |
| `adapters/postgres` | 1.25 | PostgreSQL driver and durable-job example |
| `adapters/otel` | 1.26 | OpenTelemetry request and operation-phase instrumentation |
| `experiments/adoption` | 1.26 | Isolated Huma/plain-Go comparison; no core runtime dependency |

JWT keys and principal mapping are application supplied. JWT verification is not an identity provider. OpenTelemetry receives an explicit provider and propagator; exporter/storage choices remain with the application. The invoice example uses synthetic local tokens and in-memory data.

Preparation of a job/tool input validates and authorizes without executing its handler; execution rechecks authorization later. Preparation grants no future authority. Resolvers, validators, and authorizers must not perform privileged side effects. Lost results and ambiguous post-handler failures remain uncertain rather than being automatically replayed. Database-local idempotency cannot make remote effects exactly-once.

The original infrastructure remains covered by [milestone 1](milestone-1.md), [milestone 2](milestone-2.md), [milestone 3](milestone-3.md), and [milestone 4](milestone-4.md). Their completion alone was insufficient evidence for the web framework; the acceptance matrix above covers the application experience.

## Verification

Run every module explicitly; root `go test ./...` does not enter nested modules:

```sh
TREST_TEST_WEB=1 TREST_TEST_WEBSOCKET=1 \
  TREST_TSC=/absolute/path/to/typescript/bin/tsc scripts/check-all.sh
```

`TREST_TEST_WEB` enables root integration tests that compile and execute generated clients and exercise fresh-project reload. Node and the TypeScript compiler must be installed. `TREST_TEST_WEBSOCKET` enables actual socket tests. To include database fault tests, configure `TREST_TEST_POSTGRES_DSN` for an isolated disposable `trest_jobs_test` database as described in the milestone 3 guide.

Completed on 10 November 2026 on macOS arm64:

- The integrated all-module script passed race tests, vet, and build across all seven modules with Go 1.27.3. Root integration tests ran with an explicit TypeScript compiler and Node 22: generated Go/TypeScript compile/runtime checks, invalid-call compiler rejection, typed forms/statuses/recursive models/SSE/WebSockets, the invoice workshop, project contracts/baselines, fresh-project reload and broken-build recovery all passed. Real native WebSocket checks also passed. [Full output](verification/all-modules-2026-11-10.txt).
- The complete root, WebSocket adapter, and JWT adapter passed race tests, vet, and build using Go 1.23.12. The OTel adapter was also checked on its Go 1.26 baseline during implementation. [Go 1.23 output](verification/go123-2026-11-10.txt).
- Lint reported zero issues in all seven modules; `gofmt -l .` returned no files. [Lint record](verification/lint-2026-11-10.txt).
- PostgreSQL integration tests passed again after the final authorization fix against a newly created PostgreSQL 18 container bound only to localhost. The container was removed afterward. No production credentials or existing application database were used. [Database output](verification/postgres-2026-11-10.txt).
- A live browser session used the offline docs to create an invoice (201), display a field validation failure (400), and display an authentication failure (401). The temporary server stopped cleanly. All 16 guide Go snippets compiled/constructed, the README program built on Go 1.23.12, and 33 guide links/anchors passed inspection.
- Independent review found and fixed resolver replacement of verified principal/trace metadata, pointer-wrapper construction panics, and recursive OpenAPI component collisions. Regressions exercise the public behavior; authorization rejection remains before handler execution.

The local framework completion goal is satisfied by the acceptance matrix and checks above. These checks do not claim a published stable release or compatibility with every Elysia extension.

## Performance evidence

The [13 November optimization report](benchmarks/framework-optimization-2026-11-13/README.md) contains the latest paired measurements. Against a frozen baseline that already included the 11 November changes, JSON hello fell from 3.568 to 2.544 µs/op (28.70%) and duplicate invoices from 6.600 to 4.7975 µs/op (27.31%). Allocations fell from 32 to 18 and from 48 to 34 respectively. Both timing changes have `p < 0.001`.

Trest still takes 17.64% more time than Gin for hello, while the invoice difference from Gin is inconclusive (`p = 0.701`). Separate current-only measurements with fresh cancelable parents show larger gaps: Trest measured 3.4825/5.802 µs, compared with Gin's 2.3575/4.942 µs. These do not establish a before/after improvement for cancelable requests or production latency. IDs, deadlines, validation, and authorization remained enabled. The report retains raw samples, profiles, source hashes, and verification across all seven modules.

### Historical optimization — 11 November 2026

The [11 November framework optimization report](benchmarks/framework-optimization-2026-11-11/README.md) records that round’s profiles, frozen before/after binaries, raw samples, and verification. Fourteen paired 500 ms samples ran serially on an Apple M2 Pro with Go 1.27.3 and `GOMAXPROCS=1`.

| Main `App` API workload | Before | After | Time change | Allocations before → after |
| --- | ---: | ---: | ---: | ---: |
| JSON hello | 4.273 µs | 3.548 µs | −16.98% | 35 → 32 |
| Duplicate invoice | 8.079 µs | 6.640 µs | −17.81% | 57 → 48 |

Gin measured 2.174 µs for JSON hello and 4.770 µs for duplicate invoices; Echo measured 2.216 µs and 4.757 µs. The gap narrowed, but Trest still takes about 60–63% more time for JSON hello and 39–40% more time for duplicate invoices. Unchanged control implementations showed no statistically significant timing change. These are serial in-process measurements including fresh request/recorder construction, with no network or database I/O; they do not measure concurrent server throughput or production latency.

Validation, authorization, invocation limits/deadlines, and request identity remain enabled. Common business checks reject malformed input and unauthorized requests before handler execution; parser behavior and framework features still differ. After the change, [all modules](benchmarks/framework-optimization-2026-11-11/all-modules.txt) passed race tests, vet, and build, including live generated clients and WebSockets. [Core Go 1.23 race tests](benchmarks/framework-optimization-2026-11-11/go123-race.txt), [lint](benchmarks/framework-optimization-2026-11-11/lint.txt), and 248,579 differential fuzz executions passed. Real PostgreSQL integration tests were not rerun in this pass; the earlier isolated-database verification remains historical evidence.

### Earlier optimization — 10 November 2026

The [10 November optimization report](benchmarks/optimization/README.md) records CPU/allocation profiles, unchanged benchmark fixtures, alternating before/after runs, and verification. Compiled schema/value plans, exact numeric caches, and reduced HTTP/context bookkeeping preserve validation, authorization, deadlines, trace identity, and admission control.

Medians from that earlier run on an Apple M2 Pro with Go 1.27.3:

| Workload | Before | After | Allocations before → after |
| --- | ---: | ---: | ---: |
| Trest authenticated typed POST | 10.699 µs | 4.990 µs | 202 → 50 |
| Direct HTTP with the same Trest invoker | 10.431 µs | 4.562 µs | 195 → 45 |
| Trest typed hello | 4.322 µs | 2.973 µs | 58 → 33 |
| Bare HTTP hello | 1.536 µs | 1.589 µs | 19 → 19 |

The typed POST uses identical business logic, tenant authorization, input/output schema checks, invocation limits/deadlines, request IDs, strict JSON, and response encoding. Thirteen equivalence cases include rejection before handler execution. Its direct baseline is handwritten HTTP glue around the same Trest operation invoker, not a framework-free implementation. The bare hello omits framework checks and metadata. All cases include request/recorder construction and exclude network/database I/O.

The separate invoice experiment improved Trest from 12.799 µs / 227 allocations to 6.024 µs / 56 allocations. Plain HTTP measured 3.696 µs / 40 allocations and Huma measured 6.116 µs / 57 allocations after the change. Trest is roughly level with Huma in this workload but still trails plain HTTP. The target of matching bare `net/http` has not been reached.

Core measurements used 12 alternating pairs of 500 ms samples; invoices used 10 pairs of 300 ms samples. Unchanged control implementations showed no statistically significant timing change. See the report for raw samples, `benchstat`, limitations, and reproduction commands. These 10 November measurements remain historical evidence; the 13 November paired run above is the latest performance result. Different benchmark configurations must not be compared as an optimization delta.

After optimization, [all seven modules](benchmarks/optimization/all-modules.txt) passed race tests, vet, and build, including live clients and WebSockets. Core Go 1.23 checks, isolated PostgreSQL tests, and lint also passed. Differential fuzzing and regressions cover exact numeric/Unicode behavior, custom codecs, duplicates, cancellation, and identity isolation. See the report's verification links for the recorded evidence.

## Deliberate boundaries

- Go models and explicitly declared codecs produce a supported closed schema subset. Arbitrary interfaces, unrestricted JSON Schema unions/combinators, nonstring map keys, and codecs without a matching schema are rejected at construction. Whole JSON bodies support normal embedding; outer request structs with path/query/header/cookie/body/form tags use explicit named fields.
- Clients require regeneration after a contract change. Untyped streams and raw handlers retain native transports. Typed SSE and WebSocket contracts generate typed clients. Browser WebSocket APIs cannot send arbitrary HTTP headers; use application-owned cookie authentication or an explicitly supplied compatible factory. No hidden token query parameter is added.
- Cancellation is cooperative. Shutdown rejects new work, cancels active requests, and keeps resources alive until handlers finish. A timeout limits waiting; it does not make it safe to close a resource still in use. Use `Start`/`Shutdown` or `RunServer` when mounting an app into an existing server.
- Streaming and sockets have bounded messages and queues; there is no implicit durable delivery, replay, or reconnect. Per-message resource authorization belongs in the application. The invoice feed atomically snapshots/subscribes and disconnects slow consumers.
- JavaScript numbers retain their precision limits. Custom business validation cannot be inferred into a client schema. Body defaults are contract annotations rather than automatic mutation.

The design paper's non-goals remain outside the framework: ORM, frontend platform, deployment platform, general durable workflow engine, and autonomous planner. Production load measurements, independent adoption studies, and live model-provider quality/billing evaluation require a selected application and environment. They are not implied by local framework tests.

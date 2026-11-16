# Milestone 3: durable PostgreSQL jobs

Trest's `job` package provides a persistent queue, scoped idempotency, a transactional outbox, bounded retries, lease fencing, administrative history, and explicit recovery. The root module still uses only the standard library. The optional `adapters/postgres` module supplies pgx v5.12.0 and requires Go 1.25 or newer; the core job package supports Go 1.23.

## Run the example and real database tests

Use a disposable PostgreSQL 18 database. The example never applies migrations automatically.

```sh
cd adapters/postgres
export TREST_POSTGRES_DSN='postgres://USER:PASSWORD@127.0.0.1:5432/DISPOSABLE_DB?sslmode=disable'
go run ./cmd/job-example -migrate
go run ./cmd/job-example -invoice invoice-001
go run ./cmd/job-example -invoice invoice-001
```

The first invocation creates application invoice data and an outbox entry in one transaction, dispatches the job, and processes it. Repeating the same invoice ID retrieves the same queue result. The handler protects its database effect and recorded result with `DoEffect`, so a deliberate replay does not repeat the database write. The example identity is synthetic local data; deployed applications must authenticate callers and supply their own authorization policy. Disable `sslmode` only for an isolated local database; configure normal verified TLS for remote deployments.

Integration tests require the database name `trest_jobs_test` and create a random isolated schema per test. They drop only those schemas afterward. They do not use an existing application schema.

```sh
cd adapters/postgres
export TREST_TEST_POSTGRES_DSN='postgres://USER:PASSWORD@127.0.0.1:5432/trest_jobs_test?sslmode=disable'
go test -race -tags=integration ./...
```

The suite exercises concurrent duplicate enqueue, changed-input conflicts, tenant/actor propagation, revoked authorization, trace and correlation propagation, lease expiry and stale completion, retry exhaustion, pending and running cancellation, heartbeat renewal, incompatible payload versions, retention windows, outbox rollback and restart recovery, duplicate delivery, and crashes before/after business commit. A handler that commits an effect and then returns a domain error remains uncertain. The transactional effect test verifies that concurrent duplicates execute the database callback once and that a failed callback rolls back both the effect and its idempotency reservation.

## Construction and trusted invocation

1. Construct and freeze the operation registry.
2. Apply `job/migrations/001_init.sql` explicitly using the application's deployment migration process. `job.MigrationSQL()` exposes the same bytes for test or deployment tooling. Review schema capacity and indexes against production load before deployment.
3. Construct `job.New(db, registry, options)` using an application-owned connection pool.
4. Enqueue a `job.Request` with an operation ID, explicit payload version, caller scope, and business JSON. Idempotency keys are strongly recommended and required for `Publish` and `DoEffect`.
5. Start a bounded number of `Worker.Run(ctx)` calls explicitly. Each worker runs one handler at a time. Stop claiming by cancelling its context, then wait for cooperative handlers to drain before closing the database.

`Enqueue` calls `PrepareJSON`, which validates and authorizes the operation without executing its handler. The principal and tenant come from `operation.WithPrincipal` on trusted application context; they are persisted separately from the input. The worker restores these references, revalidates input, and reauthorizes immediately before `InvokeJSON`. Revocation and resource changes therefore apply at execution. A body field cannot grant identity or permissions. Callers must not let untrusted clients construct the trusted context, caller scope, retry-safety declaration, or administrative operator identity.

`operation.WithTrace` and `WithCorrelationID` propagate across the queue; `Request.CorrelationID` is stored explicitly and the publisher's trace context is restored as the parent of new invocation spans. No credentials, connection strings, input bodies, output bodies, or raw exception text enter `Events`. Queue records themselves contain private input/output and require application access controls and a retention policy.

Envelope version 1 is distinct from the operation ID and payload version. `Options.PayloadVersions` defines accepted versions; unspecified operations accept `"1"`. An incompatible deployment fails old jobs with `unsupported_payload_version` without calling the handler. Keep old operation versions registered while draining them, or explicitly migrate payloads through an application-owned process. Trest never reinterprets an old payload as a new schema automatically.

## State and retry semantics

| State | Meaning |
| --- | --- |
| `pending` | Scheduled and eligible after its availability time. |
| `running` | Claimed with a unique attempt token and a live lease. |
| `succeeded` | A bounded JSON result was durably recorded. |
| `failed` | A known preflight failure, expired unstarted deadline, or exhausted declared-safe retry budget. |
| `cancelled` | Cancelled before execution, or explicitly reconciled by an operator. |
| `uncertain` | Execution may have caused effects but no reliable final outcome is recorded. |

Defaults are a 30-second lease, 1 MiB input/output limits, three attempts, exponential backoff from one second to one minute with equal jitter, and seven-day job/idempotency retention. Configure these before constructing workers. Each operation still applies its own invocation deadline. The job deadline can be earlier; changing the lease does not extend business execution deadlines.

Transient preflight failures (`unavailable` or `rate_limited`) may retry without `RetrySafe`, because the business handler has not run. After invoking a handler, automatic retries require `Request.RetrySafe=true` and a transient error. Set that declaration only when application database idempotency or provider-side protection makes replay safe. Other errors after invocation, panics, cancellation, oversized results, and lost completion acknowledgements are uncertain. A domain error code alone cannot prove that no effect committed.

Expired running leases become uncertain and are never automatically replayed. `Recover` and `Claim` perform bounded recovery. The attempt token and live-lease checks fence queue completion and heartbeat updates. They cannot prevent an old process from writing to an unrelated database or remote provider. A noncooperative handler may outlive cancellation; the application must impose a process shutdown limit and reconcile its effects.

## Idempotency and transactions

Queue keys are unique by `(tenant, operation ID, caller scope, idempotency key)`. Canonical input plus payload version is hashed. A duplicate with the same input and actor returns the existing job ID, current state, or saved result; changed input or actor returns `ErrConflict`. The key is not released when a job fails. Queue deduplication is distinct from effect idempotency.

`DoEffect` explicitly starts a database transaction, reserves the same scoped key in a separate effect ledger, runs the application's callback using that transaction, records its result, and commits both. Concurrent duplicates wait for the original transaction and return its result. Callback errors roll back both the database work and reservation. A caller that loses the commit acknowledgement must retry the same key and input; it must not invent a replacement key. This helper protects only writes through the supplied transaction in the same database. Do not treat it as exactly-once execution for remote API calls.

`Publish(ctx, tx, request)` writes an outbox record through an existing application transaction. Commit it with the business state change. The dispatcher transfers the outbox record and queue insertion in one PostgreSQL transaction; duplicate delivery resolves through the queue key. A key/input conflict marks that outbox row failed instead of blocking later rows. `Outbox` exposes its status without returning payloads. Corrupt or unsupported outbox envelopes require operator intervention; their bytes are retained rather than silently dropped. Cross-database atomicity and durable workflow step replay are outside this adapter.

## Recovery and operator actions

All administrative methods require application-owned authorization. Trest does not expose these through unauthenticated HTTP endpoints.

- Use `List(ctx, job.Uncertain, limit)` or `List(ctx, job.Failed, limit)` and `Events` to inspect bounded history. Do not log `Job.Envelope.Request.Input` or `Job.Output` by default.
- For an uncertain job, inspect the application's database or the provider's idempotency/result record. Keep the original job and operation identity while reconciling.
- If success is confirmed, call `Resolve` with the approved result, operator identity, and a safe reason. `Resolve` records evidence; it never executes the handler.
- If no effect is confirmed and replay is appropriate, call `Retry` with operator identity, reason, a bounded additional-attempt allowance, and a new deadline when the old deadline has passed. Retry preserves the original input, actor, operation version, job ID, and full history. Authorization is checked again when execution resumes.
- `Cancel` immediately cancels pending jobs and requests cancellation of running jobs. Running cancellation may remain uncertain; it does not roll back previously committed effects.

`Cleanup` removes only succeeded/failed/cancelled jobs after both terminal retention and idempotency windows expire; uncertain jobs are retained until reconciliation. Event history is deleted with its job. `CleanupOutbox` removes old dispatched rows, leaving failed rows visible. `CleanupEffects` removes expired completed effect keys. Once a key is deleted, replay can execute again: set every retention window longer than the maximum supported retry, manual recovery, and provider deduplication window. Cleanup is explicit and bounded, never an import-time or hidden background task.

## Guarantee boundaries

The adapter restarts the whole operation on retry. It does not replay Go stacks or provide a general durable workflow engine. A queue completion write and a remote effect are separate transactions. Lease tokens protect queue ownership, and idempotency protects only the scope in which it is implemented. Preserve uncertainty and use explicit reconciliation when those boundaries cannot be committed atomically.

Implementation uses PostgreSQL's documented row locking and `SKIP LOCKED` behavior ([PostgreSQL SELECT reference](https://www.postgresql.org/docs/current/sql-select.html)) and pgx's `database/sql` bridge ([pgx stdlib reference](https://pkg.go.dev/github.com/jackc/pgx/v5/stdlib)). These explain the primitives; the guarantees above are enforced by Trest's tests and explicit application choices.

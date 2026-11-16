# Reproducible adoption experiment

This optional nested Go module compares common application behavior through plain `net/http`, Trest's main `App` API, Trest's HTTP adapter, [Huma's official `net/http` adapter](https://pkg.go.dev/github.com/danielgtaylor/huma/v2/adapters/humago), Gin, Echo, and Chi. Framework dependencies are isolated here; the Trest core retains its Go 1.23 baseline and standard-library-only dependency graph.

| Benchmark name | Implementation | Pinned version |
| --- | --- | --- |
| `net_http` | Standard-library `http.ServeMux` | Running Go toolchain |
| `trest_app` | `trest.New` with typed routes | Local checkout |
| `trest` | Operation registry + HTTP adapter | Local checkout |
| `huma` | Huma + `humago` | v2.40.0 |
| `gin` | Gin | v1.12.0 |
| `echo` | Echo | v5.4.4 |
| `chi` | Chi | v5.5.0 |

The experiment requires Go 1.26. See the [latest framework optimization report — 13 November](../../docs/benchmarks/framework-optimization-2026-11-13/README.md) for paired results, profiles, raw samples, and provenance. Trest's target of beating both Gin and Echo remains unmet. The [11 November optimization](../../docs/benchmarks/framework-optimization-2026-11-11/README.md) and [earlier framework comparison](../../docs/benchmarks/frameworks-2026-11-11/README.md) remain available as historical evidence.

## What is actually compared

Each implementation shares the same domain rules and in-memory idempotency store. Credentials map to synthetic server-owned tenants. The common test workload covers:

1. Adding `/invoices/preview`, with an allowed owner request and a denied cross-tenant request.
2. Diagnosing an invalid amount: all return `400` with no stored effect.
3. Sixteen concurrent identical invoice requests: exactly one stored invoice and the same result for every caller. A changed input under the same scoped key returns `409`.

The same application policy is checked before entering the domain function and again inside it. Trest uses operation validation and authorization hooks; the other fixtures use explicit application calls. Tests instrument domain entry to prove malformed input, invalid values, and denied authorization never reach it. The observer is nil in timed fixtures. Each correctness subtest has its own state, and concurrent duplicate requests verify all invoice fields and one stored effect.

Gin, Echo, and Chi use a shared, strict standard-library JSON decoder for the invoice workload, with a 1 MiB body limit, unknown-field rejection, and trailing-value rejection. They retain their native routers; Gin and Echo use their JSON response APIs, while Chi uses the standard-library JSON writer. This measures these frameworks with explicit application binding, not their default binders. Huma and Trest retain their native typed binding and schema processing. Huma's optional schema-link response transform is disabled so it does not add a `$schema` field to the shared response. Trest's default deadline, capacity, request identity, and structural checks remain enabled. Gin uses `gin.New` in release mode; no optional request logger is installed for any framework.

Exact parser edge behavior, error envelopes, headers, and framework feature sets still differ. The suite establishes common business behavior, not universal framework equivalence.

The store is an in-memory, mutex-protected teaching fixture. It scopes keys by tenant, operation version, and synthetic caller; it does not claim crash durability, cross-process atomicity, or exactly-once external effects.

## Run it

```sh
cd experiments/adoption
GIN_MODE=release go test -race ./...
GIN_MODE=release go test -run '^$' -bench 'Benchmark(HelloJSON|DuplicateInvoice)' -benchmem -count 10
# Install benchstat separately if it is not already available.
python3 benchmark.py /tmp/trest-framework-results --rounds 14 --benchtime 500ms --gomaxprocs 1
```

`benchmark.py` requires Python 3, Go, and `benchstat` on `PATH`. It builds one test binary and runs every sample serially in its own process. Fourteen rounds rotate each framework through every position twice, reverse order for the second cycle, and alternate workload order. It sets `GIN_MODE=release` and an explicit `GOMAXPROCS`, retains raw samples, and saves medians, benchstat output, dependency versions, source hashes, binary hash, and execution order. Choose a new output directory each time; existing results are never overwritten. Avoid other CPU-heavy work during measurement.

`BenchmarkHelloJSON` serves `GET /hello` and serializes the same `{"message":"hello"}` value on each request. `BenchmarkDuplicateInvoice` warms `POST /invoices` before timing, then repeatedly submits the same scoped idempotency key. Both include fresh `httptest` request and response-recorder creation. They exclude network traffic, TLS, databases, application startup, and contract export. They measure serial in-process cost, not concurrent server throughput or production latency.

Dependencies are isolated by this directory's `go.mod` and `go.sum`. Use `go list -deps -f '{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{end}}' ./cmd/measure` to inspect the compiled module graph. The older `GIN_MODE=release go run ./cmd/measure > measured-new.json` utility remains available for exploratory startup and latency sampling; use the rotating benchmark runner for framework comparisons.

Current benchmarks use `b.Loop`, prewarm the duplicate invoice, and disable Huma's schema-link transform. The November 11 and 13 runs fix `GOMAXPROCS=1`; older runs below used 12. Do not calculate performance changes by comparing these different configurations. Historical files remain unchanged.

## Latest optimization — 13 November 2026

**The target of beating both Gin and Echo remains unmet.** The [new report](../../docs/benchmarks/framework-optimization-2026-11-13/README.md) measures additional optimization against the already-optimized 11 November baseline, using fourteen paired 500 ms samples, frozen binaries, and unchanged original fixtures on an Apple M2 Pro, macOS arm64, Go 1.27.3, `GOMAXPROCS=1`.

| Main `App` API workload | Before | After | Time change | Allocations before → after |
| --- | ---: | ---: | ---: | ---: |
| JSON hello | 3.568 µs | 2.544 µs | −28.70% | 32 → 18 |
| Duplicate invoice | 6.600 µs | 4.798 µs | −27.31% | 48 → 34 |

Both improvements have `p < 0.001`; unchanged Gin/Echo controls show no significant timing change. Current Gin medians are 2.163/4.766 µs and Echo medians are 2.201/4.848 µs. Trest hello still takes 17.64% more time than Gin; invoice versus Gin is statistically inconclusive (`p = 0.701`), not a win. IDs, deadlines, capacity, validation, and authorization remain enabled. See [paired analysis](../../docs/benchmarks/framework-optimization-2026-11-13/benchstat.txt) and [current comparisons](../../docs/benchmarks/framework-optimization-2026-11-13/gin-echo-comparison.txt).

Supplemental current-only benchmarks give every request a fresh cancelable parent. Trest records 3.483/5.802 µs, Gin 2.357/4.942 µs, and Echo 2.405/5.014 µs. These expose a larger remaining gap and do not replace the original workload or establish production latency. See the [cancelable-parent analysis](../../docs/benchmarks/framework-optimization-2026-11-13/cancelable/benchstat.txt).

All seven modules passed race tests, vet, and builds with generated-client and WebSocket checks enabled. Core Go 1.23 compatibility checks and lint passed; differential fuzzing completed 1,743,257 executions. The report includes source hashes, raw samples, profiles, rejected diagnostics, and material tradeoffs. Measurements come from a shared host; compare the paired samples, not isolated numbers from different runs.

## Historical optimization — 11 November 2026

The [framework optimization report](../../docs/benchmarks/framework-optimization-2026-11-11/README.md) compares frozen before/after binaries with fourteen paired 500 ms samples on an Apple M2 Pro, macOS arm64, Go 1.27.3, and `GOMAXPROCS=1`. The benchmark fixtures are unchanged between the pair.

| Main `App` API workload | Before | After | Time change | Allocations before → after |
| --- | ---: | ---: | ---: | ---: |
| JSON hello | 4.273 µs | 3.548 µs | −16.98% | 35 → 32 |
| Duplicate invoice | 8.079 µs | 6.640 µs | −17.81% | 57 → 48 |

Gin's corresponding medians were 2.174 µs and 4.770 µs; Echo's were 2.216 µs and 4.757 µs. Trest's gap narrowed, but its main `App` API still takes about 60–63% more time for JSON hello and 39–40% more time for duplicate invoices. Unchanged control implementations showed no statistically significant timing change. Validation, authorization, invocation limits/deadlines, and request identity remain enabled. These serial in-process measurements include request/recorder construction and exclude network/database I/O; they do not establish concurrent throughput or production latency.

All-module race tests, vet, and build passed, including live generated clients and WebSockets. Core Go 1.23 race tests, lint, and 248,579 differential fuzz executions also passed. Real PostgreSQL integration tests were not rerun in this pass. See the report for verification records, [before samples](../../docs/benchmarks/framework-optimization-2026-11-11/before.txt), [after samples](../../docs/benchmarks/framework-optimization-2026-11-11/after.txt), and [benchstat](../../docs/benchmarks/framework-optimization-2026-11-11/benchstat.txt).

## Optimization rerun — 10 November 2026

The [profile-guided optimization report](../../docs/benchmarks/optimization/README.md) compares frozen before/after binaries using this unchanged benchmark. Ten paired 300 ms samples alternated execution order; no other task builds or tests ran during measurement. The host was an Apple M2 Pro, macOS arm64, Go 1.27.3, `GOMAXPROCS=12`.

| Duplicate invoice request | Before | After | Allocations before → after |
| --- | ---: | ---: | ---: |
| Plain `net/http` | 3.723 µs | 3.696 µs | 40 → 40 |
| Trest | 12.799 µs | 6.024 µs | 227 → 56 |
| Huma v2.40.0 | 5.975 µs | 6.116 µs | 57 → 57 |

Trest's time fell 52.94% and allocations fell 75.33%. Plain HTTP and Huma control timings had no statistically significant change. Trest is approximately level with Huma here and still takes about 1.63× plain HTTP's time. The common business policies remain checked; parser and framework feature differences still prevent a universal parity claim.

[Before samples](../../docs/benchmarks/optimization/adoption-before.txt), [after samples](../../docs/benchmarks/optimization/adoption-after.txt), [benchstat](../../docs/benchmarks/optimization/adoption-benchstat.txt), and [verification](../../docs/benchmarks/optimization/README.md#verification) are retained. Use this paired rerun to assess the optimization. The older measurement below used a separate latency-sampling program and remains historical evidence.

## Historical measurement — 8 November 2026

[measured.json](measured.json) was produced on 8 November 2026 with Go 1.27.3, macOS/arm64, `GOMAXPROCS=12`. It contains the exact machine-readable observations and dependency versions. Each stack received 100 warmup requests, 3,000 serial latency samples, 30 construction samples, and Go benchmark allocation measurement.

| Observed measure | Plain `net/http` | Trest | Huma |
| --- | ---: | ---: | ---: |
| Latency p50 | 4.750 µs | 12.750 µs | 4.500 µs |
| Latency p95 | 9.000 µs | 17.959 µs | 7.959 µs |
| Sample throughput | 165,696/s | 69,336/s | 185,258/s |
| Benchmark allocations/request | 39 | 217 | 56 |
| Benchmark bytes/request | 7,913 | 15,144 | 8,468 |
| Construction p50 | 1.792 µs | 17.292 µs | 65.584 µs |
| Contract export p50 | Not implemented | 73.000 µs | 123.292 µs |

Trest was slower and allocated more for this request workload. The data supports further profiling before setting a performance budget. It does not establish that any framework is generally faster or that developer productivity compensates for overhead.

The timed request is `POST /invoices` with the **same idempotency key**, so after warmup it follows the duplicate-result path. Measurements include `httptest` request construction, JSON binding/validation, synthetic authentication, application checks, idempotency lookup, and response recording. There is no socket, TLS, real database, external service, tracing exporter, or body capture. Trest's default invocation deadline, capacity check, request ID, and structural validation remain enabled. All operations are serial in the measurement; the separate correctness suite exercises concurrent duplicates. The plain implementation does not export OpenAPI. Huma and Trest contract outputs have different size and content, so export timings are not an equal-schema benchmark.

One machine/run, microsecond timers, scheduler/GC activity, fixed execution order, and shared-machine load limit the statistical conclusions. The benchmark's amortized ns/op can differ from the sampled latency distribution because they are separate phases. Re-run multiple times on a controlled machine before making optimization or deployment decisions.

## Human and agent adoption study

No human or coding-agent maintenance study was conducted. The tests establish behavior and repeatable tasks; they do not measure elapsed development time, review effort, incorrect edits, or subjective comprehension.

For a later controlled study, use the three common tasks above, the same acceptance tests, fixed task descriptions, identical model/prompt/tool conditions for agent runs, and independent participants or counterbalanced order to reduce learning effects. Record task start/stop, failed test count, policy omissions found in review, review minutes, changed lines, and dependency changes. Preserve actual observations with the participant's consent. An empty metric must remain unmeasured, not zero. Do not extrapolate the current runtime microbenchmark into an adoption claim.

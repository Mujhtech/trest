# Reproducible adoption experiment

This optional nested Go module compares equal application behavior through plain `net/http`, Trest, and [Huma's official `net/http` adapter](https://pkg.go.dev/github.com/danielgtaylor/huma/v2/adapters/humago). It pins Huma **v2.40.0**. Huma requires Go 1.26 in this version; the Trest core retains its Go 1.23 baseline and has no Huma dependency.

## What is actually compared

Each implementation shares the same domain rules and in-memory idempotency store. Credentials map to synthetic server-owned tenants. The common test workload covers:

1. Adding `/invoices/preview`, with an allowed owner request and a denied cross-tenant request.
2. Diagnosing an invalid amount: all return `400` with no stored effect.
3. Sixteen concurrent identical invoice requests: exactly one stored invoice and the same result for every caller. A changed input under the same scoped key returns `409`.

The same application policy is checked at the adapter boundary and again inside the domain function. Trest uses operation validation and authorization hooks; Huma and plain HTTP use explicit application calls. Huma has broader schema and transport support than Trest's supported schema subset. Exact error envelopes, parser edge behavior, default headers, and framework feature sets differ. The suite demonstrates the common business tasks, not universal behavioral equivalence.

The store is an in-memory, mutex-protected teaching fixture. It scopes keys by tenant, operation version, and synthetic caller; it does not claim crash durability, cross-process atomicity, or exactly-once external effects.

## Run it

```sh
cd experiments/adoption
go test -race ./...
go test -run '^$' -bench BenchmarkDuplicateInvoice -benchmem -count 3
go run ./cmd/measure > measured-new.json
```

Dependencies are isolated by this directory's `go.mod` and `go.sum`. The compiled measurement binary imports one third-party module, Huma; its test dependencies recorded in `go.sum` are separate from the Trest core. Use `go list -deps -f '{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{end}}' ./cmd/measure` to inspect the compiled module graph.

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

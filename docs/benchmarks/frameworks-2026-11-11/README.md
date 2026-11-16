# Go framework comparison — 11 November 2026

Trest is slower than the other tested frameworks on both workloads in this run. Its main `App` API takes **7.8495 µs** for the duplicate invoice request: **9.43% more time than Huma**, **59–67% more than Chi/Gin/Echo**, and **66.71% more than plain `net/http`**. The small JSON endpoint takes **4.245 µs**, approximately twice the plain HTTP baseline. These are local serial request costs, not network latency or server throughput.

## Results

Medians of 14 samples per case, each timed for at least 500 ms. Lower values are better. Bytes and allocations include test request/response construction as well as framework and application work.

| Implementation | JSON GET, µs/op | B/op | Allocs/op | Duplicate invoice POST, µs/op | B/op | Allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Plain `net/http` | 2.1610 | 6,152 | 20 | 4.7085 | 7,905 | 40 |
| Gin v1.12.0 | 2.1610 | 6,152 | 20 | 4.7125 | 7,905 | 40 |
| Echo v5.4.4 | 2.1880 | 6,128 | 19 | 4.7815 | 7,825 | 39 |
| Chi v5.5.0 | 2.2985 | 6,521 | 22 | 4.9305 | 8,273 | 42 |
| Huma v2.40.0 (`humago`) | 2.4575 | 6,329 | 24 | 7.1730 | 8,314 | 55 |
| Trest HTTP adapter (`trest`) | 4.3165 | 7,592 | 36 | 7.6730 | 9,201 | 56 |
| Trest main API (`trest_app`) | 4.2450 | 7,096 | 35 | 7.8495 | 9,281 | 57 |

The [benchstat summary](benchstat.txt) shows rounded 95% confidence-interval ranges of 0–3% for timings. A displayed ±0% is rounding, not zero uncertainty. [Comparisons against plain HTTP](comparison.txt) show no detected timing difference between Gin and the baseline in either workload, or Echo and the baseline for JSON GET. Trest's differences from plain HTTP and [Huma](huma-vs-trest.txt) are statistically significant in these samples (`p < 0.001`). The results do not establish a universal framework ranking. In particular, small differences between the simpler stacks should not drive conclusions about production performance.

## Workloads and configuration

The fixtures and benchmarks are in [experiments/adoption](../../../experiments/adoption/README.md).

- **JSON GET:** `GET /hello`, serializing `{"message":"hello"}` from a typed Go value on every request. One static route, no authentication, no optional middleware, no pre-encoded response. The tests require status 200, JSON content type, and exactly the expected field.
- **Invoice POST:** `POST /invoices` with a 102-byte JSON body, synthetic bearer authentication, field validation, tenant authorization, a shared domain recheck, SHA-256 input fingerprint, mutex-protected idempotency lookup, and JSON serialization. Each fixture is warmed with one successful creation before timing. Every timed request uses the same scoped key and returns the stored invoice with status 201; this does not measure fresh inserts.

Both benchmarks allocate a fresh `httptest` request and response recorder for every iteration. Setup and warmup are outside `b.Loop` timing. All implementations use the same request body, credentials, business function, in-memory store, and response fields. Error-path tests run separately from timing.

Gin, Echo, and Chi register routes directly, with no second router underneath. For the invoice request they use shared explicit `encoding/json` binding, a 1 MiB body bound, unknown-field rejection, and a trailing-value check. Gin and Echo still use their own JSON response APIs. Thus these numbers describe the frameworks with this explicit binding policy, not their default binders. Plain HTTP implements the same policy directly. Trest and Huma use native typed binding and schema processing. Parser edge semantics and error envelopes are not universally equal.

Trest retains default invocation deadlines, capacity checks, request identity, structural validation, and recovery. Both Trest APIs remain separate cases because users can choose either. Huma's optional schema-link transform is disabled: it otherwise injects `$schema` into the response and adds a `Link` header, despite its schema-serving endpoint being disabled. Other default headers and trailing JSON whitespace can differ. No request logging, real credentials, database, external service, tracing exporter, socket, TLS, or load generator is involved.

Pinned framework versions were checked against their official releases: [Gin v1.12.0](https://github.com/gin-gonic/gin/releases/tag/v1.12.0), [Echo v5.4.4](https://github.com/labstack/echo/releases/tag/v5.4.4), and [Chi v5.5.0](https://github.com/go-chi/chi/releases/tag/v5.5.0). Huma remains at the existing v2.40.0 pin. See [go.mod](../../../experiments/adoption/go.mod) and [go.sum](../../../experiments/adoption/go.sum).

## Measurement method and provenance

- Apple M2 Pro, macOS 27.2.1, arm64, Go 1.27.3, `GOMAXPROCS=1`, `GIN_MODE=release`.
- One compiled non-race test binary; one process per sample. Samples run serially. Fourteen rounds rotate all seven frameworks through every position twice, reverse the second cycle, and alternate workload order.
- 196 measured samples: 14 per workload/framework pair. No task-owned builds, tests, or other benchmarks ran concurrently with the timed samples.
- This is a shared development machine without CPU affinity or controlled system-wide background load. Repeated samples and balanced ordering reduce, but do not eliminate, host noise.
- Trest source is based on commit `73aafaf`, plus the uncommitted experiment additions. The core is unchanged. [run.json](run.json) records the exact source hashes, binary hash, framework/transitive module versions, environment, execution order, and timestamps. Source hashes were checked after measurement.

Raw evidence: [samples.txt](samples.txt), [summary.json](summary.json), [benchstat.txt](benchstat.txt), [comparison.txt](comparison.txt), and [Huma versus Trest](huma-vs-trest.txt).

The older November 8 and 10 results used different fixtures, `GOMAXPROCS=12`, `b.N` loops, and Huma's default schema transform. November 10's standalone invoice benchmark included an initial write; November 8's measurement program performed 100 warmup requests before sampling. This run's JSON response also differs from the earlier core string hello benchmark. Do not use cross-run deltas as evidence of a regression or improvement. Historical files have been preserved.

## Reproduce

From the repository root, with Go, Python 3, and `benchstat` available:

```sh
GIN_MODE=release scripts/check-all.sh
python3 experiments/adoption/benchmark.py /tmp/trest-framework-results \
  --rounds 14 --benchtime 500ms --gomaxprocs 1
benchstat -row .name -col '/framework@(huma trest_app)' \
  /tmp/trest-framework-results/samples.txt
```

Choose a new output directory; the runner refuses to overwrite existing evidence. Run on an otherwise idle machine. The runner supports different durations and scheduler settings, but compare only samples collected under the same configuration.

## Verification

`scripts/check-all.sh` passed race tests, vet, and builds across all seven modules; [output](checks.txt) is retained. The expanded correctness suite checks complete successful responses, independent fixture state, concurrent duplicates, conflicting retries, and rejection before entering the domain function. Invalid cases cover malformed JSON, zero/negative/excessive amounts, unsupported currency, missing customer/key, missing/invalid credentials, and cross-tenant access. All seven hello fixtures pass exact response-field checks. `go mod verify`, formatting, and `git diff --check` also passed.

Optional real PostgreSQL, WebSocket, and generated-client integration suites were not enabled for this experiment-only change. The core module remains standard-library-only. This report supports further profiling of Trest's request path; it does not support a speed advantage over the measured alternatives.

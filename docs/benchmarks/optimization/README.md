# Profile-guided optimization — 10 November 2026

Trest's authenticated POST takes **53.4% less time and 75.2% fewer allocations** after this pass. The invoice experiment improves by 52.9%, reaching approximately Huma's measured time. **The target of matching plain `net/http` has not been reached.** Validation and authorization remain enforced.

## Final measurements

These are `benchstat` medians from alternating frozen before/after binaries on an Apple M2 Pro, macOS arm64, Go 1.27.3, `GOMAXPROCS=12`. Both versions ran the same benchmark source. Requests and recorders are constructed inside the timed loop; there is no network or database I/O.

| Core workload | Before | After | Time reduction | Allocations before → after |
| --- | ---: | ---: | ---: | ---: |
| Trest authenticated typed POST | 10.699 µs | 4.990 µs | 53.36% | 202 → 50 |
| Handwritten HTTP with the shared Trest invoker | 10.431 µs | 4.562 µs | 56.27% | 195 → 45 |
| Trest typed hello | 4.322 µs | 2.973 µs | 31.21% | 58 → 33 |
| Bare `net/http` hello control | 1.536 µs | 1.589 µs | No significant change | 19 → 19 |

Typed POST allocation volume fell from 13.504 KiB to 8.623 KiB per request (36.15%). Hello fell from 8.338 KiB to 6.887 KiB (17.40%). The Trest timing improvements have `p < 0.001`; the unchanged hello control has `p = 0.156`.

The POST baseline calls the **same Trest operation invoker**. It shares business validation, tenant authorization, schema checks, capacity, deadlines, trace metadata, limits, and panic recovery. It measures the additional HTTP glue; it is not a framework-free implementation. Thirteen equivalence tests include authorization rejection before the handler, duplicate/unknown fields, invalid output, body limits, and panic recovery. Trest's measured additional HTTP time is about 0.43 µs here.

The bare hello handler writes one constant string. It omits routing, input/output validation, request IDs, invocation tracing, admission, and deadlines. It measures the cost of those framework facilities, not equal-policy throughput. Trest remains about 1.87× its time in this workload.

The separate [invoice experiment](../../../experiments/adoption/README.md) has a framework-free HTTP implementation and Huma v2.40.0. All three implement the same tested business tasks, but their full parser, error, and framework contracts differ.

| Duplicate invoice workload | Before | After | Allocations before → after |
| --- | ---: | ---: | ---: |
| Plain `net/http` | 3.723 µs | 3.696 µs | 40 → 40 |
| Trest | 12.799 µs | 6.024 µs | 227 → 56 |
| Huma | 5.975 µs | 6.116 µs | 57 → 57 |

Trest's invoice allocation volume fell from 14.359 KiB to 9.020 KiB (37.19%). Plain HTTP and Huma controls had no significant before/after timing change (`p = 1.000` and `p = 0.247`). Trest is approximately level with Huma on this workload and remains about 1.63× plain HTTP's time. These results do not establish general framework parity or production capacity.

## What the profiles changed

The original POST allocation profile attributed large costs to exact-number construction, `encoding/json.Decoder.Token`, tree construction in `schema.readValue`, and repeated JSON Pointer paths. The changes address those costs:

- Compile numeric bounds, enum values, and field paths once. Common signed/unsigned integers use exact magnitude comparisons; larger or fractional numbers retain bounded rational arithmetic. Typed integer checks no longer format and reparse decimal text.
- Compile typed-value validation plans for ordinary closed structs and primitive fields. Custom codecs, pointers, maps, arrays/slices, floats, embedded structs, and recursive shapes retain encoded validation. Business validation still runs after structural checks.
- Compile a wire plan for scalar roots and flat objects with at most 64 scalar properties. `json.Valid` checks the complete grammar. Unknown or duplicate fields, missing required fields, nested values, malformed JSON, and unsupported schemas use the original decoder. Escaped names and invalid UTF-8 retain `encoding/json` normalization. Exact violation ordering and messages remain unchanged.
- Avoid phase clocks and completion-event construction when no observer/hook consumes them. Request IDs, invocation spans, deadlines, capacity, and cancellation checks still run.
- Store immutable principal, trace, and correlation metadata in private context wrappers; combine HTTP state, its context wrapper, and response writer. Preserve parent cancellation, causes, metadata shadowing, and resolver identity isolation. No request contexts or identity objects are pooled.
- Preserve native string writes; skip empty query parsing and probing only for the canonical `http.NoBody`. Arbitrary readers remain bounded and checked, and HEAD/output-byte-limit behavior is unchanged.

The final allocation profile is dominated more by request/recorder construction, headers, decoding/encoding, retained request contexts, and cooperative deadline timers. The default protection and transport behavior remain intact. The fast plans shift work and storage to registration; construction time and plan memory were not separately benchmarked in this pass. Complex models and rejection paths still use the full decoder and should be measured against an application's own traffic mix.

## Verification

- [All seven modules](all-modules.txt): race tests, vet, and build through `scripts/check-all.sh`, including generated Go/TypeScript clients, reload, HTTP/SSE, and native WebSockets.
- Go 1.23.12 core compatibility: [race tests](go123-race.txt), [vet](go123-vet.txt), and [build](go123-build.txt).
- [PostgreSQL integration](postgres.txt): a new disposable PostgreSQL 18 container on localhost, removed after the tests. No existing database or production credentials were used.
- [Lint](lint.txt) reports zero issues; [format check](format.txt) is empty.
- Wire differential fuzzing completed 206,651 executions over flat objects and seven scalar schema families, matching the full decoder's exact violations and error text. Tests cover numeric limits, duplicate and escaped keys, Unicode/surrogates, nullable values, formats, nesting, concurrency, and the 64-property boundary.
- [Typed-value differential fuzzing](typed-fuzz.txt) compares against encoded validation. Boundary tests include signed minima, unsigned maxima, numeric enum equivalence, omitted fields, codecs, and named primitives.
- [Custom decoder regression](codec-regression.txt), on Go 1.27.3 and 1.23.12: valid wire input that decodes into an invalid typed value is rejected before authorization and handler execution. Resolver forgery, capacity release, deadline/cancellation, trace, and protocol suites also pass.

The core remains standard-library-only with its Go 1.23 baseline. Two independent reviews found no blocking regression. No validation or authorization checks were disabled to obtain these measurements.

## Evidence and reproduction

Core measurement used **12 paired samples, 500 ms each**; invoices used **10 paired samples, 300 ms each**. Each pair alternated its execution order: before/after, then after/before. Binaries were built before timing. No other task test, build, fuzz, or profiling job ran during the timed samples. The plain HTTP and Huma implementations served as unchanged controls. Host timing varied between intermediate runs, which is why the tables use the final paired run rather than the fastest observed samples.

- Core: [before](core-before.txt), [after](core-after.txt), [benchstat](core-benchstat.txt).
- Invoice: [before](adoption-before.txt), [after](adoption-after.txt), [benchstat](adoption-benchstat.txt).
- [Run order, binary hashes, and unchanged benchmark-fixture hashes](run.json); [changed Go source hashes](source-hashes.json).
- CPU and allocation profiles plus readable summaries are in [profiles](profiles/README.md). Intermediate `before.txt`, `numeric.txt`, `typed-values.txt`, and `http.txt` document the optimization sequence; their noninterleaved timings are not the final comparison.

Build each source revision into a separate binary before measuring:

```sh
go test -c -o /tmp/trest-after.test .
GOMAXPROCS=12 /tmp/trest-after.test -test.run '^$' \
  -test.bench '^(BenchmarkHello|BenchmarkTypedPOST)$' \
  -test.benchmem -test.count=1 -test.benchtime=500ms

cd experiments/adoption
go test -c -o /tmp/adoption-after.test .
GOMAXPROCS=12 /tmp/adoption-after.test -test.run '^$' \
  -test.bench '^BenchmarkDuplicateInvoice$' \
  -test.benchmem -test.count=1 -test.benchtime=300ms
```

Run the corresponding before binary serially in alternating order for the sample counts above, append each version's output to its own file, and compare with `benchstat before.txt after.txt`. The local source snapshot and original binaries used here were retained under `/tmp/trest-perf-before`; this workspace has no Git revision identifier. The raw results and hashes above preserve the evidence if that temporary snapshot is removed.

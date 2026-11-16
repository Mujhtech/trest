# Trest request-path optimization — 11 November 2026

Trest's main API takes **16.98% less time for JSON GET** and **17.81% less time for the duplicate invoice POST** after this change. Allocations fall from **35 to 32** and **57 to 48**, respectively. Validation and authorization remain enforced. The gap with Gin and Echo is smaller, but parity has **not** been reached: Trest still takes about **60–63% more time for JSON GET** and **39–40% more for invoices** in this serial, in-process benchmark.

## Before and after

Medians of 14 samples per case, each timed for at least 500 ms. Both binaries use identical benchmark fixtures and dependency versions. Lower values are better.

| Trest API / workload | Before, µs/op | After, µs/op | Time change | Allocations before → after | Bytes before → after |
| --- | ---: | ---: | ---: | ---: | ---: |
| Main App / JSON GET | 4.2730 | 3.5475 | −16.98% | 35 → 32 | 7,096 → 7,032 |
| Main App / duplicate invoice | 8.0785 | 6.6400 | −17.81% | 57 → 48 | 9,281 → 8,958 |
| HTTP adapter / JSON GET | 4.3050 | 3.6290 | −15.70% | 36 → 34 | 7,592 → 7,560 |
| HTTP adapter / duplicate invoice | 7.7760 | 6.6505 | −14.47% | 56 → 50 | 9,201 → 9,022 |

All four Trest timing changes are statistically significant in these samples (`p < 0.001`). None of the ten timing comparisons for unchanged controls (`net/http`, Gin, Echo, Chi, Huma across both workloads) detects a significant change. See [benchstat](benchstat.txt), [before samples](before.txt), [after samples](after.txt), and [exact medians](summary.json). This comparison uses the frozen pre-change binary from this run; it does not subtract results collected on different days or with different fixtures.

## Current comparison

| Implementation | JSON GET, µs/op | B/op | Allocs/op | Duplicate invoice, µs/op | B/op | Allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Plain `net/http` | 2.1405 | 6,152 | 20 | 4.7330 | 7,905 | 40 |
| Gin v1.12.0 | 2.1735 | 6,152 | 20 | 4.7695 | 7,905 | 40 |
| Echo v5.4.4 | 2.2155 | 6,128 | 19 | 4.7565 | 7,825 | 39 |
| Chi v5.5.0 | 2.3325 | 6,521 | 22 | 4.9880 | 8,273 | 42 |
| Huma v2.40.0 (`humago`) | 2.4600 | 6,329 | 24 | 7.2340 | 8,314 | 55 |
| Trest HTTP adapter | 3.6290 | 7,560 | 34 | 6.6505 | 9,022 | 50 |
| Trest main App | 3.5475 | 7,032 | 32 | 6.6400 | 8,958 | 48 |

Relative to Gin, Trest App takes 63.22% more time on JSON GET and 39.22% more on invoices. Relative to Echo, the differences are 60.12% and 39.60%. The remaining Gin differences are statistically significant; [the comparison](gin-echo-comparison.txt) includes uncertainty. These results do not establish a universal framework ranking or production throughput.

## Changes and preserved behavior

- **Decode into the typed destination.** Request binding fills the route's input directly, avoiding a reflection/interface copy. The original decoder remains available and shares parsing logic. Wire validation, unknown/duplicate field handling, body limits, custom codecs, and multipart cleanup remain covered.
- **Prove native schema constraints once.** At construction, closed scalar Go types can prove some schema constraints for every possible value. For example, an `int64` cannot exceed its own representable range. Only such checks are omitted from the request path. Narrower ranges, enums, string constraints, and required omitted fields still run. Pointers, containers, floats, recursive models, custom codecs, and encoding-sensitive tags retain the appropriate existing validation path. Business input validation, authorization, and business output validation always retain their normal enforcement order.
- **Avoid repeated string and header work.** Unconstrained strings avoid unnecessary boxing and rune counting. Exact common content types avoid MIME parsing, with the complete parser retained for other spellings and parameters. Constant response content types and canonical request-ID header spelling avoid repeated formatting/normalization.
- **Batch cryptographic reads.** Each route, registry, or adapter owns a small synchronized entropy buffer. Every ID consumes a separate 16-byte portion of `crypto/rand` output and returns an independently owned string. There is no global generator or background goroutine. Refills fail without exposing partial bytes, and panic recovery cannot leave the mutex locked. Request IDs and invocation span IDs remain independent. The [ID microbenchmark](randomid-mutex.txt) also checks serial and parallel performance; it is supporting evidence, not a server-throughput measurement.

Default invocation deadlines, capacity checks, cancellation, recovery, principal propagation, trace links, and authorization remain enabled. The [hook regression](../../../operation/registry_validation_test.go) explicitly checks that input or authorization rejection happens before handler execution, including types eligible for static schema proofs. Tests also preserve output rejection.

## Profiles and remaining costs

The [profile collection](profiles/README.md) contains before/after CPU and allocation profiles for both App workloads. Initial allocation profiles identified reflection copies, header normalization, and scalar validation work; CPU stacks also exposed cryptographic ID generation. The final allocation profiles still show deadline/context creation, JSON work, request metadata, and response recording. Fresh `httptest` requests and recorders account for part of the measured work in every implementation.

The CPU samples on this host contain substantial `runtime.kevent`/GC/netpoll activity. Their percentages are not per-function explanations of the measured latency improvement. Timing claims come from the separate, non-profiled paired runs. Further work should measure context/metadata allocation and concurrent request behavior while preserving context lifetime, cancellation, and trace identity. This pass does not establish a safe way to remove those semantics.

## Method and provenance

- Apple M2 Pro, macOS 27.2.1, arm64, Go 1.27.3, `GOMAXPROCS=1`, `GIN_MODE=release`.
- Two frozen non-race binaries; one process per timed sample. Fourteen rounds rotate all seven frameworks through every position twice, reverse the second cycle, and alternate workload and before/after order.
- 392 samples in total: 14 per binary, framework, and workload. No task-owned builds, tests, or other benchmarks ran concurrently with these samples.
- Shared development host, without CPU affinity or controlled system-wide background load. Balanced ordering and unchanged controls reduce uncertainty; they do not eliminate host noise.
- Source starts at commit `73aafaf`, with the existing uncommitted experiment fixtures. Their source hashes are identical in both binaries. The core optimization is recorded in [optimization.patch](optimization.patch). [Before](before-source-hashes.json), [after](after-source-hashes.json), and [changed-source hashes](changed-source-hashes.json) identify the measured source. The source hashes were rechecked after timing.
- [run.json](run.json) records binary hashes, timestamps, execution order, toolchain/platform details, and binary build information. Framework dependency pins remain in the unchanged [go.mod](../../../experiments/adoption/go.mod) and [go.sum](../../../experiments/adoption/go.sum).

The [experiment guide](../../../experiments/adoption/README.md) describes the workloads. JSON GET serializes the same typed value on every request. Invoice POST uses synthetic authentication, field validation, tenant authorization, a shared domain recheck, and a mutex-protected idempotency store; timed requests return a previously created invoice. It does not measure new inserts. Gin, Echo, and Chi use explicit strict standard-library binding; Gin and Echo retain their JSON response APIs. Huma uses native binding with its schema-link transform disabled. Trest uses native typed binding and its default invocation policies. Parser edges, error envelopes, headers, and feature sets are not universally equivalent.

Both workloads include fresh request/recorder allocation and exclude sockets, TLS, databases, external services, application startup, and contract generation. The [earlier framework comparison](../frameworks-2026-11-11/README.md) and [10 November optimization](../optimization/README.md) remain historical evidence with their own configurations.

## Verification

- [All seven modules](all-modules.txt): `scripts/check-all.sh` passed race tests, vet, and builds, with generated-client integration and actual WebSocket tests enabled.
- [Go 1.23.12 core compatibility](go123-race.txt): race tests, vet, and build passed. [Lint](lint.txt) reported zero issues; formatting and `git diff --check` passed.
- Differential fuzzing passed **248,579 cases**: [wire validation](schema-wire-fuzz.txt), [typed values](schema-value-fuzz.txt), and [static type proofs](schema-static-fuzz.txt). Regressions cover exact numeric bounds, Unicode, unsupported encoding options, custom codecs, and application enforcement order.
- ID tests cover concurrent calls, independent generators, refills, retained strings, short reads, errors, and panic recovery. Independent reviews found no remaining validation or authorization regression.

Optional real PostgreSQL integration tests were not rerun for this change. No production credentials or existing application database were used. The core remains standard-library-only.

## Reproduce

The [comparison runner](compare.py) requires Python 3 and `benchstat`. Compile each source version with the same Go toolchain, dependencies, and environment using `go test -c` in `experiments/adoption`. The patch records the difference from the frozen baseline and can be reversed in a disposable copy of the optimized source to reconstruct it; verify against the source hashes. Keep those binaries outside the copies before switching sources.

From the repository root, using the paths to the two binaries:

```sh
python3 docs/benchmarks/framework-optimization-2026-11-11/compare.py \
  /tmp/trest-before.test /tmp/trest-after.test /tmp/trest-paired-results \
  --count 14 --time 500ms \
  --frameworks net_http,trest_app,trest,huma,chi,gin,echo
benchstat -row .name -col '/framework@(gin echo trest_app)' \
  /tmp/trest-paired-results/after.txt
```

Use a new output directory and avoid concurrent CPU-heavy work. For a new comparison of the current frameworks alone, use `experiments/adoption/benchmark.py` as described in the experiment guide. Do not treat profiler-enabled timing or different benchmark configurations as interchangeable with these samples.

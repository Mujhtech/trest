# Trest request-path optimization — 13 November 2026

**The target of beating both Gin and Echo remains unmet.** Trest App now takes **28.70% less time for JSON hello** and **27.31% less for duplicate invoices** than this round's baseline. It still takes **17.64% more time than Gin for hello**, and the invoice difference from Gin is not statistically significant. IDs, deadlines, validation, and authorization remain enabled.

## Measured results

Medians of fourteen paired samples per case, each timed for at least 500 ms. The baseline already contains the [11 November optimizations](../framework-optimization-2026-11-11/README.md); these are additional gains measured against that frozen implementation, not differences between separate historical runs.

| Trest App workload | Before, µs/op | After, µs/op | Time change | Allocations before → after |
| --- | ---: | ---: | ---: | ---: |
| JSON hello | 3.568 | 2.544 | −28.70% | 32 → 18 |
| Duplicate invoice | 6.600 | 4.798 | −27.31% | 48 → 34 |

Both timing improvements have `p < 0.001`. Unchanged Gin and Echo controls show no statistically significant before/after timing change. See [benchstat](benchstat.txt), [before](before.txt), [after](after.txt), and [exact medians](summary.json).

| Current workload | Trest App, µs/op | Gin v1.12.0, µs/op | Echo v5.4.4, µs/op |
| --- | ---: | ---: | ---: |
| JSON hello | 2.544 | 2.163 | 2.201 |
| Duplicate invoice | 4.798 | 4.766 | 4.848 |
| Cancelable-parent hello | 3.483 | 2.357 | 2.405 |
| Cancelable-parent invoice | 5.802 | 4.942 | 5.014 |

The original hello gap against Gin is significant (`p < 0.001`); invoice versus Gin is inconclusive (`p = 0.701`), so it is not a win. Trest allocates less on these original workloads: 18/34 allocations versus Gin's 20/40 and Echo's 19/39. See the [Gin comparison](gin-echo-comparison.txt) and [Echo comparison](echo-comparison.txt).

The cancelable-parent cases are separate current-only measurements, also fourteen 500 ms samples. Each creates and cancels a fresh parent context for every framework, resembling the cancellation capability of incoming server requests. Trest's gaps against Gin are 47.72% and 17.40%, both significant. They must not be presented as before/after improvements; see [samples](cancelable/samples.txt), [analysis](cancelable/benchstat.txt), and [execution order](cancelable/run.json).

## What changed

- Native scalar and flat-object decoding combines strict wire parsing, constraints, and assignment. Exact type proofs avoid redundant checks; malformed JSON, numeric ranges, required/duplicate/unknown fields, and custom-codec fallback remain enforced. Scalar parameters avoid unnecessary JSON round trips. Empty-input binding is skipped only when the request demonstrably contains nothing to bind.
- Compiled native response encoding reads every current value; only field names are pre-encoded. Unsupported types use the standard encoder. Escaping, output limits, and HEAD behavior remain covered. Route-owned scratch buffers discard unusually large storage.
- Request-owned metadata, headers, and one-use invocation frames reduce allocation. Ordinary typed routes pass context explicitly and bind without changing request fields. Middleware/authentication callbacks can retain their requests and earlier contexts. Mutable request and context storage is never pooled.
- Atomic publication and bounded admission replace invocation read locks and channel admission. Public operations without hooks can bypass empty phase machinery only when native type proofs apply. Business validation, resource resolution, authorization, output hooks, cancellation, and capacity release retain their enforcement paths.
- Lazy deadlines apply only to unobserved invocations whose parents have neither cancellation nor a deadline. Done or an unknown Value lookup materializes a standard-library context. Real cancelable parents still receive eager standard timeouts. Independent cryptographic IDs use immutable encoded blocks and atomic reservations; refills remain locked. Retaining one ID can retain a 512-byte block.

The core remains standard-library-only. ServeMux routing is unchanged. These changes add parser, encoder, and context-state logic that requires ongoing regression coverage; lower allocation counts do not establish lower latency for every workload.

## Method, remaining costs, and rejected probes

The host was an Apple M2 Pro, macOS arm64, Go 1.27.3, with `GOMAXPROCS=1` and Gin release mode. Two frozen non-race binaries produced 168 main samples. Framework, workload, and before/after ordering rotated; task-owned tests, builds, and benchmarks did not run concurrently. The shared development host had no controlled system-wide load or CPU affinity, so noise remains possible.

Both original workloads include fresh `httptest` requests and recorders and exclude network, TLS, databases, startup, and contract export. Invoice timing measures warmed duplicate lookup, not insertion. Common business policies are checked, but parser edges, headers, and framework features differ. The [experiment guide](../../../experiments/adoption/README.md) describes these limits.

[Profiles](profiles/) retain CPU and allocation evidence. Remaining work includes request/context allocation, invocation lifetime and identity management, binding, serialization, and recording. Host `runtime.kevent`/GC activity complicates CPU percentages; profiler-enabled timings are not the result above. Further safe optimization may be possible; these measurements do not prove an irreducible floor.

An isolated lightweight probe removed IDs/deadlines and was rejected after the user chose to preserve them. A principal-coallocation probe did not justify its timing/size tradeoff. Private-field routing shortcuts were rejected for standard-library coupling. Reusable-writer and parallel diagnostics remain supplemental; they do not replace this comparison. Retained [diagnostics](diagnostics/) document discarded candidates separately. Five [additional isolated candidates](diagnostics/late-candidates/README.md) examined state compaction, direct dispatch, larger raw entropy refills, combined completion, and deferred value lookup. None was merged; their measured pilots did not establish a latency improvement.

## Verification and reproduction

[All seven modules](all-modules.txt) passed race tests, vet, and builds, including live generated-client and WebSocket checks. Core Go 1.23.12 [race tests](go123-race.txt), [vet](go123-vet.txt), and [build](go123-build.txt) passed; [lint](lint.txt) reported zero issues. [Differential fuzzing](fuzz/) completed **1,743,257 executions**, covering native parsing/validation, parameters, output encoding, and ID hexadecimal conversion. Optional real PostgreSQL integration tests were not rerun; no production credentials were needed.

[Source audit](source-audit.json) verifies twelve original experiment files and dependency pins unchanged; only supplemental cancelable benchmarks were added. [Before](before-source-hashes.json) and [after](after-source-hashes.json) hashes, [the patch](optimization.patch), and [run metadata](run.json) identify the measured sources and binaries. Compile both source versions with the same toolchain using `go test -c` in `experiments/adoption`, then use [compare.py](compare.py) with `--count 14 --time 500ms --frameworks trest_app,gin,echo`. [current-run.py](current-run.py) reproduces supplemental current-only sampling. Use fresh output directories and avoid concurrent CPU-heavy work.

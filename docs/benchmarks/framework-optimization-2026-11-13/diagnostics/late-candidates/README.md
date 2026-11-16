# Additional isolated candidates

These probes followed the main comparison and did not produce a validated latency gain. **None was applied to the production source.** Their baseline is pass11, the main report's after version. They do not change the reported 28.70% / 27.31% improvement or establish a win over Gin and Echo.

| Candidate | Change | Result |
| --- | --- | --- |
| [State compaction](state-compaction/benchstat.txt) | Removed duplicate parent and slot storage; smallest request allocation 288 → 256 bytes | Eight paired 400 ms samples: hello 2.564 → 2.586 µs, invoice 4.747 → 4.760 µs; no significant timing change. Allocation bytes fell by 32 / 64 on the original workloads. |
| [Direct dispatch](direct-dispatch/benchstat.txt) | Inlined the proven public/native/no-hook branch in the unobserved invoker | Eight paired 400 ms samples: hello 2.585 → 2.583 µs, cancelable hello 3.380 → 3.377 µs; no significant change. |
| [Entropy reservoir](entropy-reservoir/benchstat.txt) | Read 1 KiB of entropy per refill while preserving separate immutable 512-byte encoded blocks | Eight paired 400 ms samples: hello 2.656 → 2.629 µs, invoice 4.998 → 4.745 µs; no significant change. Added 768 bytes of standing storage per generator. |
| [Combined completion](finish/benchstat.txt) | Added direct dispatch plus a final deadline check combined with cancellation for proven no-hook operations | Ten paired 500 ms samples with Gin/Echo controls: hello 2.750 → 2.739 µs, invoice 5.130 → 5.168 µs; no significant Trest change. Unchanged Echo controls shifted about 1.3–1.4%, further limiting attribution of small differences. |
| [Deferred value lookup](lazy-value/candidate.patch) | Used public context.WithoutCancel to avoid materializing a lazy timer solely for an ordinary Value lookup | Correctness checks passed; no timing claim. The original workloads do not exercise this unknown-key path, so it was not added as an optimization of these measured costs. |

State compaction and the reservoir also measured both cancelable workloads without a significant timing change. All runs were serial, with alternating before/after order; the first three pilots timed Trest only. Samples and execution order accompany each measured candidate. Compare only within a paired run: host conditions changed between runs.

Independent reviews found no correctness issues. State compaction, entropy reservoir, combined completion, and deferred value lookup passed focused race checks on Go 1.27 and 1.23 and the adoption race suite. Combined completion and deferred value lookup also passed focused Go 1.23 vet. Direct dispatch passed focused Go 1.27 race checks. These are isolated-candidate checks, separate from the main production version's full seven-module verification.

Each directory retains a patch against the main report's after source, source hashes, and changed-file list. Original benchmark fixtures and dependency files are unchanged. Reconstruct the main report's after source, apply a candidate patch in a separate copy, and build its adoption benchmark binary with the same Go toolchain. Use the main report's compare.py and the count, benchtime, framework, and workload selections in each run.json.

The smaller state has a memory benefit, and deferred value lookup may help other workloads; neither is presented as the requested latency win. These probes remain available for future evaluation without adding their complexity to the current implementation.

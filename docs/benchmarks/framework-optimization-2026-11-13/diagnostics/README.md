# Diagnostic evidence and rejected candidates

These experiments informed the implementation but do not establish that Trest beats Gin or Echo. They use intermediate frozen snapshots, so their absolute timings must not be combined with the final comparison.

- `lightweight/`: paired ablation against pass8, with HTTP IDs and unobserved invocation timeouts deliberately removed in an isolated copy. The altered-contract patch was never applied to the workspace. The user explicitly chose to preserve those features.
- `principal/`: isolated principal-context coallocation against pass9. It saved one allocation but added 16 bytes per invoice request and did not establish a timing improvement; the patch was rejected.
- `hex/`: eight alternating pairs comparing the fixed-size word-based encoder to standard `encoding/hex`. Whole-Generator Pair calls improved from 114.05 to 94.73 ns/op; block-only encoding is a separate microbenchmark. The core retained the encoder after differential tests and fuzzing.
- `atomic-serial/` and `atomic-parallel/`: matching before/after copies with the same word-based encoder and benchmark files. The delta is atomic ID reservation. The serial hello pilot improved 2.69%; the four-worker run did not establish a throughput improvement. The implementation was retained and included in the final comparison. The allocation increase was about three bytes per request. Mutex profiles are diagnostics, not controlled timing comparisons.
- `parallel/`: six rotated rounds at two and four workers using pass9 production code and the included supplemental benchmark. Each iteration still constructs fresh requests and recorders; aggregate ns/op measures throughput, not individual request latency. No win over both controls was established.
- `hotpath/`: pass9 production code with the included reusable-writer diagnostic, isolating handler work from request parsing and recorder allocation. It still invokes the existing handlers and serializes each returned value. It is not a replacement for the original end-to-end fixture.

A proposed exact-route shortcut was also rejected during review because it would depend on private standard-library request fields and toolchain-specific assumptions. No routing code was changed. Source/binary hashes and execution order are retained in each available `run.json`; the final production audit is `../source-audit.json`.

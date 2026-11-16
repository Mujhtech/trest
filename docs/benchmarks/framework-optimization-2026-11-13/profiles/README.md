# Final candidate profiles

These profiles use the frozen final `pass11.test` binary recorded in `../run.json`, with the exact production source in `../after-source-hashes.json`. Each workload runs for ten seconds with `GOMAXPROCS=1` and `GIN_MODE=release`, after the unprofiled comparisons finished.

`HelloJSON`, `DuplicateInvoice`, and `CancelableHelloJSON` each have a CPU profile (`.cpu`), sampled allocation profile (`.mem`), run log, CPU flat/cumulative summaries, and an allocation-object summary. The cumulative view focuses on stacks containing Trest; the flat view includes the whole process. The large `runtime.kevent` component on this host prevents treating raw sample percentages as a latency breakdown.

Use `go tool pprof` with the matching binary to inspect them. Rebuild the recorded source with the same Go toolchain using `go test -c` in `experiments/adoption`. For example:

```sh
go tool pprof -top -alloc_objects /tmp/trest-final.test DuplicateInvoice.mem
go tool pprof -http=127.0.0.1:0 /tmp/trest-final.test HelloJSON.cpu
```

Profiler-enabled ns/op is diagnostic only. Timing claims use the separate fourteen-sample paired run in the parent directory.

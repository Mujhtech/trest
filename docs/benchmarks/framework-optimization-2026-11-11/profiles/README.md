# CPU and allocation profiles

Both App workloads were profiled for five seconds with the frozen binaries, one at a time, with `GIN_MODE=release` and `GOMAXPROCS=1`. These profile runs are separate from the non-profiled paired timing samples in the parent report.

For each `HelloJSON` and `DuplicateInvoice` workload, `before` and `after` files include:

- `.cpu`: CPU samples readable by `go tool pprof`.
- `.mem`: sampled allocation/heap data. The text summaries use `-alloc_objects`.
- `-cpu-flat.txt`, `-cpu-cum.txt`, `-mem-flat.txt`, `-mem-cum.txt`: top 40 functions sorted by flat or cumulative cost.
- `-profile.txt`: the profiler-enabled benchmark's output. Do not use its timing for the before/after performance claim.

Example capture and inspection, with an already compiled binary:

```sh
GIN_MODE=release GOMAXPROCS=1 /tmp/trest-after.test \
  -test.run='^$' -test.bench='^BenchmarkHelloJSON$/^framework=trest_app$' \
  -test.benchtime=5s \
  -test.cpuprofile=/tmp/hello.cpu -test.memprofile=/tmp/hello.mem
go tool pprof -top -cum /tmp/trest-after.test /tmp/hello.cpu
go tool pprof -top -alloc_objects /tmp/trest-after.test /tmp/hello.mem
```

The recorded CPU profiles contain substantial runtime GC/netpoll/`kevent` samples. This limits precise attribution of request cost to individual functions. Allocation profiles are sampled and contain different iteration counts, so compare allocation sites rather than raw totals. The paired `benchmem` results in the parent directory establish the per-request allocation change.

Profiles contain function names and local source paths from the test binaries. No production workload, credentials, or user payloads were used.

# CPU and allocation profiles

Each capture ran the named Trest benchmark for three seconds, separately from the timing comparison. Before captures use the frozen pre-optimization binary; after captures use the final optimized binary. Profile samples are diagnostic, not substitutes for the repeated timings in the [report](../README.md).

| Workload | Before | After |
| --- | --- | --- |
| Authenticated POST CPU | [profile](post-before-cpu.pprof), [top](post-before-cpu.txt) | [profile](post-after-cpu.pprof), [top](post-after-cpu.txt) |
| Authenticated POST allocations | [profile](post-before-alloc.pprof), [objects](post-before-alloc.txt) | [profile](post-after-alloc.pprof), [objects](post-after-alloc.txt) |
| Typed hello CPU | [profile](hello-before-cpu.pprof), [top](hello-before-cpu.txt) | [profile](hello-after-cpu.pprof), [top](hello-after-cpu.txt) |
| Typed hello allocations | [profile](hello-before-alloc.pprof), [objects](hello-before-alloc.txt) | [profile](hello-after-alloc.pprof), [objects](hello-after-alloc.txt) |

Allocation files support both `alloc_objects` and `alloc_space`. The text files use objects. CPU profiles include substantial runtime/GC/platform activity; inspect cumulative application frames when locating request work.

```sh
go test -c -o /tmp/trest-profile.test .
/tmp/trest-profile.test -test.run '^$' \
  -test.bench '^BenchmarkTypedPOST/Trest$' -test.benchtime=3s \
  -test.cpuprofile=/tmp/trest.cpu -test.memprofile=/tmp/trest.mem
go tool pprof -top -cum /tmp/trest-profile.test /tmp/trest.cpu
go tool pprof -top -alloc_objects /tmp/trest-profile.test /tmp/trest.mem
```

Use the matching binary for source/disassembly inspection. The profile files retain function names, while the [run manifest](../run.json) identifies the measured binaries. Profile-run stdout is retained in the neighboring `*-run.txt` files.

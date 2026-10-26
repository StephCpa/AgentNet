# Control-path microbenchmark — 2026-10-27

The retained independent PostgreSQL 16.15 run passed all 38 runner checks,
including real MinIO, the microbenchmark and raw-report validation. Existing
correctness coverage remains 123 database tests and 8 Go unit tests; four process
helpers are excluded. The performance harness and its 24 subcases are not added
to correctness-test counts. Five Python report-validator tests passed. Both Go
vet commands and drift checking of 11 generated binding files passed.

`microbench.json` contains all 4,800 measured operations (13,200 RPCs), across
four paths, concurrency 1/8 and three repetitions of 200 operations. Every case
also warms 20 operations at matching concurrency. All measured requests succeeded
and all post-run database facts were verified; legitimate-request failure and
false-rejection rates are 0/4,800. The over-budget negative probe is outside the
timed valid workload. `microbench-report.md` is generated from the raw samples by
`test/bench/report_micro.py`, not manually assembled from selected observations.

The client and production service implementations share one Go test process,
communicating over loopback TCP/mTLS to an independent PostgreSQL process.
This is a closed-loop warm local microbenchmark; initial TLS setup, input
preparation and replay preseeding are excluded. Per-operation timing excludes
work-dispatch queue time, and protobuf byte counts exclude transport/SQL/audit
traffic. Database data grows across cases; initial global task counts are retained.

Hardware: AMD Ryzen 9 5900HS, 8 physical cores/16 logical processors,
25,132,666,880 bytes total physical memory reported by Windows. Runtime settings,
durability settings, source revision, commands and scoped proxy handling are in
`run-metadata.json` and raw JSON. OS caches/background activity were not controlled.
The isolated PostgreSQL server stopped normally (`pg_ctl.log`). No keys, database
contents, binaries or diagnostic-pilot samples are committed.

The first pilot rejected an invalid one-hour gateway TTL; the harness was fixed
to use the existing production default. A successful debug pilot then informed
the same-concurrency warmup correction. The retained final run used a new cluster
and frozen benchmark/validator sources, with no failed assertion or measurement
retry. No production service implementation changed in this increment.

These results establish the first M1 microbenchmark reference, not open-loop
capacity, the design's production message-rate target, complex-agent scale,
paired B1–B4 task benefits, or full M1/M3/M4 exit.

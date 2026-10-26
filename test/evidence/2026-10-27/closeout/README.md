# Local subtask closeout and cancellation evidence

The final frozen run passed 40 runner gates, 146 database tests and 13 unit tests.
Five subprocess helper entry points are excluded from those counts. The separate
MinIO test and root control-path microbenchmark passed; 4,800 operations and
13,200 RPCs had no failures. The isolated PostgreSQL cluster was stopped.

- [Design and remaining scope](../../../../docs/detailed-design/29-subtask-closeout-and-cancellation.md)
- [Exact commands, versions and test counts](run-metadata.json)
- [Runner results](summary.json) and [Go tests](go-tests.log)
- [Microbenchmark report](microbench-report.md) and [raw samples](microbench.json)
- [Initial development failure](development-failure-go-tests.log) and [corrected targeted run](targeted-go-tests.log)
- [Full-run cleanup failure](cleanup-failure-go-tests.log) and [Windows access probes](cleanup-access-probe.json)

The initial development run exposed an invalid outbox conflict target and two
nested-fixture expiry boundaries. The first full run then failed only during
Windows cleanup of an exited instancekey executable. Updated cleanup retries
only sharing violation 32 for at most five seconds; it never retries a command or
assertion. The final full run used the corrected sources in a new cluster.

These are local process/storage and mTLS results. They do not establish remote
settlement, full contract acceptance, production capacity, or a new TLA+ proof.

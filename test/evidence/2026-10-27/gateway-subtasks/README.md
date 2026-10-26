# Gateway subtask ingress evidence

The final frozen run passed 40 runner gates, 152 database tests and 13 unit tests.
Five subprocess helpers are excluded. MinIO passed; the root control-path
microbenchmark completed 4,800 operations and 13,200 RPCs with zero failures and
verified database facts. The isolated PostgreSQL cluster stopped.

- [Implementation and remaining scope](../../../../docs/detailed-design/30-gateway-subtask-ingress.md)
- [Commands, versions and test counts](run-metadata.json)
- [Runner results](summary.json), [Go tests](go-tests.log) and [static checks](static-checks.log)
- [Microbenchmark report](microbench-report.md) and [raw samples](microbench.json)
- [Development fixture failure](development-go-tests.log) and [corrected targeted run](targeted-go-tests.log)
- [First full-run startup failure](startup-failure-go-tests.log) and [Windows access probe](startup-access-probe.json)

The development audit constraint initially rejected an existing root signing row;
it now faults only the target child command. The first full run then failed when
Windows sharing violation 32 prevented instancerenew.exe from starting its watch
subcase. That subcase had not executed and the run is retained as failed.

The test harness now retries only Start failures with no process created and
Windows error 32, using fresh command objects for at most five seconds. Once a
process starts, its result and business assertions are never retried. Cleanup
handles only the same transient error after child waits. Updated frozen sources
passed the final full run on a new cluster.

This is an internal mTLS gRPC entry point, not completion of the Python SDK,
HTTPS/JSON edge, full contract/VERIFIED validation or cross-domain protocols.

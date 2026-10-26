# Benchmarks

The M1 microbenchmark measures implemented single-domain control paths. It does
not replace design 13's M3 paired B1–B4 task benchmark. B3 selection is recorded
in `b3-baseline-selection.md`.

## Reproduce the M1 microbenchmark

```text
python test/schema/run.py --pg-bin <PG16-bin> --work-dir .local/micro-new --kernel-tests --microbench --bench-samples 200 --bench-repetitions 3
python test/bench/report_micro.py .local/micro-new/microbench.json --output .local/micro-new/microbench-report.md
python -m unittest discover -s test/bench -p test_report_micro.py -v
```

The schema runner creates a fresh loopback PostgreSQL cluster, applies all current
migrations and runs correctness gates first. It runs the build-tagged Go harness
with a 15-minute ceiling, preserves raw samples/logs, then stops the database.
Add `--minio-bin <fixed-binary>` to independently rerun the real object-store gate.
No installed PostgreSQL service is used. Failure is not retried or discarded.

Four scenarios run at concurrency 1 and 8. Each repetition uses a new domain,
20 warmup operations at the same concurrency and the requested measured sample count; scenario order
rotates between repetitions. The four scenarios are gateway new-command
admission, exact original-receipt replay, a shared budget account under reservation
contention, and an eight-RPC gateway/lease/decision/four-reservations/local-tx chain.
The last path writes distinct records with a 256-byte ASCII payload in each value.

The test uses production services over real TCP/mTLS and a separate PostgreSQL
process, with a configured test CA, consent and registered local handler. Client
and services share a Go test process. Calls use long-lived shared connections;
initial TLS handshakes and input preparation are outside the measured window.
This is a closed-loop benchmark, with no open-loop arrival schedule or external
business/LLM traffic. Client-side timing starts when a worker receives work;
it excludes work-dispatch queue delay. Stage durations, RPC count, uncompressed
protobuf bytes and errors are retained. Protobuf bytes exclude transport, SQL and
audit traffic. Measurement overhead is included in operation latency.

After each case the harness checks task counts, exact replay identities, held
reservation counts plus a rejected overflow probe, or committed business/result
counts and settled dimensions. Successful request timing alone is insufficient.
The validator recomputes throughput and nearest-rank quantiles, requires the full
matrix and verified facts, and refuses to generate a success report from failed
or incomplete samples. It reports repetition medians and ranges without claiming
confidence intervals from a small number of runs.

Database tables grow across cases; global task counts before each run are kept.
Prior correctness tests and per-case warmup make this a warm local run. Record
hardware, runtime, database settings and commands with each retained result.
Neither the measured operation rate nor an eight-worker result proves the
production 200-message/s target, real-agent scale, or B4's benefit over B2/B3.

# PostgreSQL transaction probes

Requires PostgreSQL 16+ binaries including pgcrypto. Python uses only the standard
library. Each run creates a **new** cluster under a previously nonexistent directory,
binds a random loopback port, runs the complete migration without removing extensions,
and stops the cluster in `finally`. Data and logs remain for inspection.

```text
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/pg-new
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/pg-upgrade --schema <d6b219c-001.sql> --upgrade schema/002_reservation_cardinality.sql
```

On Windows use the portable [EDB PostgreSQL binaries](https://www.enterprisedb.com/download-postgresql-binaries).
No installed service or existing database is modified. Do not expose this disposable,
trust-authenticated cluster beyond loopback.

2026-10-27 results on PostgreSQL 16.15 with pgcrypto:

- Corrected 001: 20 checks passed (including migration, seed and server version).
- Original d6b219c 001: failed the four-dimension effect test with
  `duplicate key value violates unique constraint "uq_reservation_effect"`.
- Original 001 plus 002: 21 checks passed, including the same four-dimension test.
- Both effect and attempt can own four reservations. Rebinding or clearing an
  existing binding is rejected. Legal partial settlement commits; delegated and
  account changes cannot bypass S+H+D; duplicate roots and negative balances fail.
- Two psql sessions each request 7 units with 8 available under an account row lock;
  exactly one reservation commits. This checks the documented SQL transaction pattern;
  the separate kernel gate below exercises the Go implementation.

Evidence: `test/evidence/2026-10-27/sql-*-summary.json` and the original failure log.
These are DDL/transaction probes, not service FI or command-signature verification.

## Go kernel gate

```text
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/kernel-new --kernel-tests
```

Requires Go and resolved `go.mod` dependencies. After the 20 schema probes, this
applies migrations 003–012 (including legacy IN_FLIGHT, effect provenance, identity policy, user sessions, credentials, gateway bindings, renewals, local transactions and archive catalog) and runs `go test ./... -count=1 -v` against the isolated
database, then builds and runs the actual local outboxd executable. A skipped test
or nonzero exit fails the gate. Runner check counts include aggregate Go and CLI
gates; they are not the Go test count.

2026-10-27: 22 top-level Go kernel integration tests, 10 decision-binding subcases
and two canonicalization unit tests passed. See `services/tcs/README.md` for scope
and `test/evidence/2026-10-27/kernel/` for retained output. Full process-failure and
external-peer testing remains pending.

The delivery increment adds eight delivery tests and two kernel tests (notification
flow and T1 process kill), with four real child-process crash windows in total.
That delivery snapshot has 26 runner checks. Its evidence is in
`test/evidence/2026-10-27/delivery/`; older kernel evidence remains an immutable snapshot.

Execution increment: that snapshot has 28 runner checks. It includes 25 TCS, 8 delivery
and 17 execution database integration tests, plus two JCS and one parameter-store
unit tests. Real OS-kill windows total seven, including three mock-target FI-03
capabilities. See `test/evidence/2026-10-27/execution/`; 26 above describes the prior
delivery snapshot. Production authenticated target interoperation remains pending.

The authenticated-service increment adds migration 006 and real TCP/mTLS tests,
including the actual anetd executable with temporary PEM/config files. The runner
has 29 checks; Go counts and retained output are in
`test/evidence/2026-10-27/rpc/`. Network test fixtures provision trusted consent;
they do not establish OIDC integration or a production enrollment control plane.
The final snapshot contains 64 database tests (25 TCS, 8 delivery, 17 execution,
14 RPC) and 3 unit tests. Process-local `GOFLAGS=-p=1` was used for this Windows
run after temporary executable file-in-use errors; assertions were not retried.

OIDC/user-consent increment (design 20): migration 007, 30 runner checks and 74
database tests (the previous 64 plus 10 identity tests), with 3 unit tests. This
includes a separate HTTPS IdP fixture, the real identityd process, browser consent
feeding ICS/T1, negative token/CSRF/ownership cases and expiry after a row-lock
wait. Evidence: `test/evidence/2026-10-27/identity/`. Production IdP enrollment and
the full GW/DPoP/credential lifecycle were pending in that snapshot.

Owner-controlled credential increment (design 21): migration 008, 31 runner checks,
81 database tests (25 TCS, 8 delivery, 17 execution, 16 RPC, 15 identity) and 6 unit
tests (2 JCS, 1 parameter-store, 3 PKI). Three process-helper entries are excluded.
Tests include fresh-key rotation, exact certificate replay, concurrent rotation,
session expiry during locks, old-credential fencing after TCS/ICS lock waits and
the actual instancekey/identityd executables. Evidence:
`test/evidence/2026-10-27/credentials/`. This implements owner-controlled enrollment,
rotation and revocation; unattended renewal and complete GW/DPoP remain pending.

SDK gateway increment (design 22): migration 009, 32 runner checks, 92 database
tests (25 TCS, 8 delivery, 17 execution, 27 RPC, 15 identity) and 6 unit tests.
Evidence: `test/evidence/2026-10-27/gateway/`. Cases include first-signature/expiry
persistence, concurrent submission, original receipt recovery, interrupted permit
flow, atomic signing/acceptance audits, failure audit, three certificate-expiry
lock waits and anetd kill/restart recovery. External protocol/agentgateway hooks,
OAuth/DPoP, quotas and complete M1 exit remain pending.

Unattended-renewal increment (design 23): migration 010, 33 runner checks, 103
database tests (25 TCS, 8 delivery, 17 execution, 38 RPC, 15 identity), and 7 unit
tests. Evidence: `test/evidence/2026-10-27/renewal/`. Real anetd/instancerenew processes
exercise CA loading, two-phase activation, OS kill, post-commit publication failure,
same-key restart recovery and automatic watch triggering. Linux CLI/file tests
cross-compile only; the runtime evidence is Windows. No full M1 exit is claimed.

Local transaction increment (design 24): 34 runner checks, 111 database tests and
7 unit tests passed; three process helpers are excluded. Seven new TCS tests cover
atomic business/ledger writes, replay, CAS, expiry/revocation, cancellation and
actual commit-boundary process kills. RPC and the shipped daemon exercise the
compiled handler and restart recovery. Evidence: `test/evidence/2026-10-27/localtx/`.

## Real object-store gate

Add `--minio-bin <fixed-minio-executable>` to the kernel command to run the
separate `minio_integration` build-tag gate. It fails if the supplied binary cannot
start; ordinary HTTP fault-fixture tests do not stand in for this gate. The runner
binds PostgreSQL and the tests bind MinIO only on loopback with isolated data.

The Go gate now retains partial logs on timeout and stops the process tree. Its
cold-build limit is 360 seconds; this does not retry failed tests. Resolve modules
with a working network/proxy before running. No global proxy configuration is
changed by the driver. The repository now requires Go 1.24.9+ (Parquet dependency).

Archive increment (design 25): 36 runner checks including the real MinIO gate,
123 database tests and 8 unit tests passed; four process helpers are excluded.
The default command without MinIO has 35 runner checks and 122 database tests.
Evidence: `test/evidence/2026-10-27/archive/`. It includes archive/lookup recovery,
bounded cleanup and signed replay after expiry (FI-13), not full M1/M4 exit or
capacity evidence. Commands and development failures are recorded with the logs.

## Control-path microbenchmark

Add `--microbench --bench-samples 200 --bench-repetitions 3` to the kernel command.
This runs the separate `microbench` build tag after correctness tests, retains
every measured sample, validates raw metrics/facts and renders a report. Its
15-minute process ceiling does not retry failed measurements. See
`test/bench/README.md` for timing, workload and reporting semantics.

Design 26's retained run passed 38 runner checks with both MinIO and microbench
enabled; correctness counts remain 123 database tests and 8 Go unit tests.
The additional performance/report gates are counted separately from those tests.
Evidence: `test/evidence/2026-10-27/microbench/`; the 4,800-operation report is a
local closed-loop reference, not the M3 task comparison or production capacity.

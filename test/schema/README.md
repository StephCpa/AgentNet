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
applies migrations 003–014 (including legacy IN_FLIGHT, effect provenance, identity policy, user sessions, credentials, gateway bindings, renewals, local transactions, archive catalog, subtask allocation and closeout/cancellation) and runs `go test ./... -count=1 -v` against the isolated
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

Latest authority-chain increment (design 27): 38 runner checks, 128 database
tests (33 TCS, 8 delivery, 17 execution, 44 RPC, 15 identity, 10 archive and
1 separate MinIO test), and 13 Go unit tests passed. Four process helpers are
excluded. Evidence: `test/evidence/2026-10-27/authority-chain/`. The retained
4,800-operation benchmark also passed. Three-level grant chains are imported
fixtures; public subtask creation, cross-owner delegation and budget transfer
remain pending. The design 26 numbers below describe its earlier snapshot.

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

## Atomic subtask creation (design 28)

Migration 013 and the frozen suite passed 39 gates, 137 database tests (33 TCS,
8 delivery, 17 execution, 52 RPC, 16 identity, 10 archive, 1 separate MinIO),
and 13 Go unit tests; four process helpers are excluded. Nine new top-level
database tests cover actual signed child creation, original subject versus
recipient actor, budget transfer/replay/rollback, nested and zero allocations,
revocation, concurrency limits and expiry after waits, plus visible consent.
Existing send tests now exercise actual delegated tasks.

Evidence: `test/evidence/2026-10-27/subtasks/`. A prior frozen run failed only
Windows TempDir cleanup on instancekey.exe; its log and the successful native
DELETE-access recovery probe are retained. Unchanged sources passed a fresh
cluster run including MinIO and 4,800 benchmark operations. The benchmark
measures root control paths, not delegation cost. Child closeout, cancellation
propagation and cross-domain execution remain pending.

## Local subtask closeout and cancellation (design 29)

Migration 014 and the final frozen suite passed 40 runner gates, 146 database
tests (33 TCS, 8 delivery, 17 execution, 61 RPC, 16 identity, 10 archive and one
separate MinIO test), plus 13 unit tests. Five subprocess helpers are excluded.
Nine new top-level tests cover authoritative closeout/return, UNKNOWN preservation,
nested and zero allocations, rollback/replay, identity revocation during waits,
ancestry and sibling access checks, actual outboxd propagation, create/cancel races
and four OS termination windows.

Evidence: `test/evidence/2026-10-27/closeout/`. Development failures and a full-run
Windows image-file cleanup sharing violation are retained. The existing identity
process test now bounds cleanup-only retries of Windows error 32 after process
exit; commands/assertions are never retried. The final fresh-cluster run passed,
including MinIO and the unchanged root control-path microbenchmark (4,800 operations,
13,200 RPCs, zero failures). All isolated clusters were stopped. This does not
establish remote tree execution, complete contract/VERIFIED validation or a TLA+
proof of process recovery.

## Gateway subtask ingress (design 30)

With migrations through 014, the frozen suite passed 40 gates, 152 database tests
(33 TCS, 8 delivery, 17 execution, 67 RPC, 16 identity, 10 archive and one separate
MinIO test) and 13 unit tests; five subprocess helpers are excluded. Six new
RPC tests cover unsigned child intent binding, nested lifecycle and receipt
recovery, namespace/identity/content conflicts, authorization guards, concurrent
replay, audit rollback, expiry/inbox cleanup and post-INSERT identity/time checks.
The shipped anetd test additionally terminates signing before commit and after
commit before TCS acceptance, then recovers one child across restarts.

Evidence: `test/evidence/2026-10-27/gateway-subtasks/`. A development audit-fault
fixture error and the first full-run Windows instancerenew startup/cleanup sharing
violation are retained. The process test now retries only error 32 when Start
created no process, within five seconds, plus the same error during post-Wait
cleanup. It never retries a started command or business assertion. The final
fresh-cluster run passed MinIO and 4,800 microbenchmark operations (13,200 RPCs,
zero failures); the cluster stopped. The benchmark still measures root paths,
not delegated latency or production capacity.

## Contract admission and immutability (design 31)

Apply migrations through 015. The final frozen run passed 41 gates, 157 database
tests (37 TCS, 8 delivery, 17 execution, 68 RPC, 16 identity, 10 archive plus one
separate MinIO test), and 18 unit tests; five process helpers are excluded.
New tests cover typed contract/schema rejection, independent TCS and ICS/GW
checks, contract/task-reference immutability, new versions, concurrent conflicts,
and exact recovery of historical admissions before strict new-admission parsing.

Evidence: `test/evidence/2026-10-27/contracts/`. The initial parser admitted a
reference to a draft-07 resource under a nonstandard schema position. Its probe
and an intermediate passing run are retained separately; the final graph check
and regression cases reject it while preserving recursive 2020-12 schemas.
The final MinIO gate and 4,800-operation microbenchmark passed; the cluster stopped.
This is not ART, VERIFIED, full contract execution or milestone completion.

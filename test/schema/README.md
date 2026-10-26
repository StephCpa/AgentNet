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
applies migrations 003–005 (including legacy IN_FLIGHT and effect provenance handling) and runs `go test ./... -count=1 -v` against the isolated
database, then builds and runs the actual local outboxd executable. A skipped test
or nonzero exit fails the gate. Runner check counts include aggregate Go and CLI
gates; they are not the Go test count.

2026-10-27: 22 top-level Go kernel integration tests, 10 decision-binding subcases
and two canonicalization unit tests passed. See `services/tcs/README.md` for scope
and `test/evidence/2026-10-27/kernel/` for retained output. Full process-failure and
external-peer testing remains pending.

The delivery increment adds eight delivery tests and two kernel tests (notification
flow and T1 process kill), with four real child-process crash windows in total.
The current isolated runner has 26 checks. Latest evidence is in
`test/evidence/2026-10-27/delivery/`; older kernel evidence remains an immutable snapshot.

Execution increment: the current runner has 28 checks. It includes 25 TCS, 8 delivery
and 17 execution database integration tests, plus two JCS and one parameter-store
unit tests. Real OS-kill windows total seven, including three mock-target FI-03
capabilities. See `test/evidence/2026-10-27/execution/`; 26 above describes the prior
delivery snapshot. Production authenticated target interoperation remains pending.

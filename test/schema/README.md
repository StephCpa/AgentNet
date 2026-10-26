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
  exactly one reservation commits. This checks the documented transaction pattern,
  not the absent TCS implementation or every cross-table lock interleaving.

Evidence: `test/evidence/2026-10-27/sql-*-summary.json` and the original failure log.
These are DDL/transaction probes, not service FI or command-signature verification.

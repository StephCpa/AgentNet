# Local transaction execution — 2026-10-27

- 34 runner checks passed on an isolated PostgreSQL 16.15 cluster.
- 111 database tests: TCS 32 (25 existing + 7 local), delivery 8, execution 17,
  RPC 39, identity 15.
- 7 unit tests: canonicalization 2, parameter store 1, PKI 3, credential files 1.
- Three child-process helper entries and nested subtests are excluded.
- `go vet ./...` and drift verification of 11 generated binding files passed.
- The isolated database stopped normally. Runtime platform: Windows amd64.

Local coverage includes actual-body digest binding, replay after business updates,
concurrent record CAS, rollback on invalid parameters/metering/upper bounds/audit,
expiration or identity revocation while waiting for a business lock, and
cancellation serialized with commit. Actual children are killed after business
writes but before commit, and after commit; retries preserve the complete atomic
fact. The real anetd executable loads the registered handler, executes it over
mTLS/ICS, and returns the original result after termination and restart.

The first development run found test fixture mistakes, subsequently fixed. The
second passed all new local/RPC assertions but failed existing identity-test
temporary-file cleanup on a Windows executable sharing lock. No process remained;
the file later opened exclusively. The retained final run uses unchanged sources,
a fresh cluster and no assertion retry. Full commands and counts are recorded in
`run-metadata.json`; no private keys or database data files are retained.

These results cover a compiled trusted handler within one PostgreSQL transaction.
They do not establish atomic external side effects, an untrusted plugin sandbox,
ART/PRODUCED/VERIFIED receipt handling, Linux runtime behavior, or full M1 exit.

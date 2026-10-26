# Unattended credential renewal — 2026-10-27

- 33 runner checks passed on a fresh isolated PostgreSQL 16.15 cluster.
- 103 database tests: TCS 25, delivery 8, execution 17, RPC 38, identity 15.
- 7 unit tests: canonicalization 2, parameter-store 1, PKI 3, credential files 1.
- Three process helper entries and nested subtests are excluded from these counts.
- `go vet ./...` and drift verification of 11 generated binding files passed.
- The isolated PostgreSQL cluster stopped normally.

Real mTLS/PG tests cover prepare/activate replay, pending-certificate isolation,
fresh-key enforcement, cross-table key claims, concurrent prepare and activation,
owner revocation/rotation, preparation expiry, audit rollback, revocation during
an instance lock wait, expiry during a public-key claim lock wait, safe abandonment,
and bounded activation after the old certificate naturally expires.

A real anetd executable loads the renewal CA and performs preparation/activation.
The real instancerenew executable is killed while activation is blocked, recovers
after local publication failure following committed activation, and automatically
renews a due certificate in watch mode. Recovery keeps the same prepared private
key and subsequently accepts work through TCS. The publication-failure case also
recovers after the preparation deadline has passed. Expired, never-activated
preparations are confirmed abandoned through the old credential before replacement.

The CLI and credential-file test binary also cross-compile for Linux; they were
not executed there. File publication/locking and process recovery ran on Windows.
This does not establish arbitrary power-loss durability, CA rotation, Workload API,
node attestation, cross-domain revocation or full M1 exit. Workers must reload the
published bundle and reconnect; arbitrary processes are not reconfigured by the CLI.

Logs/metadata only: no private keys, renewal journals, session tokens or database
data files are retained. `run-metadata.json` records reproduction commands and
scope. Process-local `GOFLAGS=-p=1` serializes package builds while concurrency
and process-kill tests stay active. The retained final run had no assertion retry.

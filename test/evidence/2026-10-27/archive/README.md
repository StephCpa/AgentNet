# Partition archive and expiry cleanup — 2026-10-27

The retained run passed 36 runner checks on an isolated PostgreSQL 16.15 cluster,
123 database tests and 8 unit tests. Database counts: TCS 33 (26 kernel + 7 local),
delivery 8, execution 17, RPC 39, identity 15, archive 10, plus 1 separately tagged
real MinIO test. Four process helpers and nested subtests are excluded.
Both Go vet commands and all 11 generated binding drift checks passed. The
database stopped normally. Runtime platform: Windows amd64, Go 1.25.1.

Coverage includes row/byte chunk boundaries, empty partitions, oversized rows,
UTC/session-timezone stability, complete-month retention, frozen source writes,
upload/corruption/missing-object failures, post-DROP catalog rollback, store
binding, concurrent finalization/lookup and cleanup backlog draining before an
object-store failure. FI-13 removes an expired inbox through the real cleanup
path and verifies that the original signed command cannot create another task.

Actual child processes are killed at DETACHED, uploaded-before-checkpoint,
EXPORTED and COMPLETE. The shipped archived CLI resumes, repeats and queries.
The MinIO test uses an executable built from pinned official source (see
`minio-provenance.log` and `run-metadata.json`), not the HTTP fault fixture. It
checks authentication, conditional create-only writes, retained object VersionId,
and reads after MinIO process kill/restart and a newer object version. The binary,
private keys, temporary files and database contents are not committed.

Development runs exposed a local-timezone boundary bug and a misplaced read
transaction commit that rolled back checkpoints and repeated uploads. Both were
fixed; the latter gained a byte-boundary regression. Earlier passing/failing
runs are not substituted for this final run. Runtime/build issues and the scoped
proxy adjustment are recorded in metadata. No assertion retry occurred in the
retained run.

These are correctness and recovery results, not a capacity benchmark, production
bucket durability/retention audit, worker event-replay API or full M1/M4 exit.

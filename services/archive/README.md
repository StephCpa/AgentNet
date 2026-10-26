# Recoverable partition archive

Apply migration 012 after 011. `archived` owns maintenance of the database's
shared UTC monthly `task_events` and `audit_log` partitions. It needs a dedicated
operator DB identity with partition DDL and archive-catalog DML rights; it is not
a worker API. Task records, effects, receipts and business tables remain online.

```json
{
  "work_dir": "archive-work",
  "chunk_rows": 10000,
  "max_chunk_bytes": 67108864,
  "s3": {
    "endpoint": "https://objects.example.internal",
    "region": "us-east-1",
    "bucket": "agentnet-archive",
    "prefix": "database-prod-1"
  }
}
```

The work directory must exist; relative paths resolve against the config file.
Normal exits remove scratch files. OS termination can leave `anet-archive-*`
files there; after stopping all maintenance/lookup processes using that directory,
operators may remove those scratch files. Resume reads the durable database
checkpoint and source/object data, not the scratch files. Unreferenced uploaded
objects are retained; automatic object garbage collection is not implemented.
Set `ANET_DATABASE_URL`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optional
`AWS_SESSION_TOKEN` in the process environment. Bucket creation and retention/IAM
configuration are operator prerequisites. Use a private bucket; grant this worker
Put/Get access without object deletion or overwrite privileges. Versioning pins
reads to the verified object version. With unversioned storage, the same digest
checks detect changed bytes but cannot recover deleted objects. HTTPS is required
except for numeric loopback addresses used by isolated tests.

```text
go run ./cmd/archived -config archive.json -once
go run ./cmd/archived -config archive.json
go run ./cmd/archived -config archive.json -domain dom_example -task task_example
```

The daemon runs immediately and every 24 hours, retrying failed cycles after a
minute. S3 requests have a two-minute timeout per HTTP attempt. `-once` returns a
nonzero exit on failure. Lookup returns hot, detached and cold records, including
the source table and the complete original row JSON. It errors on overflow of
`-limit` (default 10,000; maximum 100,000); it never silently truncates evidence.
This operator lookup is not yet a worker-facing event replay API.

## Durable transitions

1. Select only complete UTC monthly partitions whose upper bound is at least
   180 days old for events or 365 days old for audit. Normalize timestamps to UTC
   regardless of the database driver's local scan timezone.
2. In one transaction, lock the parent/child, install write/truncate guards,
   detach the child, and create a `DETACHED` catalog entry. Ordinary late writes
   cannot change that source. Unmatched partition shapes are retained for review.
3. Export bounded, ordered Parquet chunks. Each carries typed source/time/sequence/
   domain/task columns and the full JSON row, preserving nulls and future columns.
   Upload create-only, read back and check digest, size, row count and final cursor,
   then commit the chunk and per-task lookup index. A lost response may leave an
   unreferenced object; retry never overwrites another object or advances a cursor
   before verification. Oversized single rows fail with the source retained.
4. Publish and verify a versioned JSON manifest, then mark `EXPORTED`. Before
   deleting the source, read back the manifest and every chunk again, compare the
   frozen source row count, and commit DROP plus `COMPLETE` atomically. Any catalog
   error rolls back the DROP. Export and lookup bind to the original store identity.

A PostgreSQL session advisory lock excludes concurrent maintenance workers and
releases after process death. The parent lock is held only during detach, never
during upload. Lookup locks prevent a gap or duplicate between hot/detached/cold
reads and archive transitions. The catalog and task index must be included in
database backup; object storage durability, access policy and backups remain
operational prerequisites. Object retention/deletion is not performed by this
worker: enforce the domain's retention policy separately, at least the specified
default three years for event archives. Audit expiry requires explicit policy.

## Cleanup scope

Cleanup runs before archive I/O and drains eligible backlogs in transactions of
at most 1,000 entries per table: command inbox rows only after
both `expires_at` and `not_after` and at least seven days since receipt; DONE outbox
rows only seven days after actual dispatch. Pending/IN_FLIGHT/DEAD work, unresolved
dead-letter references, delivery dedup inboxes and durable gateway command bindings
are retained. Source time is PostgreSQL's clock. Expired command replay is still
rejected by T1 after its inbox row has been removed (FI-13).

Migration 012 is additive. To roll back the application, stop maintenance and keep
its catalog, detached sources and objects. Do not drop the catalog or recreate an
old partition under the same name; lookup/recovery depends on those identities.
Destructive rollback uses the established snapshot/PITR procedure. This increment
does not archive tasks/effects/artifacts or delete object-store content.

The create-only protocol follows [S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html).
The Parquet implementation is pinned to [parquet-go v0.29.0](https://github.com/parquet-go/parquet-go/releases/tag/v0.29.0),
which raises the module's Go minimum from 1.24.0 to 1.24.9. Tests run on Go 1.25.1.

## Reproduction

```text
python test/schema/run.py --pg-bin <PG16-bin> --work-dir .local/archive-new --kernel-tests --minio-bin <fixed-minio-executable>
```

The base gate uses an HTTP object-store fault fixture and real PostgreSQL/child
processes. `--minio-bin` adds a separate, non-skipping real MinIO gate with bucket
versioning, signed requests, conditional writes and process restart. Test counts
and retained platform evidence are recorded in design 25 after the complete run.

BEGIN;
-- Global maintenance catalog. One database partition may contain multiple domains.
CREATE TABLE archive_jobs (
 job_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 parent_table TEXT NOT NULL CHECK(parent_table IN ('task_events','audit_log')),
 partition_name TEXT NOT NULL UNIQUE,
 partition_oid OID NOT NULL,
 range_start TIMESTAMPTZ NOT NULL,
 range_end TIMESTAMPTZ NOT NULL CHECK(range_end>range_start),
 store_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('DETACHED','EXPORTED','COMPLETE')),
 detached_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 completed_at TIMESTAMPTZ,
 manifest_key TEXT,
 manifest_digest TEXT,
 manifest_version TEXT NOT NULL DEFAULT '',
 CHECK((state='COMPLETE')=(completed_at IS NOT NULL)),
 CHECK((state='DETACHED')=(manifest_key IS NULL)),
 CHECK((manifest_key IS NULL)=(manifest_digest IS NULL))
);
CREATE TABLE archive_chunks (
 job_id UUID NOT NULL REFERENCES archive_jobs,
 ordinal INT NOT NULL CHECK(ordinal>0),
 object_key TEXT NOT NULL UNIQUE,
 object_version TEXT NOT NULL DEFAULT '',
 digest TEXT NOT NULL,
 size_bytes BIGINT NOT NULL CHECK(size_bytes>0),
 row_count BIGINT NOT NULL CHECK(row_count>0),
 last_at TIMESTAMPTZ NOT NULL,
 last_seq BIGINT NOT NULL,
 PRIMARY KEY(job_id,ordinal)
);
-- A compact lookup index into immutable Parquet chunks, rather than one row per event.
CREATE TABLE archive_task_index (
 job_id UUID NOT NULL,
 ordinal INT NOT NULL,
 domain_id TEXT NOT NULL,
 task_id TEXT NOT NULL,
 PRIMARY KEY(domain_id,task_id,job_id,ordinal),
 FOREIGN KEY(job_id,ordinal) REFERENCES archive_chunks
);
CREATE FUNCTION anet_archive_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'ANET-ARCHIVE-FROZEN: detached archive source is read-only'; END $$;
CREATE INDEX ON command_inbox(expires_at);
CREATE INDEX ON outbox(dispatched_at) WHERE status='DONE';
COMMIT;

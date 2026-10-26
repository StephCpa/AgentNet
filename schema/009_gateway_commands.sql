BEGIN;
ALTER TABLE audit_log ADD COLUMN rpc_method TEXT, ADD COLUMN reason_code TEXT;
-- Durable first signature, not an ACCEPTED receipt. Keep the binding/tombstone
-- indefinitely until a separately verified archival policy exists.
CREATE TABLE gateway_commands (
 domain_id TEXT NOT NULL REFERENCES domains,
 command_id TEXT NOT NULL,
 principal_id TEXT NOT NULL REFERENCES principals,
 agent_id TEXT NOT NULL REFERENCES agents,
 instance_id TEXT NOT NULL REFERENCES agent_instances,
 intent_digest TEXT NOT NULL,
 signed_command JSONB NOT NULL,
 root_permit JSONB,
 not_after TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(domain_id,command_id),
 CHECK(not_after>created_at)
);
COMMIT;

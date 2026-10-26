BEGIN;
ALTER TABLE effects
 ADD COLUMN dispatch_grant_id TEXT REFERENCES grants,
 ADD COLUMN dispatch_actor_instance_id TEXT REFERENCES agent_instances,
 ADD COLUMN dispatch_action TEXT,
 ADD COLUMN dispatch_resource TEXT,
 ADD COLUMN target_ttl_us BIGINT CHECK(target_ttl_us>0);

CREATE TABLE effect_execution (
 effect_id TEXT PRIMARY KEY REFERENCES effects,
 lease_token UUID,
 lease_until TIMESTAMPTZ,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 send_count INT NOT NULL DEFAULT 0 CHECK(send_count>=0),
 attempt_id UUID,
 key_expires_at TIMESTAMPTZ,
 reconcile_count INT NOT NULL DEFAULT 0,
 retry_count INT NOT NULL DEFAULT 0,
 escalated BOOLEAN NOT NULL DEFAULT FALSE,
 finished BOOLEAN NOT NULL DEFAULT FALSE,
 result_payload JSONB,
 result_signature BYTEA,
 result_key_id TEXT,
 CHECK ((lease_token IS NULL)=(lease_until IS NULL)),
 CHECK ((result_payload IS NULL)=(result_signature IS NULL)),
 CHECK ((result_payload IS NULL)=(result_key_id IS NULL))
);
CREATE INDEX ON effect_execution(next_attempt_at) WHERE NOT finished AND NOT escalated;
-- Old ADMITTED rows have no dispatch provenance. Never assume they were unsent.
INSERT INTO effect_execution(effect_id,escalated)
 SELECT effect_id,TRUE FROM effects WHERE state IN ('ADMITTED','UNKNOWN');
COMMIT;

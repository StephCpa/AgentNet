BEGIN;
CREATE TABLE local_effect_results (
 effect_id TEXT PRIMARY KEY REFERENCES effects,
 result JSONB NOT NULL CHECK(jsonb_typeof(result)='object'),
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE local_records (
 domain_id TEXT NOT NULL REFERENCES domains,
 resource TEXT NOT NULL,
 record_key TEXT NOT NULL,
 version BIGINT NOT NULL CHECK(version>0 AND version<=9007199254740991),
 value JSONB NOT NULL,
 last_effect_id TEXT NOT NULL REFERENCES effects,
 PRIMARY KEY(domain_id,resource,record_key)
);
COMMIT;

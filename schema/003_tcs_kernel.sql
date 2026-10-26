-- Single-domain transaction kernel. Legacy tasks have no owner and fail closed.
BEGIN;
ALTER TABLE tasks ADD COLUMN owner_principal_id TEXT REFERENCES principals;
ALTER TABLE tasks ADD COLUMN budget_frozen BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE effects ADD COLUMN result_digest TEXT;
COMMIT;

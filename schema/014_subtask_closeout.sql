-- Durable local cancellation and one-time delegation closeout.
BEGIN;
ALTER TABLE budget_accounts ADD COLUMN closed_at TIMESTAMPTZ;
ALTER TABLE budget_accounts ADD CONSTRAINT closed_account_balanced
  CHECK (closed_at IS NULL OR (budget_limit=settled AND delegated=0));
CREATE FUNCTION anet_closed_account_immutable() RETURNS trigger AS $$
BEGIN
  IF OLD.closed_at IS NOT NULL AND NEW IS DISTINCT FROM OLD THEN
    RAISE EXCEPTION 'ANET-BUD-CLOSED';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER closed_account_immutable BEFORE UPDATE ON budget_accounts
FOR EACH ROW EXECUTE FUNCTION anet_closed_account_immutable();

ALTER TABLE task_delegations ADD COLUMN closed_at TIMESTAMPTZ;
ALTER TABLE task_delegations ADD COLUMN close_result JSONB;
ALTER TABLE task_delegations ADD CONSTRAINT close_result_complete
  CHECK ((closed_at IS NULL) = (close_result IS NULL));
CREATE FUNCTION anet_delegation_immutable() RETURNS trigger AS $$
BEGIN
  IF (to_jsonb(NEW)-'closed_at'-'close_result') IS DISTINCT FROM
     (to_jsonb(OLD)-'closed_at'-'close_result') OR
     (OLD.closed_at IS NOT NULL AND NEW IS DISTINCT FROM OLD) THEN
    RAISE EXCEPTION 'ANET-BUD-DELEGATION-IMMUTABLE';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER delegation_immutable BEFORE UPDATE ON task_delegations
FOR EACH ROW EXECUTE FUNCTION anet_delegation_immutable();

-- Intent references deliberately have no task FKs: enqueueing descendants must
-- never acquire child row locks while holding the current task lock. The worker
-- verifies the real ancestry and cancellation origin before applying any intent.
CREATE TABLE task_cancellations (
  task_id TEXT PRIMARY KEY,
  parent_task_id TEXT NOT NULL,
  origin_task_id TEXT NOT NULL,
  domain_id TEXT NOT NULL REFERENCES domains,
  state TEXT NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING','DONE','INVALID')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  completed_at TIMESTAMPTZ
);
CREATE INDEX ON task_cancellations(domain_id,created_at,task_id) WHERE state='PENDING';
COMMIT;

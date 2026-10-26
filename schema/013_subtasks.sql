-- Single-domain atomic delegation. Apply after 012_archive.sql.
BEGIN;
CREATE TABLE recipient_policies (
  policy_id TEXT PRIMARY KEY,
  domain_id TEXT NOT NULL REFERENCES domains,
  agent_id TEXT NOT NULL REFERENCES agents,
  instance_id TEXT NOT NULL REFERENCES agent_instances,
  version TEXT NOT NULL CHECK (version <> ''),
  rights JSONB NOT NULL,
  not_before TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  CHECK (expires_at > not_before)
);
CREATE FUNCTION anet_recipient_policy_immutable() RETURNS trigger AS $$
BEGIN
  IF (to_jsonb(NEW)-'revoked_at') IS DISTINCT FROM (to_jsonb(OLD)-'revoked_at')
     OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
    RAISE EXCEPTION 'ANET-AUT-POLICY-IMMUTABLE';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER recipient_policy_immutable BEFORE UPDATE ON recipient_policies
FOR EACH ROW EXECUTE FUNCTION anet_recipient_policy_immutable();

CREATE TABLE task_delegations (
  child_task_id TEXT PRIMARY KEY REFERENCES tasks,
  parent_task_id TEXT NOT NULL REFERENCES tasks,
  child_grant_id TEXT NOT NULL UNIQUE REFERENCES grants,
  recipient_policy_id TEXT NOT NULL REFERENCES recipient_policies,
  creator_principal_id TEXT NOT NULL REFERENCES principals,
  creator_agent_id TEXT NOT NULL REFERENCES agents,
  creator_instance_id TEXT NOT NULL REFERENCES agent_instances,
  command_id TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX ON task_delegations(parent_task_id);
CREATE TABLE delegation_allocations (
  child_account_id TEXT PRIMARY KEY REFERENCES budget_accounts,
  parent_account_id TEXT NOT NULL REFERENCES budget_accounts,
  reservation_id TEXT NOT NULL UNIQUE REFERENCES budget_reservations,
  child_task_id TEXT NOT NULL REFERENCES task_delegations(child_task_id),
  amount NUMERIC(20,6) NOT NULL CHECK (amount > 0)
);
-- Every task owns exactly one account in each dimension, including children.
CREATE UNIQUE INDEX budget_task_dimension ON budget_accounts(task_id,dimension);
COMMIT;

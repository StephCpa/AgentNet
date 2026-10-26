-- Admission pins a version permanently. New semantics require a new version and
-- a new task; ordinary task_version changes must never rebind an existing task.
BEGIN;
CREATE FUNCTION anet_contract_immutable() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'ANET-CMD-CONTRACT-IMMUTABLE';
  END IF;
  IF NEW IS DISTINCT FROM OLD THEN
    RAISE EXCEPTION 'ANET-CMD-CONTRACT-IMMUTABLE';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER contract_immutable BEFORE UPDATE OR DELETE ON contracts
FOR EACH ROW EXECUTE FUNCTION anet_contract_immutable();

CREATE FUNCTION anet_task_contract_immutable() RETURNS trigger AS $$
BEGIN
  IF (NEW.contract_id,NEW.contract_version) IS DISTINCT FROM
     (OLD.contract_id,OLD.contract_version) THEN
    RAISE EXCEPTION 'ANET-TSK-CONTRACT-IMMUTABLE';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER task_contract_immutable BEFORE UPDATE ON tasks
FOR EACH ROW EXECUTE FUNCTION anet_task_contract_immutable();
COMMIT;

-- Upgrade an existing v0.2 (d6b219c) database. Safe after the corrected 001 too.
-- One reservation has one immutable target; one target can hold several dimensions.
BEGIN;
DROP INDEX IF EXISTS uq_reservation_effect;
DROP INDEX IF EXISTS uq_reservation_attempt;
CREATE INDEX IF NOT EXISTS ix_reservation_effect
  ON budget_reservations (effect_id) WHERE effect_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS ix_reservation_attempt
  ON budget_reservations (attempt_id) WHERE attempt_id IS NOT NULL;
COMMIT;

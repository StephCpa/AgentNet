BEGIN;

ALTER TABLE outbox
  ADD COLUMN destination TEXT NOT NULL DEFAULT 'local-control',
  ADD COLUMN lease_token UUID,
  ADD COLUMN lease_until TIMESTAMPTZ,
  ADD COLUMN last_failure TEXT;

-- Legacy IN_FLIGHT rows have no recoverable lease. Redelivery requires an inbox.
UPDATE outbox SET status='PENDING' WHERE status='IN_FLIGHT';
ALTER TABLE outbox ADD CONSTRAINT outbox_lease_shape CHECK (
  (status='IN_FLIGHT' AND lease_token IS NOT NULL AND lease_until IS NOT NULL)
  OR (status<>'IN_FLIGHT' AND lease_token IS NULL AND lease_until IS NULL)
);
ALTER TABLE outbox ADD CONSTRAINT outbox_attempts_nonnegative CHECK (attempts>=0);
CREATE INDEX outbox_expired_leases ON outbox(lease_until) WHERE status='IN_FLIGHT';

-- Acceptance is a durable notification, never proof of an external business effect.
CREATE TABLE delivery_inbox (
  source_domain TEXT NOT NULL REFERENCES domains,
  destination TEXT NOT NULL,
  dedup_key TEXT NOT NULL,
  task_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  payload JSONB NOT NULL,
  message_digest TEXT NOT NULL,
  accepted_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (source_domain,destination,dedup_key)
);

-- Durable alert; a monitoring bridge can acknowledge it independently of delivery.
CREATE TABLE delivery_dead_letters (
  outbox_seq BIGINT PRIMARY KEY REFERENCES outbox,
  source_domain TEXT NOT NULL REFERENCES domains,
  destination TEXT NOT NULL,
  attempts INT NOT NULL,
  reason TEXT NOT NULL,
  recorded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  acknowledged_at TIMESTAMPTZ
);
COMMIT;

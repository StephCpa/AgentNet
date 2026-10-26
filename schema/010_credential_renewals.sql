BEGIN;
CREATE TABLE credential_renewals (
 instance_id TEXT NOT NULL REFERENCES agent_instances,
 request_id TEXT NOT NULL,
 base_version BIGINT NOT NULL CHECK(base_version>0),
 base_fingerprint TEXT NOT NULL,
 payload_digest TEXT NOT NULL,
 fingerprint TEXT NOT NULL UNIQUE,
 public_key_hash TEXT NOT NULL UNIQUE,
 certificate_der BYTEA NOT NULL,
 certificate_pem BYTEA NOT NULL,
 certificate_expires_at TIMESTAMPTZ NOT NULL,
 prepare_expires_at TIMESTAMPTZ NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','active','expired','superseded')),
 activated_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(instance_id,request_id),
 CHECK(prepare_expires_at<=certificate_expires_at)
);
CREATE UNIQUE INDEX one_pending_renewal ON credential_renewals(instance_id) WHERE status='pending';
-- Serialize public-key claims across owner issuance and prepared renewals.
-- Activation alone copies the same certificate into committed history.
CREATE FUNCTION anet_credential_key_claim() RETURNS trigger AS $$ BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended('anet.credential-key:'||NEW.public_key_hash,0));
 IF TG_TABLE_NAME='credential_renewals' THEN
  IF EXISTS(SELECT 1 FROM instance_credentials WHERE public_key_hash=NEW.public_key_hash) THEN
   RAISE EXCEPTION 'ANET-ID-KEY-REUSED' USING ERRCODE='23505';
  END IF;
 ELSE
  IF EXISTS(SELECT 1 FROM credential_renewals r WHERE r.public_key_hash=NEW.public_key_hash
     AND (r.fingerprint<>NEW.fingerprint OR r.instance_id<>NEW.instance_id OR r.base_version+1<>NEW.version)) THEN
   RAISE EXCEPTION 'ANET-ID-KEY-REUSED' USING ERRCODE='23505';
  END IF;
 END IF;
 RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER credential_key_claim BEFORE INSERT ON instance_credentials FOR EACH ROW EXECUTE FUNCTION anet_credential_key_claim();
CREATE TRIGGER renewal_key_claim BEFORE INSERT ON credential_renewals FOR EACH ROW EXECUTE FUNCTION anet_credential_key_claim();
COMMIT;

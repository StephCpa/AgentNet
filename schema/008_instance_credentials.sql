BEGIN;
ALTER TABLE agent_instances ADD COLUMN credential_version BIGINT NOT NULL DEFAULT 1 CHECK(credential_version>0);
CREATE TABLE instance_credentials (
 fingerprint TEXT PRIMARY KEY,
 instance_id TEXT NOT NULL REFERENCES agent_instances,
 version BIGINT NOT NULL CHECK(version>0),
 public_key_hash TEXT NOT NULL UNIQUE,
 certificate_der BYTEA NOT NULL,
 certificate_pem BYTEA NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 retired_at TIMESTAMPTZ,
 issued_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(instance_id,version)
);
CREATE TABLE credential_requests (
 domain_id TEXT NOT NULL REFERENCES domains,
 owner_principal_id TEXT NOT NULL REFERENCES principals,
 request_id TEXT NOT NULL,
 payload_digest TEXT NOT NULL,
 fingerprint TEXT NOT NULL REFERENCES instance_credentials,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(domain_id,owner_principal_id,request_id)
);
-- The old row's credential is retired atomically with replacement/revocation.
CREATE FUNCTION anet_credential_version() RETURNS trigger AS $$ BEGIN
 IF NEW.credential_version<OLD.credential_version OR
    (NEW.cred_hash IS DISTINCT FROM OLD.cred_hash AND NEW.credential_version<>OLD.credential_version+1) THEN
   RAISE EXCEPTION 'ANET-ID-CREDENTIAL-VERSION';
 END IF;
 IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS NULL THEN
   RAISE EXCEPTION 'ANET-ID-INSTANCE-REVOKED';
 END IF;
 IF NEW.cred_hash IS DISTINCT FROM OLD.cred_hash OR NEW.revoked_at IS NOT NULL THEN
   UPDATE instance_credentials SET retired_at=COALESCE(retired_at,clock_timestamp())
    WHERE instance_id=OLD.instance_id AND fingerprint=OLD.cred_hash;
 END IF;
 RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER instance_credential_version BEFORE UPDATE ON agent_instances FOR EACH ROW EXECUTE FUNCTION anet_credential_version();
COMMIT;

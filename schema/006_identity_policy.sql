BEGIN;
-- A leaf certificate hash identifies one instance; legacy non-certificate test
-- credentials are not interpreted as mTLS registrations.
CREATE UNIQUE INDEX instance_cert_unique ON agent_instances(cred_hash) WHERE cred_hash LIKE 'sha256:%';
CREATE UNIQUE INDEX instance_spiffe_unique ON agent_instances(spiffe_id) WHERE spiffe_id IS NOT NULL;
CREATE TABLE instance_roles (
 instance_id TEXT NOT NULL REFERENCES agent_instances,
 role TEXT NOT NULL CHECK(role IN ('adapter','identity-admin')),
 PRIMARY KEY(instance_id,role)
);
-- Standing user consent is provisioned through a trusted control plane. An agent
-- can request a subset but cannot create or enlarge this record via worker RPCs.
CREATE TABLE root_consents (
 consent_id TEXT PRIMARY KEY,
 domain_id TEXT NOT NULL REFERENCES domains,
 subject_principal_id TEXT NOT NULL REFERENCES principals,
 actor_agent_id TEXT NOT NULL REFERENCES agents,
 actor_instance_id TEXT REFERENCES agent_instances,
 version BIGINT NOT NULL DEFAULT 1 CHECK(version>0),
 rights JSONB NOT NULL,
 task_limits JSONB NOT NULL,
 not_before TIMESTAMPTZ NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 policy_version TEXT NOT NULL,
 revoked_at TIMESTAMPTZ,
 CHECK(expires_at>not_before)
);
CREATE FUNCTION anet_consent_version() RETURNS trigger AS $$ BEGIN
 IF NEW.version<=OLD.version THEN RAISE EXCEPTION 'ANET-AUT-CONSENT-VERSION'; END IF;
 RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER root_consent_version BEFORE UPDATE ON root_consents FOR EACH ROW EXECUTE FUNCTION anet_consent_version();
ALTER TABLE grants ADD COLUMN consent_id TEXT REFERENCES root_consents, ADD COLUMN consent_version BIGINT;
ALTER TABLE effects ADD COLUMN dispatch_data_class TEXT;
CREATE TABLE authorization_decisions (
 decision_id TEXT PRIMARY KEY,
 domain_id TEXT NOT NULL REFERENCES domains,
 task_id TEXT NOT NULL REFERENCES tasks,
 grant_id TEXT NOT NULL REFERENCES grants,
 signed_payload JSONB NOT NULL,
 signature BYTEA NOT NULL,
 key_id TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
COMMIT;

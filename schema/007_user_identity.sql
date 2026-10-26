BEGIN;
-- Issuer + subject is authoritative. Email/display names never identify users.
CREATE TABLE oidc_subjects (
 domain_id TEXT NOT NULL REFERENCES domains,
 issuer TEXT NOT NULL,
 subject TEXT NOT NULL,
 principal_id TEXT NOT NULL REFERENCES principals,
 PRIMARY KEY(domain_id,issuer,subject)
);
CREATE TABLE oidc_logins (
 state_hash TEXT PRIMARY KEY,
 domain_id TEXT NOT NULL REFERENCES domains,
 issuer TEXT NOT NULL,
 browser_hash TEXT NOT NULL,
 nonce TEXT NOT NULL,
 pkce_verifier TEXT NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX ON oidc_logins(expires_at);
CREATE TABLE user_sessions (
 session_hash TEXT PRIMARY KEY,
 domain_id TEXT NOT NULL REFERENCES domains,
 issuer TEXT NOT NULL,
 subject TEXT NOT NULL,
 principal_id TEXT NOT NULL REFERENCES principals,
 expires_at TIMESTAMPTZ NOT NULL,
 revoked_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX ON user_sessions(expires_at);
CREATE INDEX ON user_sessions(principal_id);
COMMIT;

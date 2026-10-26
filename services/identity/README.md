# OIDC login and user consent

Owner enrollment and manual rotation remain here. Managed instances can also use
the separate two-phase renewal service and local `instancerenew` client (design 23);
that path does not create users/Agents, grant roles or replace owner revocation.

`cmd/identityd` runs the HTTPS user control page. It performs authorization-code
login with discovery/JWKS, S256 PKCE, state bound to a browser cookie, and a nonce
bound to the ID token. It verifies signature, exact issuer, client audience, azp
where applicable, issue/expiry times, nonce and any access-token hash. Supported
ID-token algorithms are RS256, ES256 and EdDSA; no unsigned/HMAC fallback exists.

`issuer + subject` maps to a previously provisioned user principal in
`oidc_subjects`. Email, display name and request-body user IDs cannot create or
link accounts. User sessions are opaque random cookies; only their hashes are
stored. Sessions last no longer than the ID token (at most one hour, with 500 ms
expiry margin). Every authenticated request rechecks the session, mapping and
live principal in PostgreSQL. Local logout revokes the session in the database.

The control page shows configured permission profiles and owned Agents. A user
can approve a profile with lower per-task ceilings, choose a shorter validity
period, optionally restrict it to one owned registered instance, and revoke an
existing consent. These are standing permissions for multiple tasks; they are
not aggregate user spending limits. The page states this before approval.

The form cannot supply arbitrary rights, policy versions or subject identity.
Mutations require an exact Origin and session-derived CSRF token. Unknown and
duplicate fields are rejected. Consent writes and successful audit events share
a transaction; failed transactions roll back before a separate denial audit.
An unavailable denial audit returns 503. Session expiry is rechecked after lock
waits. Revocation requires the current version and increments it, invalidating
previous ICS permits/decisions through the existing T1/T2/send checks.

## Deployment

Apply migrations 001–008. Provision the domain, user principal and Agent through
the trusted administration path, then add the exact IdP subject mapping. Instances
can be enrolled through the owner control page described below:

```sql
INSERT INTO oidc_subjects(domain_id,issuer,subject,principal_id)
VALUES ('dom_example','https://id.example.org','<immutable IdP subject>','usr_example');
```

Register a code-flow client at the IdP with an exact callback URL such as
`https://identity.example.org/callback`. Discovery must advertise PKCE S256 and
all endpoints must use HTTPS. The service does not follow upstream redirects.
Client authentication follows the provider token endpoint; `client_secret_file`
is optional for a public client. The IdP must issue ID tokens of at most one hour.

```json
{
  "domain": "dom_example",
  "listen": "127.0.0.1:8444",
  "public_url": "https://identity.example.org",
  "issuer": "https://id.example.org",
  "client_id": "agentnet-control",
  "client_secret_file": "oidc-client-secret",
  "certificate_file": "server.pem",
  "private_key_file": "server.key",
  "profiles": {
    "research": {
      "name": "Research tools",
      "policy_version": "research/1",
      "max_hours": 24,
      "rights": {
        "actions": ["search"], "resources": ["catalog:research"],
        "data_classes": ["public"], "depth_left": 0,
        "delegation_scope": [],
        "spatial": {"domains": ["dom_example"], "egress_only_via_gateway": true}
      },
      "task_limits": {"money": "10", "tokens": "100000", "calls": "100", "messages": "100"}
    }
  }
}
```

`public_url` is an HTTPS origin without a trailing slash; its authority must match
the incoming Host exactly. Paths are relative to the config file. Optional
`issuer_ca_file` adds a private issuer CA to the system roots. There is no TLS
verification bypass, plaintext listener or trust in forwarded identity headers.

```powershell
$env:ANET_DATABASE_URL = '<deployment database connection string>'
go run ./cmd/identityd -config <identity-config.json>
```

The process loads certificates and explicit configuration; it neither migrates nor
creates users/Agents automatically. The PKCE verifier exists only in the short-lived
login transaction table until callback consumption/expiry cleanup. ID/access tokens,
client secrets, cookies and authorization codes are not written to the audit log.

## Validation and remaining work

```text
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/identity-new --kernel-tests
```

Tests use a separate HTTPS IdP implementing discovery, authorization, one-use code
exchange, PKCE and RSA/JWKS. They cover actual identityd startup, legitimate login,
claim/signature failures, code injection, state/callback replay, CSRF, ownership,
session/mapping/principal revocation, expiry after a real row-lock wait, consent
versions, and browser approval/withdrawal feeding ICS/T1. Retained evidence:
`test/evidence/2026-10-27/identity/` and design 20.

No particular production IdP has been provisioned or tested. Refresh tokens,
IdP back-channel/global logout, user/Agent administration, CA/trust-root
rotation, client-credentials + DPoP, token exchange and the full GW
pipeline remain separate work. Expired sessions require a new login. Legacy
manually provisioned consent remains readable/revocable by its owner.

Protocol references: [OIDC Core, authorization code flow](https://openid.net/specs/openid-connect-core-1_0.html#CodeFlowAuth),
[OAuth security BCP, code injection and CSRF](https://www.rfc-editor.org/rfc/rfc9700.html#section-2.1.1).

## Owner-controlled instance credentials (design 21)

Apply migration 008. Add `credential_ca_file`, `credential_ca_key_file`,
`trust_domain` and optional `credential_ttl` (default `12h`, range `1m`–`24h`) to
the identityd config. The first CA PEM certificate must match the signing key;
remaining certificates are the issuing chain. Configure TCS `client_ca_file` to
trust that chain. The CA and browser HTTPS server certificate have separate roles.
No CA is created automatically. Without a configured CA, signing is disabled;
an authenticated owner can still revoke an existing instance.

On the workload host, generate a key in a new directory:

```text
go run ./cmd/instancekey -out <new-key-directory>
```

Submit only `instance.csr` through the control page. Keep `instance.key` on the
workload host and install the returned certificate chain alongside it. The tool
refuses an existing output directory; protect the local files using OS permissions
and workload isolation. Server-side registration never receives the private key.

`POST /instances` is a same-origin, session/CSRF-authenticated form endpoint:

| Action | Fields beyond `csrf` and `action` | Outcome |
|---|---|---|
| `enroll` | `agent`, `csr`, `request_id` | New server-assigned instance; PEM chain download |
| `rotate` | Above plus `instance`, `version` | Same instance, next credential version; PEM download |
| `revoke` | `instance`, `version` | Terminal instance revocation and redirect to control page |

The page generates 43-character random request IDs; clients retry the same body
with the same ID after a lost response. A JCS descriptor binds CSR/action/Agent/
instance/version. Active duplicate issuance returns identical saved PEM; changed
payloads or retired credentials return 409. Successful downloads include
`X-AgentNet-Instance`, `X-AgentNet-Credential-Version` and `X-AgentNet-Existing`.

CSR proof of possession is mandatory. Ed25519, RSA 2048–8192 and P-256/P-384 keys
are supported; SHA-1 and unknown signing algorithms are rejected. Requested SANs,
subjects and CA/extensions never override the server-assigned identity. New
registration grants no adapter/admin role. Issued public keys cannot be shared
across managed instances or reused for rotation; generate a fresh key instead.

Current certificate replacement and history retirement are atomic. Rotation has
no overlapping old-key window; install the new key/certificate and reconnect.
RPC checks the current fingerprint on every call; ICS/TCS recheck the transport's
fingerprint after relevant lock waits. Logical caller identity and command
deduplication survive rotation. Unmanaged legacy rows lack fabricated certificate
history; an `ins_` instance can be migrated by owner-authorized fresh-key rotation.

Revocation here is enforced by AgentNet's live registry, not a published CRL/OCSP
service. Other services that check only the CA chain do not inherit this immediate
revocation behavior. Automatic renewal is provided separately by design 23;
node attestation, CA rollover and cross-domain revocation remain pending.
Owner-control evidence: `test/evidence/2026-10-27/credentials/`.

# Instance-authenticated renewal

This service renews an already enrolled **managed** instance credential. The
instance must have an `instance_credentials` record, a current unrevoked client
certificate and a live owner/Agent. Initial enrollment remains the OIDC owner
control plane. Legacy registrations without certificate history require owner
rotation into managed history first.

## Protocol and recovery

`anet.credentials.v1.InstanceCredentials` is an internal mTLS gRPC service:

| Method | Credential required | Result |
|---|---|---|
| GetStatus | Current certificate | Instance ID, version, fingerprint and expiry |
| PrepareRenewal | Current certificate | Fresh CSR -> persisted candidate certificate, next version and activation deadline |
| ActivateRenewal | Candidate's new key/certificate | Atomic activation, old-key retirement and audit; idempotent retry |
| AbandonRenewal | Still-current old certificate | Confirm a never-activated attempt can be discarded; preserve the current credential |

Bodies contain no principal, Agent or instance selector. A preparation is bound to
the authenticated instance, base version/fingerprint, random request ID and CSR
digest. Repeating it returns the exact original certificate; changing its CSR or
reusing a public key fails. At most one pending preparation exists per instance.
Global key-claim triggers serialize owner issuance and prepared renewals across
their two tables. Abandoned/expired keys remain reserved and cannot be recycled.

A pending certificate is recognized **only** on the exact ActivateRenewal method.
It cannot call GetStatus, prepare another credential, submit work or obtain an
authorization decision. Its activation permission lasts at most five minutes and
never beyond the candidate certificate expiry. A preparation made while the old
certificate was valid can activate during that bounded window after the old one
naturally expires. Owner revocation/rotation, Agent suspension and principal
revocation invalidate it immediately through the live registry.

Activation locks the instance before the renewal row, rechecks identity, version,
base fingerprint and activation deadline, inserts committed certificate history,
updates the instance and writes audit in one transaction. It rechecks the deadline
after the shared key-claim lock too. Audit failure leaves the old credential usable.
An activated request remains recoverable with its new current certificate even
after the original preparation deadline, until that certificate expires or is
revoked/replaced. It cannot be abandoned after activation.

## Server configuration

Apply migrations 001–010, then explicitly enable renewal in `anetd`:

```json
{
  "credential_ca_file": "workload-ca-chain.pem",
  "credential_ca_key_file": "workload-ca.key",
  "credential_ttl": "12h"
}
```

The existing `trust_domain` supplies the instance URI namespace. The CA must be
trusted by `client_ca_file`; a missing key, invalid CA, mismatched key or unsupported
TTL fails startup. The TTL range is one minute through 24 hours. All three fields
omitted disables the service; no CA is generated automatically. This is local
controlled issuance, not a Workload API, node attestation or CA rotation service.

## Workload client

`cmd/instancerenew` generates keys locally and stores a recovery journal before
each remote phase. Configure it on the workload host:

```json
{
  "endpoint": "anet.internal:7443",
  "server_name": "anet.internal",
  "server_ca_file": "server-ca.pem",
  "bundle_file": "current.pem",
  "state_dir": "renewal-state"
}
```

`current.pem` initially contains the enrolled certificate chain **and its PKCS8
private key**. Server CA trust is separate from workload signing CA configuration.
Paths are relative to the configuration file. Run:

```text
go run ./cmd/instancerenew -config workload.json
go run ./cmd/instancerenew -config workload.json -watch
```

One-shot mode renews immediately. Watch mode checks every ten seconds and starts
renewal with one third of the certificate lifetime remaining, capped at one hour;
pending work resumes immediately on startup. Each attempt has a 45-second timeout.
The state directory uses an OS-held exclusive file lock, released on process death.
One state directory must be shared by all renewal processes for the same bundle.

The journal saves the new private key and CSR before preparation, then saves the
returned certificate before activation. The client authenticates activation with
that new key. Only after activation succeeds does it replace `current.pem` as one
complete certificate/key bundle. Same-directory temporary files are synced before
rename; Windows uses replace-existing/write-through MoveFileEx, Unix uses rename
and directory sync. The old bundle is not truncated on a failed write. Tests prove
process-crash recovery, not arbitrary hardware/power-loss guarantees.

On restart, an existing journal is retried with the same request/key. If publication
failed after server activation, the new certificate recovers activation even though
the old certificate no longer works. If preparation expired without activation,
the still-current old certificate must confirm abandonment before keys are removed.
If neither credential can authenticate, owner recovery is required; the client
keeps recovery state. Do not delete pending state to force a retry.

Workload clients must reload the bundle and reconnect after publication. Existing
TLS channels using the retired certificate are rejected; this client does not
silently reconfigure arbitrary worker processes or promise uninterrupted streams.
Protect the state directory and bundle with host access controls (including Windows
ACLs); Go file modes alone are not Windows process isolation. Journals and bundles
contain private keys and must never be copied into test evidence or logs.

See design 23 for test evidence, the measured scope and remaining M1 work.

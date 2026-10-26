# Authenticated TCS / ICS service

`cmd/anetd` serves the implemented TCS and ICS methods over TLS 1.3 with mandatory
client certificates. The leaf must have a lifetime of at most 24 hours and exactly
one URI SAN: `spiffe://<trust_domain>/agent-instance/<instance_id>`. Every RPC checks
the registered certificate fingerprint, exact URI, live principal/agent/instance,
and any required role in PostgreSQL. CN, metadata and request body identity are
not authentication inputs. Existing TLS channels do not bypass revocation checks.

Apply migrations 001–009 and provision the domain, identities, certificate
registrations and standing `root_consents` through a trusted control plane. These
administrative operations are not exposed to workers. Consent limits are **per
task**, not aggregate user quotas. A consent update must increase its version.

## Running

```powershell
$env:ANET_DATABASE_URL = '<deployment database connection string>'
go run ./cmd/anetd -config <server-config.json>
```

Configuration uses the JSON fields of `FileConfig` in `runtime.go`:

```json
{
  "domain": "dom_example",
  "trust_domain": "example.internal",
  "listen": "127.0.0.1:8443",
  "certificate_file": "server.pem",
  "private_key_file": "server.key",
  "client_ca_file": "client-ca.pem",
  "ics_key_id": "ics-1",
  "ics_signing_key_file": "ics-ed25519-pkcs8.pem",
  "command_keys": {"gateway-1": "<base64 Ed25519 public key>"},
  "result_keys": {"adapter-1": "<base64 Ed25519 public key>"},
  "operations": {
    "bank": {
      "action": "pay", "resource": "account:example",
      "capability": "queryable", "purpose": "tool_call",
      "dimensions": ["money", "tokens", "calls", "messages"],
      "sensitivity": "sensitive", "data_class": "internal"
    }
  }
}
```

Paths resolve relative to the config file. Key placeholders must be replaced by
real public keys; no development identity or consent is created at startup.
`ics_verification_keys` optionally retains old public keys during rotation; the
current signing key is included automatically. `idempotent-key` operations also
require `target_ttl` (a Go duration such as `24h`). There is no plaintext listener.

## Implemented RPC profile

| RPC | Accepted input and behavior |
|---|---|
| GW SubmitCommand (optional) | Unsigned strict intent; durable server signature/expiry, ICS authorization, TCS acceptance and exact receipt recovery |
| ICS IssueRootPermit | Signed GW command; exact consent subset and per-task ceilings; short-lived signed permit |
| TCS SubmitCommand | Only signed command + signed root permit; root grant, task, accounts, receipt and notification commit in T1 |
| TCS GetTask | Owner-checked consistent task/effects/receipts/four-dimension budget snapshot |
| TCS AcquireLease / RenewLease | Worker identity comes from mTLS; optional body instance must match |
| TCS ReserveBudget / ReleaseReservation | Unbound reservations only; no caller-supplied effect/attempt binding or soft TTL |
| ICS Decide | Authoritative root grant/consent snapshot, configured action/resource/class, signed decision persisted |
| TCS AdmitEffect | Signed ICS decision and proposal bindings; unsigned legacy authorization rejected |
| TCS CancelTask | Durable cancellation, repeated cancellation returns original version |
| TCS ReportEffectResult | Live adapter role + signed adapter fact; late settlement remains possible after cancellation |
| ICS Revoke | Owned root grant via `grant_id`; repeated revocation returns original effective time |

Unknown protobuf fields and unsupported legacy request fields are rejected. ANET
codes use gRPC `ErrorInfo`; database internals are not returned. Other declared
RPCs return `Unimplemented`, including delegation, registration, event streaming
and token exchange. DIR bindings are generated but no DIR server is registered.

`anetd` runs TCS/ICS and optionally the SDK Gateway (design 22). Outbox delivery and the execution engine remain separate
components; starting this daemon alone does not execute external business effects.
`services/identity` now supplies OIDC browser login and user consent (design 20).
Owner-controlled certificate enrollment/rotation/revocation is now provided by
design 21; fingerprints and certificate expiry are rechecked inside GW/ICS/TCS
after lock waits. Design 22 adds durable command signing and intercepted RPC
failure audit, distinguishing definite denials from unknown outcomes. See
[`services/gateway`](../gateway/README.md) for configuration and coverage limits.
DPoP, unattended renewal, general revocation/push, production target transport
and ART classification remain unfinished.

## Verification

```text
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/rpc-new --kernel-tests
```

The suite uses real TCP/mTLS and PostgreSQL, including a separately built/launched
`anetd` process with temporary PEM/config files. See design 19 and
`test/evidence/2026-10-27/rpc/` for the retained run and scope.
The checkpoint has 14 RPC tests within 64 total database tests and 29 runner
checks. On this Windows run, process-local `GOFLAGS=-p=1` avoided temporary test
executable contention; the individual concurrency/OS-kill tests remain enabled.

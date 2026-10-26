# Authenticated TCS / ICS service

`cmd/anetd` serves the implemented TCS and ICS methods over TLS 1.3 with mandatory
client certificates. The leaf must have a lifetime of at most 24 hours and exactly
one URI SAN: `spiffe://<trust_domain>/agent-instance/<instance_id>`. Every RPC checks
the registered certificate fingerprint, exact URI, live principal/agent/instance,
and any required role in PostgreSQL. CN, metadata and request body identity are
not authentication inputs. Existing TLS channels do not bypass revocation checks.

Apply migrations 001–014 and provision the domain, identities, certificate
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
| GW CreateSubtask (optional) | Unsigned strict child intent; shared command namespace, original signature/expiry recovery, independent TCS delegation guards |
| InstanceCredentials (optional) | Managed-instance status, two-phase renewal/activation and confirmed abandonment; pending keys can only activate |
| ICS IssueRootPermit | Signed GW command; exact consent subset and per-task ceilings; short-lived signed permit |
| TCS SubmitCommand | Only signed command + signed root permit; root grant, task, accounts, receipt and notification commit in T1 |
| TCS CreateSubtask | Only signed `anet.subtask/1` command (request field 9); parent lease/epoch/version, immutable recipient policy, rights and four-dimensional H→D/child B transfer |
| TCS GetTask | Task owner, assigned Agent or direct creating Agent; consistent task/effects/receipts/four-dimension budget snapshot |
| TCS AcquireLease / RenewLease | Worker identity comes from mTLS; optional body instance must match |
| TCS ReserveBudget / ReleaseReservation | Unbound reservations only; no caller-supplied effect/attempt binding or soft TTL |
| ICS Decide | Authoritative full grant/consent chain, distinct subject/actor, configured action/resource/class, signed decision persisted |
| TCS AdmitEffect | Signed ICS decision and proposal bindings; unsigned legacy authorization rejected |
| TCS CancelTask | Durable cancellation, repeated cancellation returns original version; outboxd propagates it through local descendants |
| TCS CloseSubtaskBudget | Child ID only; authoritative one-time cost rollup/unused return and immutable replay result |
| TCS ReportEffectResult | Live adapter role + signed adapter fact; late settlement remains possible after cancellation |
| ICS Revoke | Task owner or delegated grant actor via `grant_id`; repeated revocation returns original effective time |

Unknown protobuf fields and unsupported legacy request fields are rejected. ANET
codes use gRPC `ErrorInfo`; database internals are not returned. Other declared
RPCs return `Unimplemented`, including registration, event streaming
and token exchange. DIR bindings are generated but no DIR server is registered.

For child creation, the trusted command issuer signs `tcs.SubtaskSubmission` and
its `tcs.SubtaskCommand` (see `services/tcs/subtasks.go`). The authenticated caller
must match that envelope. Root consent and contract explicitly permit delegation;
`recipient_policies` is trusted operator provisioning, never request-body policy.
Reserve each positive dimension with purpose `subtask`, then bind its reservation
and exact amount in the command; zero dimensions use an empty reservation ID.
Only the same domain is supported. The GW child signing path is now available
through `Gateway.CreateSubtask` (design 30). Policy management UI, the complete
Python SDK and remote delegation remain pending. Apply migration 014 for local
closeout and tree cancellation. `CloseSubtaskBudget` accepts no reported cost or
return amount; a pending liability returns `ANET-BUD-SUBTASK-NOT-READY`. See design 29.

`anetd` runs TCS/ICS and optionally the SDK Gateway (design 22). Outbox delivery and the execution engine remain separate
components; starting this daemon alone does not execute external business effects.
`services/identity` now supplies OIDC browser login and user consent (design 20).
Owner-controlled certificate enrollment/rotation/revocation is now provided by
design 21; fingerprints and certificate expiry are rechecked inside GW/ICS/TCS
after lock waits. Design 22 adds durable command signing and intercepted RPC
failure audit, distinguishing definite denials from unknown outcomes. See
[`services/gateway`](../gateway/README.md) for configuration and coverage limits.
Design 23 adds unattended renewal and the local `instancerenew` client; workloads
must reload the resulting bundle and reconnect. DPoP, CA rotation, general
revocation/push, production target transport and ART classification remain unfinished.

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

## Local transactions

`ExecuteLocalEffect` accepts a signed admission plus canonicalizable JSON parameters.
Configure `capability: local-tx` and `local_handler: record.put/v1`; action must be
`record.put` and resource a configured `records:` namespace. The four dimensions
are required. Unknown handlers and handlers attached to external capabilities
fail startup. See [local handler](../../adapters/local/README.md) and design 24.

Contract admission (design 31): apply migration 015 before the updated service.
New admissions use internal/contract for typed acceptance rules and offline
JSON Schema 2020-12 compilation. External schema resources are rejected; embed
required resources in $defs. Contracts and existing task contract references
are immutable. Exact accepted-command replay precedes the stricter profile.
This does not implement ART, evidence verification or successful task completion.

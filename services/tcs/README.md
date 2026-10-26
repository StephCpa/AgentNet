# Single-domain TCS transaction kernel

Implementation: `commands.go`, `effects.go`, `lifecycle.go`; migration: `003_tcs_kernel.sql`.
This package runs real PostgreSQL transactions using pgx. It is a library, not an
unauthenticated HTTP server. The gRPC transport in `services/rpc` authenticates
Caller and maps signed envelopes without overriding fields from unsigned JSON.

## Implemented operations

| Operation | Durable behavior |
|---|---|
| Submit | Verify GW command signature, serialize command ID, check expiry after waits, atomically create contract/task/four accounts/inbox/receipt/event/outbox; retries return original task and receipt |
| GetTask | Authorized status/version/epoch snapshot |
| AcquireLease / RenewLease | Bind authenticated instance; preserve epoch on renewal; reject expired renewal; takeover increments epoch after expiry + grace |
| Reserve | Task and account locks; S+H+D admission; exact NUMERIC amounts, no floating-point money |
| ReleaseUnbound | Idempotently release an unused hold; bound holds cannot use this path |
| Admit | Existing result before freshness/version checks; signed ICS decision and persisted root grant; actor/lease/epoch/version checks; server-selected required dimensions; atomic four-dimensional binding plus event/outbox |
| Cancel | Idempotent terminal cancellation and durable outbox intent; no new admissions |
| Resolve | Signed adapter/reconciler evidence; UNKNOWN retains holds; terminal evidence settles atomically even after cancel/takeover; exact duplicates do not settle twice |

An untrusted self-report of zero settles the full hold. An overrun is preserved in
audit with its actual usage and evidence reference; holds remain and the task is
frozen for new spending. It requires a future exception-accounting workflow; the
kernel does not truncate real spend to manufacture a budget guarantee.

Root tasks are owned by the authenticated principal. The identity must be a live
registered instance of an active agent owned by that principal in this domain.
Persisted grants must be root grants with matching subject/actor/task/action/resource,
live temporal bounds and no revocation. Delegated grants are explicitly rejected
until chain checking and delegated-task ownership are implemented.

## Signatures and trust boundary

`Config` requires distinct logical key registries for CommandKeys (GW), DecisionKeys
(ICS) and ResultKeys (adapter/reconciler); no allow-all verifier is provided. Callers
must provision public keys from trusted configuration. Tests generate isolated keys;
they do not establish an OIDC/mTLS deployment or production key lifecycle.

`Envelope` contains KeyID, Payload bytes and an Ed25519 Signature. Verification
canonicalizes Payload with RFC 8785 plus the AgentNet numeric profile, rejects
duplicate keys and invalid Unicode, verifies the canonical bytes, and decodes the
versioned typed payload with unknown fields rejected. Structures in `types.go`
are the canonical payload schema for this internal API:

- Submission: kind=`anet.command/1`, caller and the complete command. Caller uses
  the Go JSON field names `Principal`, `Agent`, `Instance`. Command includes
  command_id/audience/not_after/contract_id/contract_version/contract/limits.
  The signed caller must equal the transport-authenticated caller. Re-signing
  an expired command with a changed not_after is not a replay of its old signature.
- Decision: kind=`anet.authorization/1`, decision_id/allow/at/expires_at plus
  subject, actor_agent, actor_instance, grant_id, task_id, target, action, resource,
  params_digest, authority_digest, audience and sensitivity.
- Result: kind=`anet.result/1`, task_id/effect_id/params_digest/audience/outcome,
  usage, usage_source and evidence. The result key issuer attests the business
  outcome; a worker's unsigned statement cannot release an uncertain hold.

The network protobufs now bind to this envelope API through `services/rpc`.
The server verifies complete signed content and rejects the old unsigned
AuthorizationDecision.allow field as proof.

The transport also supplies `Caller.CredentialHash` from the verified certificate.
Transaction identity checks match the current fingerprint again after lock waits,
so certificate rotation fences requests already authenticated with the old key.
This field is excluded from JSON; signed commands retain the stable three-part
principal/agent/instance identity. Trusted library fixtures may omit the fingerprint;
the mTLS transport always supplies it. See design 21 for credential lifecycle scope.

## Transactions and setup

Apply migrations 001–008 in order to a new database; 002 also upgrades a d6b219c schema.
003 leaves legacy task ownership NULL, so the new kernel denies access until an
operator performs an evidence-based ownership backfill. Do not guess owners.

T1 uses a namespaced transaction advisory lock on the logical command identity.
Other paths lock task, then root grant (shared, T2 only), accounts sorted by ID,
reservations sorted by ID, then effects. No external call runs inside the transaction.
The owner and caller are checked again after relevant lock waits, alongside current
database time. All connection/commit failures are returned to the caller; retries
must reuse the original identifiers and signed command/evidence.

Defaults: lease TTL 30s, takeover grace 15s, decision max age 60s. Account amounts
are canonical decimal strings with at most 14 integer digits and 6 fractional digits.
The operation registry is copied at construction and controls target/action/resource,
capability, purpose and required dimensions; the proposal cannot omit a budget dimension.

Provision domains/principals/agents/instances and standing consent through the
trusted identity control plane before using the network service. Its signed root
permit creates the grant in T1. Legacy library fixtures seed trusted root grants
explicitly; an unsigned command cannot create a grant on its own.

## Validation

```text
go test ./...
go vet ./...
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/kernel-test --kernel-tests
```

Without ANET_TEST_DSN, Go skips PostgreSQL integration tests. The Python entry point
creates a new loopback cluster, applies migrations and invokes Go with that DSN;
any skipped integration test fails the gate. It always stops the cluster afterwards.

2026-10-27: **22 top-level kernel integration tests plus 10 decision-binding subcases**
passed on PostgreSQL 16.15. Two canonicalization unit tests also passed. Evidence:
`test/evidence/2026-10-27/kernel/`. Runtime: Go 1.25.1; module minimum declares 1.24.

Tests cover concurrent command/Reserve races, reopening a fresh pool after an ignored
ACK, transaction rollback after a database write failure, command/lease expiry during
observed lock waits, revocation during a lock wait, fencing, immutable bindings,
UNKNOWN preservation, cancellation races, late signed results and conservative charging.
Reopening a pool is not a process kill; signed test receipts are not live external
payment evidence. Full FI, outbox dispatch/recovery, ICS/GW transport authentication,
delegation/tranche/attempt billing, VERIFIED completion and protocol interop remain.

Follow-up: `services/delivery` and design 17 now implement local outbox/inbox
delivery and actual process-kill tests. The numbers and limits above describe the
initial kernel checkpoint; remote transport and external business recovery remain.

Execution increment (design 18): apply 005 as well. T2 records send-time grant/actor
provenance and seeds a durable execution job. Idempotent-key operations require a
positive TargetTTL; local-tx is rejected until an atomic business handler exists.
Parameter storage and execution/reconciliation are in services/execution.

Authenticated service increment (design 19): apply 006 as well. Network submission
requires an ICS root permit derived from trusted standing consent. T1 independently
checks its revision, scope and per-task ceilings, then creates the root grant in
the same transaction as the task and acceptance receipt. T2 and send-time gates
also check consent revision and data class. Legacy library fixtures with manually
provisioned grants remain supported; network clients cannot create those roots.

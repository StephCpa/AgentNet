# Durable effect execution and reconciliation

Apply migrations 001–005. `Engine.Run` consumes durable `EFFECT_ADMITTED`
notifications and resumes execution/reconciliation jobs. Each engine is scoped to
one domain, with a trusted target registry, result-signing key and TCS resolver.
It is a Go service component; the authenticated network service is not yet complete.

## Parameters and admission

Write parameters with `Files.Put` on a trusted, access-controlled local volume.
Use the returned JCS digest in the proposal and ICS decision. `Files.Load` verifies
that digest before sending; digest-shaped filenames prevent caller-selected paths.
Publication is atomic and cannot replace an existing digest binding. Parameters
are limited to 1 MiB. The ledger/outbox retain only the digest, not the parameter
document. Production ART encryption, ACLs, retention and power-loss durability of
the parameter volume remain deployment work; do not use an attacker-writable root.

T2 now persists grant/actor/action/resource provenance and creates the execution
job in the admission transaction. Idempotent-key operation configuration must include
the target's guaranteed `TargetTTL`. Legacy effects have no send provenance and
are escalated by migration 005, never treated as unsent. `local-tx` configuration
is rejected until a genuinely atomic business-write/budget implementation exists.

## Send and recovery rules

- Before I/O, lock the task, recheck epoch/cancel/deadline/freeze plus live root
  grant rights, principal and instance, then persist a new attempt ID and increment
  the send count. External I/O runs after commit. A crash after that marker is an
  uncertain send even if no network write actually occurred.
- `queryable`: query first. ABSENT permits a first send only with no previous send
  marker and an ADMITTED effect. After an uncertain send, ABSENT, PENDING and UNKNOWN
  never trigger another send. COMMITTED settles; a NOT_EXECUTED_FINAL proof must bind
  the exact attempt and make delayed arrival of that attempt impossible. After multiple
  keyed attempts, only an effect-wide final proof that fences every request for that
  business key permits release; a last-attempt proof is insufficient. It closes
  this effect as REJECTED. A later business retry needs a new proposal/admission.
- `idempotent-key`: send the stable effect ID as the business key. After uncertainty,
  query when available; absent/unknown or an unavailable query API permits a same-key
  retry within the original target TTL and recovery window. The TTL starts at the
  first durable send intent and is never reset. PENDING waits. Every retry rechecks
  send authority; target key-retention guarantees are a trusted adapter contract.
- `opaque`: one send intent only. If its outcome is lost, retain UNKNOWN/HELD and
  escalate to manual recovery. Never query or automatically resend an opaque target.
- Failed lookup before any send keeps ADMITTED. An externally reported UNKNOWN
  without send provenance does not authorize a fresh send.

Terminal observations bind effect/parameter digest, evidence and all usage dimensions.
Malformed cost evidence cannot poison the saved receipt. The adapter signs and saves
the first valid terminal result before reporting it, so a reporting outage replays
the same signed result without executing the business operation again. TCS remains
responsible for settlement, result conflicts and conservative overrun handling.
Trusted late facts can settle after cancellation; they do not restart the task.

Every completed query is journaled with its five-way status. Recovery delays default
to 1 minute, 5 minutes, 15 minutes, 1 hour, then 4 hours; the default window is 72 hours.
Window exhaustion or an unrecoverable binding/authority problem writes an escalation
event and outbox notification, retains holds, and moves nonterminal tasks to WAITING.
Actual query results and retry scheduling have separate counters.

## Target trust and limits

Target bindings are deployment configuration, never caller-supplied URLs. Preserve
provider identity, capability and key-retention guarantees across restarts/failover;
changing an alias to a different business system is not recovery. Production target
adapters must authenticate the endpoint and validate business evidence. The bundled
`adapters/mockbiz` client is only a loopback faultlab transport, not that trust layer.

The send gate cannot revoke an already in-flight call. Full GW outbound credentials,
remote fencing, per-attempt/query-cost accounting, delegated authority, local-tx,
PRODUCED receipt issuance and VERIFIED task completion remain separate work.

## Test entry point

```text
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/execution-new --kernel-tests
```

The mock business server uses HTTP and independent PostgreSQL transactions. Queryable
and opaque Send deliberately do not deduplicate, so an accidental retry produces two
visible business entries. FI-03 tests wait for a committed business entry, verify the
adapter has no saved result, and kill its OS process. Queryable/idempotent-key recover
one COMMITTED effect; opaque retains UNKNOWN and all holds with one external effect.
Other tests cover late completion after ABSENT, PENDING, unavailable query APIs, key
TTL, final-proof tombstones, send fencing, receipt replay and payload integrity.

Evidence: `test/evidence/2026-10-27/execution/`. This is mock-target FI-03 evidence;
it does not establish production payment semantics or the complete M1 exit gate.
